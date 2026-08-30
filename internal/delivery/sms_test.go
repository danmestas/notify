package delivery

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
)

// stubSMSResolver is a fixed-answer SMSResolver standing in for
// connectivity.Service in the unit tests.
type stubSMSResolver struct {
	p             providers.SMSProvider
	id            string // providerID
	integrationID string // defaults to "integ-sms-test" when empty
	err           error

	// integrations routes explicit "integration" identifiers to a distinct
	// provider/providerID/integrationID, standing in for
	// connectivity.Service.ResolveIntegration. See stubIntegrationRoute
	// (defined in chat_test.go, shared across this package's tests).
	integrations map[string]stubIntegrationRoute
}

func (s stubSMSResolver) ResolveSMS(_ context.Context, _ domain.Scope) (providers.SMSProvider, string, string, error) {
	iid := s.integrationID
	if iid == "" {
		iid = "integ-sms-test"
	}
	return s.p, s.id, iid, s.err
}

func (s stubSMSResolver) ResolveIntegration(ctx context.Context, scope domain.Scope, channel, identifier string) (any, string, string, error) {
	if identifier == "" {
		return s.ResolveSMS(ctx, scope)
	}
	route, ok := s.integrations[identifier]
	if !ok {
		return nil, "", "", errUnknownIntegration(channel, identifier)
	}
	return route.provider, route.providerID, route.integrationID, nil
}

// smsEnvelope mirrors chatEnvelope for the sms channel. The subscriber JSON
// carries a phone number, which is the SMS recipient.
func smsEnvelope() runenv.InAppEnvelope {
	return runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		TransactionID:  "txn_sms",
		WorkflowID:     "wf_1",
		NotificationID: "ntf_1",
		SubscriberID:   "user_123",
		Subscriber:     json.RawMessage(`{"subscriberId":"user_123","firstName":"Ada","phone":"+15557654321"}`),
		Payload:        json.RawMessage(`{"itemCount":3}`),
		Step: runenv.Step{
			StepID:          "S1",
			Channel:         "sms",
			SubjectTemplate: "Hi {{subscriber.firstName}}",
			BodyTemplate:    "You have {{payload.itemCount}} items",
		},
	}
}

func TestHandleSMS_Twilio_RendersSendsPersists(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	h := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"})

	input, err := json.Marshal(smsEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleSMS(ctx, engine.TaskPayload{RunID: "run_s1", StepID: "S1", Input: input}); err != nil {
		t.Fatalf("HandleSMS: %v", err)
	}

	// --- persisted message oracle ---
	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "sms")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Channel != "sms" {
		t.Errorf("Channel = %q, want sms", m.Channel)
	}
	if m.ProviderID != "twilio" {
		t.Errorf("ProviderID = %q, want twilio", m.ProviderID)
	}
	if m.IntegrationID != "integ-sms-test" {
		t.Errorf("IntegrationID = %q, want integ-sms-test", m.IntegrationID)
	}
	if m.Status != "sent" {
		t.Errorf("Status = %q, want sent", m.Status)
	}
	if m.Content != "You have 3 items" {
		t.Errorf("Content = %q, want %q", m.Content, "You have 3 items")
	}

	// --- capture oracle: recipient is the subscriber phone, rendered body ---
	sent := capt.Sent()
	if len(sent) != 1 {
		t.Fatalf("capt.Sent() len = %d, want 1", len(sent))
	}
	if sent[0].To != "+15557654321" {
		t.Errorf("To = %q, want %q", sent[0].To, "+15557654321")
	}
	if sent[0].Body != "You have 3 items" {
		t.Errorf("Body = %q, want %q", sent[0].Body, "You have 3 items")
	}
}

// TestHandleSMS_SignalWire_RendersSendsPersists: SignalWire follows the same
// path; the persisted provider id is "signalwire".
func TestHandleSMS_SignalWire_RendersSendsPersists(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	h := NewSMS(s, stubSMSResolver{p: capt, id: "signalwire"})

	input, _ := json.Marshal(smsEnvelope())
	if _, err := h.HandleSMS(ctx, engine.TaskPayload{RunID: "run_s2", StepID: "S1", Input: input}); err != nil {
		t.Fatalf("HandleSMS: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "sms")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "sent" || msgs[0].ProviderID != "signalwire" {
		t.Fatalf("expected one sent signalwire message, got %+v", msgs)
	}
}

// TestHandleSMS_MissingPhoneIsError: a subscriber with no phone number is a
// permanent fault — an "error" message is recorded and the provider is never
// invoked.
func TestHandleSMS_MissingPhoneIsError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	h := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"})

	env := smsEnvelope()
	env.Subscriber = json.RawMessage(`{"subscriberId":"user_123","firstName":"Ada"}`) // no phone
	input, _ := json.Marshal(env)
	if _, err := h.HandleSMS(ctx, engine.TaskPayload{RunID: "run_s3", StepID: "S1", Input: input}); err != nil {
		t.Fatalf("HandleSMS: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "sms")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "error" {
		t.Fatalf("expected one error message, got %+v", msgs)
	}
	if msgs[0].ErrorText == "" {
		t.Error("ErrorText empty, want a missing-phone explanation")
	}
	if got := len(capt.Sent()); got != 0 {
		t.Errorf("provider invoked %d times with no phone, want 0", got)
	}
}

// TestHandleSMS_SkipsWhenChannelDisabled: a disabled sms channel records a
// "skipped" message and never invokes the provider.
func TestHandleSMS_SkipsWhenChannelDisabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	h := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}).WithPreferences(stubChecker{enabled: false})

	input, _ := json.Marshal(smsEnvelope())
	if _, err := h.HandleSMS(ctx, engine.TaskPayload{RunID: "run_s4", StepID: "S1", Input: input}); err != nil {
		t.Fatalf("HandleSMS: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "sms")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "skipped" {
		t.Fatalf("expected one skipped message, got %+v", msgs)
	}
	if got := len(capt.Sent()); got != 0 {
		t.Errorf("provider invoked %d times when channel disabled, want 0", got)
	}
}

// TestHandleSMS_TransientSendErrorRetries: a transient provider fault returns a
// retryable error (engine retries) and records no terminal message.
func TestHandleSMS_TransientSendErrorRetries(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := NewSMS(s, stubSMSResolver{p: errSMSProvider{transient: true}, id: "twilio"})

	input, _ := json.Marshal(smsEnvelope())
	out, err := h.HandleSMS(ctx, engine.TaskPayload{RunID: "run_s5", StepID: "S1", Input: input})
	if err == nil {
		t.Fatal("expected a retryable error, got nil")
	}
	if out != nil {
		t.Errorf("expected nil output on retry, got %s", out)
	}

	msgs, _ := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "sms")
	if len(msgs) != 0 {
		t.Errorf("expected no persisted message on transient retry, got %d", len(msgs))
	}
}

// errSMSProvider is an SMSProvider that always fails; transient toggles whether
// the failure is wrapped as a TransientError.
type errSMSProvider struct{ transient bool }

func (e errSMSProvider) Send(_ context.Context, _ providers.SMSMessage) (providers.Result, error) {
	if e.transient {
		return providers.Result{}, providers.NewTransientError(context.DeadlineExceeded)
	}
	return providers.Result{}, context.Canceled
}

// smsEnvelopeWithIntegration returns smsEnvelope with the step's
// "integration" control set to identifier.
func smsEnvelopeWithIntegration(identifier string) runenv.InAppEnvelope {
	env := smsEnvelope()
	env.Step.Integration = identifier
	return env
}

// TestHandleSMS_ExplicitIntegration_RoutesToThatProvider proves that a step
// with a non-empty "integration" control resolves through ResolveIntegration
// (not the primary ResolveSMS path) and sends via that integration's
// provider, capturing its own providerID/integrationID on the persisted
// message.
func TestHandleSMS_ExplicitIntegration_RoutesToThatProvider(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	primary := providers.NewCaptureSMS()
	secondary := providers.NewCaptureSMS()
	h := NewSMS(s, stubSMSResolver{
		p:  primary,
		id: "twilio",
		integrations: map[string]stubIntegrationRoute{
			"signalwire-secondary": {provider: providers.SMSProvider(secondary), providerID: "signalwire", integrationID: "integ-secondary"},
		},
	})

	input, err := json.Marshal(smsEnvelopeWithIntegration("signalwire-secondary"))
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleSMS(ctx, engine.TaskPayload{RunID: "run_s_integ", StepID: "S1", Input: input}); err != nil {
		t.Fatalf("HandleSMS: %v", err)
	}

	if got := len(primary.Sent()); got != 0 {
		t.Errorf("primary provider invoked %d times, want 0", got)
	}
	if got := len(secondary.Sent()); got != 1 {
		t.Fatalf("secondary provider invoked %d times, want 1", got)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "sms")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	if msgs[0].Status != "sent" {
		t.Errorf("Status = %q, want sent", msgs[0].Status)
	}
	if msgs[0].ProviderID != "signalwire" {
		t.Errorf("ProviderID = %q, want signalwire", msgs[0].ProviderID)
	}
	if msgs[0].IntegrationID != "integ-secondary" {
		t.Errorf("IntegrationID = %q, want integ-secondary", msgs[0].IntegrationID)
	}
}

// TestHandleSMS_UnknownIntegration_ErrorPersistedNoRetry proves that a step
// naming an identifier the resolver cannot resolve persists a permanent
// "error" message naming the identifier and never sends, returning a nil
// error (no engine retry).
func TestHandleSMS_UnknownIntegration_ErrorPersistedNoRetry(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	h := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"})

	input, err := json.Marshal(smsEnvelopeWithIntegration("does-not-exist"))
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleSMS(ctx, engine.TaskPayload{RunID: "run_s_unknown", StepID: "S1", Input: input}); err != nil {
		t.Fatalf("HandleSMS returned a retryable error, want nil (permanent fault): %v", err)
	}

	if got := len(capt.Sent()); got != 0 {
		t.Errorf("provider invoked %d times for an unresolvable integration, want 0", got)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "sms")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "error" {
		t.Fatalf("expected one error message, got %+v", msgs)
	}
	if !strings.Contains(msgs[0].ErrorText, "does-not-exist") {
		t.Errorf("ErrorText = %q, must name the identifier", msgs[0].ErrorText)
	}
}
