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
	aud *audience.Service
	s   *store.Store
	eng *engine.Engine
}

// New builds a trigger Service over its collaborators.
func New(aud *audience.Service, s *store.Store, eng *engine.Engine) *Service {
	return &Service{aud: aud, s: s, eng: eng}
}

// ToField is the trigger recipient (a single subscriber). It mirrors the
// subset of the reference platform's `to` object notify supports for in-app.
type ToField struct {
	SubscriberID string `json:"subscriberId"`
	Email        string `json:"email"`
	FirstName    string `json:"firstName"`
	LastName     string `json:"lastName"`
	Phone        string `json:"phone"`
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
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Ingest processes one trigger end-to-end. On an unknown workflow it returns a
// non-error response with Acknowledged=false / Status="error" (documented
// choice: trigger ingestion is best-effort acknowledgement, mirroring the reference
// response DTO carrying an error array rather than a transport error).
func (svc *Service) Ingest(ctx context.Context, scope domain.Scope, req TriggerRequest) (TriggerResponse, error) {
	sub, err := svc.aud.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: req.To.SubscriberID,
		Email:        req.To.Email,
		FirstName:    req.To.FirstName,
		LastName:     req.To.LastName,
		Phone:        req.To.Phone,
	})
	if err != nil {
		return TriggerResponse{}, err
	}

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
	step := steps[0]
	var controls stepControls
	if len(step.ControlValues) > 0 {
		if err := json.Unmarshal(step.ControlValues, &controls); err != nil {
			return TriggerResponse{}, err
		}
	}

	transactionID := req.TransactionID
	if transactionID == "" {
		transactionID = store.NewULID()
	}
	notificationID := store.NewULID()

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
		return TriggerResponse{}, err
	}

	envelope := runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: scope.OrgID, EnvID: scope.EnvID},
		TransactionID:  transactionID,
		WorkflowID:     wf.ID,
		NotificationID: notificationID,
		SubscriberID:   req.To.SubscriberID,
		Subscriber:     subscriberJSON,
		Payload:        req.Payload,
		Step: runenv.Step{
			StepID:          step.StepID,
			Channel:         step.Type,
			ProviderID:      "",
			SubjectTemplate: controls.Subject,
			BodyTemplate:    controls.Body,
		},
	}
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return TriggerResponse{}, err
	}

	// DAGName = scope.EnvID + "-" + triggerIdentifier. dagnats maps the
	// workflow name onto a NATS KV key, which rejects ":" ("nats: invalid
	// key"). "-" is used rather than "." so the name stays a single NATS
	// subject token; it is proven end-to-end by the engine smoke workflow.
	// authoring registers under the identical name (authoring.DAGName).
	dagName := scope.EnvID + "-" + req.Name
	runID, err := svc.eng.StartRun(ctx, dagName, envelopeJSON)
	if err != nil {
		return TriggerResponse{}, err
	}

	if _, err := svc.s.Notifications().Create(ctx, scope, domain.Notification{
		ID:            notificationID,
		OrgID:         scope.OrgID,
		EnvID:         scope.EnvID,
		WorkflowID:    wf.ID,
		SubscriberID:  req.To.SubscriberID,
		TransactionID: transactionID,
		RunID:         runID,
		Payload:       req.Payload,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return TriggerResponse{}, err
	}

	return TriggerResponse{
		Acknowledged:  true,
		Status:        "processed",
		TransactionID: transactionID,
	}, nil
}
