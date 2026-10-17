// Package agentstep implements the agent_message step handler: it renders the
// step's text and conversation templates against the run, posts the text
// into that conversation of the named agent and starts the agent's turn,
// through the same in-process compose the inbox uses. The result
// {conversationId, activityId, runId} is saved to step_outputs for
// downstream steps (danmestas/notify#4).
package agentstep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
)

// Composer posts a message to an agent conversation; *ingress.Service is one.
type Composer interface {
	Compose(ctx context.Context, scope domain.Scope, in ingress.ComposeInput) (ingress.ComposeResult, error)
}

// DraftRequester records that the reply to a composed message must become a
// draft; *agentstore.Store's Drafts() is one.
type DraftRequester interface {
	Request(ctx context.Context, scope domain.Scope, clientMessageID string, d agentdomain.Draft) error
}

// Handler executes agent_message steps.
type Handler struct {
	store    *store.Store
	composer Composer
	drafts   DraftRequester
}

// New returns a Handler reading runs from s, composing through c, and
// recording review-mode draft requests in d.
func New(s *store.Store, c Composer, d DraftRequester) *Handler {
	return &Handler{store: s, composer: c, drafts: d}
}

// output is the step's result in step_outputs.
type output struct {
	ConversationID string `json:"conversationId"`
	ActivityID     string `json:"activityId"`
	RunID          string `json:"runId"`
}

// Handle is registered via engine.Handle("agent_message", h.Handle).
//
// The compose is keyed on the run's transaction, recipient and step, so a
// retried step or a redelivered trigger returns the message already posted
// instead of posting it again.
//
// A failed compose (unknown or inactive agent, a runtime this server cannot
// run, an empty rendered text) follows the step's failurePolicy: fail_closed
// fails the run with no output; continue saves an empty output and proceeds.
func (h *Handler) Handle(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	env, _, stepOutputs, err := delivery.RebuildEnvelope(ctx, h.store, t)
	if err != nil {
		return nil, engine.NewNonRetryableError(fmt.Errorf("agent_message: load run context: %w", err))
	}
	scope := domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}

	cfg, err := loadConfig(ctx, h.store, scope, env.WorkflowID, t.StepID)
	if err != nil {
		return nil, engine.NewNonRetryableError(fmt.Errorf("agent_message: load step config: %w", err))
	}

	data := map[string]any{
		"subscriber":    decodeObject(env.Subscriber),
		"payload":       decodeObject(env.Payload),
		"transactionId": env.TransactionID,
		"steps":         stepOutputs,
	}
	noTranslate := func(key string) string { return key }
	text := template.RenderAs(template.Plain, cfg.Text, data, noTranslate)
	conversation := strings.TrimSpace(template.RenderAs(template.Plain, cfg.Conversation, data, noTranslate))

	key := composeKey(env.TransactionID, env.SubscriberID, t.StepID)
	if cfg.ReviewDraft {
		// Recorded before composing, so the turn always finds it: an agent
		// can answer before Compose returns.
		if err := h.requestDraft(ctx, scope, key, env.SubscriberID, env.Payload); err != nil && cfg.FailurePolicy != "continue" {
			return nil, engine.NewNonRetryableError(fmt.Errorf("agent_message: %w", err))
		}
	}
	res, err := h.composer.Compose(ctx, scope, ingress.ComposeInput{
		AgentIdentifier: cfg.Agent,
		ConversationID:  conversation,
		SubscriberID:    env.SubscriberID,
		Text:            text,
		IdempotencyKey:  key,
		RequireActive:   true,
	})
	if err != nil {
		if cfg.FailurePolicy != "continue" {
			return nil, engine.NewNonRetryableError(fmt.Errorf("agent_message: %w", err))
		}
		res = ingress.ComposeResult{}
	}

	out, err := json.Marshal(output{
		ConversationID: res.ConversationID,
		ActivityID:     res.ActivityID,
		RunID:          res.RunID,
	})
	if err != nil {
		return nil, engine.NewNonRetryableError(fmt.Errorf("agent_message: marshal output: %w", err))
	}
	if err := h.store.StepOutputs().Save(ctx, scope, t.RunID, t.StepID, out); err != nil {
		return nil, engine.NewNonRetryableError(fmt.Errorf("agent_message: save output: %w", err))
	}
	return out, nil
}

// mailEvent is the inbound-mail trigger payload a review-mode step reads:
// the mailbox the message arrived in, the mailbox's own id for it, and its
// threading headers.
type mailEvent struct {
	Mailbox      string   `json:"mailbox"`
	MessageID    string   `json:"messageId"`
	RFCMessageID string   `json:"rfcMessageId"`
	References   []string `json:"references"`
	Subject      string   `json:"subject"`
	From         string   `json:"from"`
}

// requestDraft records the draft the agent's reply becomes. Recipients,
// subject and threading are advisory: the mailbox owner recomputes them.
func (h *Handler) requestDraft(ctx context.Context, scope domain.Scope, key, subscriberID string, payload json.RawMessage) error {
	if h.drafts == nil {
		return errors.New("review drafts are not available on this server")
	}
	var ev mailEvent
	if err := json.Unmarshal(payload, &ev); err != nil || ev.Mailbox == "" || ev.MessageID == "" {
		return errors.New("reviewDraft needs an inbound-mail payload with mailbox and messageId")
	}
	refs := ev.References
	if ev.RFCMessageID != "" && (len(refs) == 0 || refs[len(refs)-1] != ev.RFCMessageID) {
		refs = append(refs, ev.RFCMessageID)
	}
	var to []string
	if ev.From != "" {
		to = []string{ev.From}
	}
	return h.drafts.Request(ctx, scope, key, agentdomain.Draft{
		SubscriberID:    subscriberID,
		Mailbox:         ev.Mailbox,
		SourceMessageID: ev.MessageID,
		InReplyTo:       ev.RFCMessageID,
		References:      refs,
		Subject:         replySubject(ev.Subject),
		To:              to,
	})
}

// replySubject prefixes "Re: " unless the subject already has one.
func replySubject(s string) string {
	if t := strings.TrimLeft(s, " \t"); len(t) >= 3 && strings.EqualFold(t[:3], "re:") {
		return s
	}
	if s == "" {
		return ""
	}
	return "Re: " + s
}

// composeKey derives the compose idempotency key from the run's transaction,
// recipient and step: hex SHA-256, which fits the 64-character key format. The
// step id keeps two agent_message steps in one workflow apart.
func composeKey(transactionID, subscriberID, stepID string) string {
	sum := sha256.Sum256([]byte(transactionID + "\x00" + subscriberID + "\x00" + stepID))
	return hex.EncodeToString(sum[:])
}

// loadConfig reads the step's control values from its workflow.
func loadConfig(ctx context.Context, s *store.Store, scope domain.Scope, workflowID, stepID string) (steptype.AgentMessageCfg, error) {
	wf, err := s.Workflows().GetByID(ctx, scope, workflowID)
	if err != nil {
		return steptype.AgentMessageCfg{}, fmt.Errorf("workflow %q: %w", workflowID, err)
	}
	var steps []domain.Step
	if len(wf.Steps) > 0 {
		if err := json.Unmarshal(wf.Steps, &steps); err != nil {
			return steptype.AgentMessageCfg{}, fmt.Errorf("unmarshal steps: %w", err)
		}
	}
	for _, step := range steps {
		if step.StepID == stepID {
			return steptype.AgentMessageConfig(step.ControlValues)
		}
	}
	return steptype.AgentMessageCfg{}, fmt.Errorf("step %q not found in workflow %q", stepID, workflowID)
}

// decodeObject unmarshals a JSON object for template data; anything else is
// an empty map.
func decodeObject(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}
