// Package trigger implements POST /v1/events/trigger: it upserts the recipient,
// resolves the named workflow's first step, builds the runenv.InAppEnvelope
// (the StartRun input the delivery worker consumes), kicks off an engine run,
// and records the notification. It is the producer side of the wire contracts
// pinned in internal/runenv.
package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// Service orchestrates a trigger: audience upsert -> workflow lookup ->
// envelope build -> engine StartRun -> notification persist.
type Service struct {
	aud      *audience.Service
	s        *store.Store
	eng      *engine.Engine
	digester Digester
}

// Digester batches rapid trigger events for the same (env, subscriber,
// workflow) into one downstream run. eventbus.Bus satisfies this structurally.
// A nil digester disables batching: a digest-first workflow then sends per
// event instead of merging.
type Digester interface {
	DigestAppend(key string, event json.RawMessage) error
	DigestClaim(key string) (bool, error)
}

// New builds a trigger Service over its collaborators.
func New(aud *audience.Service, s *store.Store, eng *engine.Engine) *Service {
	return &Service{aud: aud, s: s, eng: eng}
}

// WithDigester attaches an optional digester and returns the Service for
// chaining. When set, a workflow whose first step is "digest" merges rapid
// events into one run; a nil digester leaves trigger behaviour unchanged.
func (svc *Service) WithDigester(d Digester) *Service {
	svc.digester = d
	return svc
}

// ToField is the trigger recipient. A single-subscriber trigger sets
// SubscriberID (plus optional profile fields); a multicast trigger sets
// TopicKey instead, fanning out to every member of that topic.
type ToField struct {
	SubscriberID string `json:"subscriberId"`
	Email        string `json:"email"`
	FirstName    string `json:"firstName"`
	LastName     string `json:"lastName"`
	Phone        string `json:"phone"`
	TopicKey     string `json:"topicKey,omitempty"`
}

// TriggerRequest is the POST /v1/events/trigger body.
type TriggerRequest struct {
	Name          string          `json:"name"`
	To            ToField         `json:"to"`
	Payload       json.RawMessage `json:"payload"`
	TransactionID string          `json:"transactionId"`
}

// TriggerResponse mirrors the reference trigger-event-response DTO (parity Oracle 1):
// Error and activityFeedLink are omitted on success; TransactionID echoes the
// client value or the server-minted id.
type TriggerResponse struct {
	Acknowledged  bool     `json:"acknowledged"`
	Status        string   `json:"status"`
	TransactionID string   `json:"transactionId"`
	Error         []string `json:"error,omitempty"`
}

// wireSubscriber is the handlebars context object for {{subscriber.*}} — the reference platform
// namespace keys, NOT Go field names, so templates resolve. It is marshaled
// into the envelope's Subscriber raw JSON.
type wireSubscriber struct {
	SubscriberID string          `json:"subscriberId"`
	FirstName    string          `json:"firstName"`
	LastName     string          `json:"lastName"`
	Email        string          `json:"email"`
	Phone        string          `json:"phone"`
	Locale       string          `json:"locale"`
	Timezone     string          `json:"timezone"`
	Data         json.RawMessage `json:"data,omitempty"`
}

// stepControls is the {subject, body} ControlValues shape notify reads from a
// workflow step, regardless of channel.
type stepControls struct {
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	Throttle struct {
		Limit     int `json:"limit"`
		WindowSec int `json:"windowSec"`
	} `json:"throttle"`
}

// Ingest processes one trigger end-to-end. On an unknown workflow it returns a
// non-error response with Acknowledged=false / Status="error" (documented
// choice: trigger ingestion is best-effort acknowledgement, mirroring the reference
// response DTO carrying an error array rather than a transport error).
func (svc *Service) Ingest(ctx context.Context, scope domain.Scope, req TriggerRequest) (TriggerResponse, error) {
	wf, err := svc.s.Workflows().GetByTriggerIdentifier(ctx, scope, req.Name)
	if errors.Is(err, store.ErrNotFound) {
		return TriggerResponse{
			Acknowledged: false,
			Status:       "error",
			Error:        []string{"workflow not found"},
		}, nil
	}
	if err != nil {
		return TriggerResponse{}, err
	}

	var steps []domain.Step
	if len(wf.Steps) > 0 {
		if err := json.Unmarshal(wf.Steps, &steps); err != nil {
			return TriggerResponse{}, err
		}
	}
	if len(steps) == 0 {
		return TriggerResponse{
			Acknowledged: false,
			Status:       "error",
			Error:        []string{"workflow has no steps"},
		}, nil
	}

	// transactionID is minted once and shared by every fan-out run so a
	// multicast emits a single logical transaction across all recipients.
	transactionID := req.TransactionID
	if transactionID == "" {
		transactionID = store.NewULID()
	}
	req.TransactionID = transactionID

	// Multicast: fan out to every member of the topic. The subscriber profiles
	// already exist (added to the topic out of band), so no per-recipient
	// upsert happens here; each member's persisted profile is resolved inside
	// ingestForSubscriber for template rendering.
	if req.To.TopicKey != "" {
		if _, err := svc.s.Topics().GetByKey(ctx, scope, req.To.TopicKey); errors.Is(err, store.ErrNotFound) {
			return TriggerResponse{
				Acknowledged: false,
				Status:       "error",
				Error:        []string{"topic not found"},
			}, nil
		} else if err != nil {
			return TriggerResponse{}, err
		}
		subs, err := svc.s.Topics().ListSubscriberIDs(ctx, scope, req.To.TopicKey)
		if err != nil {
			return TriggerResponse{}, err
		}
		for _, subscriberID := range subs {
			if _, err := svc.ingestForSubscriber(ctx, scope, wf, subscriberID, req); err != nil {
				return TriggerResponse{}, err
			}
		}
		return TriggerResponse{
			Acknowledged:  true,
			Status:        "processed",
			TransactionID: transactionID,
		}, nil
	}

	// Single-subscriber: upsert the to-field profile, then run for that one
	// recipient. Behaviour here is unchanged from the original Ingest.
	if _, err := svc.aud.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: req.To.SubscriberID,
		Email:        req.To.Email,
		FirstName:    req.To.FirstName,
		LastName:     req.To.LastName,
		Phone:        req.To.Phone,
	}); err != nil {
		return TriggerResponse{}, err
	}
	if _, err := svc.ingestForSubscriber(ctx, scope, wf, req.To.SubscriberID, req); err != nil {
		return TriggerResponse{}, err
	}
	return TriggerResponse{
		Acknowledged:  true,
		Status:        "processed",
		TransactionID: transactionID,
	}, nil
}

// ingestForSubscriber performs the per-recipient work: resolve the persisted
// subscriber profile, build the envelope from the workflow's first step, honour
// digest coordination, start the engine run, and persist the notification. It
// assumes req.TransactionID is already minted and wf has at least one step.
// A digest-merged event returns an empty runID with no error (no run started).
func (svc *Service) ingestForSubscriber(ctx context.Context, scope domain.Scope, wf domain.Workflow, subscriberID string, req TriggerRequest) (string, error) {
	var steps []domain.Step
	if err := json.Unmarshal(wf.Steps, &steps); err != nil {
		return "", err
	}
	step := steps[0]
	var controls stepControls
	if len(step.ControlValues) > 0 {
		if err := json.Unmarshal(step.ControlValues, &controls); err != nil {
			return "", err
		}
	}

	// Digest coordination: when the workflow's first step is "digest" and a
	// digester is attached, append this event to the (env, subscriber,
	// workflow) window and claim ownership. Exactly one trigger wins the claim
	// and starts the run; the rest merge silently (no new run) and are drained
	// + rendered by the owning run after the Sleep window fires.
	var digestKey string
	if step.Type == "digest" && svc.digester != nil {
		digestKey = scope.EnvID + ":" + subscriberID + ":" + wf.ID
		if err := svc.digester.DigestAppend(digestKey, req.Payload); err != nil {
			return "", err
		}
		isOwner, _ := svc.digester.DigestClaim(digestKey)
		if !isOwner {
			return "", nil
		}
	}
	notificationID := store.NewULID()

	sub, err := svc.aud.Get(ctx, scope, subscriberID)
	if err != nil {
		return "", err
	}
	subscriberJSON, err := json.Marshal(wireSubscriber{
		SubscriberID: sub.SubscriberID,
		FirstName:    sub.FirstName,
		LastName:     sub.LastName,
		Email:        sub.Email,
		Phone:        sub.Phone,
		Locale:       sub.Locale,
		Timezone:     sub.Timezone,
		Data:         sub.Data,
	})
	if err != nil {
		return "", err
	}

	envelope := runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: scope.OrgID, EnvID: scope.EnvID},
		TransactionID:  req.TransactionID,
		WorkflowID:     wf.ID,
		NotificationID: notificationID,
		SubscriberID:   subscriberID,
		Subscriber:     subscriberJSON,
		Payload:        req.Payload,
		Step: runenv.Step{
			StepID:            step.StepID,
			Channel:           step.Type,
			ProviderID:        "",
			SubjectTemplate:   controls.Subject,
			BodyTemplate:      controls.Body,
			ThrottleLimit:     controls.Throttle.Limit,
			ThrottleWindowSec: controls.Throttle.WindowSec,
		},
	}
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}

	// DAGName = scope.EnvID + "-" + triggerIdentifier. dagnats maps the
	// workflow name onto a NATS KV key, which rejects ":" ("nats: invalid
	// key"). "-" is used rather than "." so the name stays a single NATS
	// subject token; it is proven end-to-end by the engine smoke workflow.
	// authoring registers under the identical name (authoring.DAGName).
	dagName := scope.EnvID + "-" + req.Name
	runID, err := svc.eng.StartRun(ctx, dagName, envelopeJSON)
	if err != nil {
		return "", err
	}

	if _, err := svc.s.Notifications().Create(ctx, scope, domain.Notification{
		ID:            notificationID,
		OrgID:         scope.OrgID,
		EnvID:         scope.EnvID,
		WorkflowID:    wf.ID,
		SubscriberID:  subscriberID,
		TransactionID: req.TransactionID,
		RunID:         runID,
		Payload:       req.Payload,
		DigestKey:     digestKey,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return "", err
	}
	return runID, nil
}
