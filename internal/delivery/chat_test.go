package delivery

import (
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// deliveryCounter reads the "<channel>.<outcome>" counter out of the
// process-global notify_delivery_total expvar map, returning 0 when absent.
// Counters are cumulative across the test binary, so callers assert on a delta.
func deliveryCounter(t *testing.T, key string) int64 {
	t.Helper()
	m, ok := expvar.Get("notify_delivery_total").(*expvar.Map)
	if !ok {
		t.Fatalf("notify_delivery_total not published as *expvar.Map")
	}
	iv, ok := m.Get(key).(*expvar.Int)
	if !ok || iv == nil {
		return 0
	}
	return iv.Value()
}

// stubChatResolver is a fixed-answer ChatResolver standing in for
// connectivity.Service in the unit tests.
type stubChatResolver struct {
	p             providers.ChatProvider
	id            string // providerID
	integrationID string // defaults to "integ-chat-test" when empty
	err           error

	// integrations routes explicit "integration" identifiers (env.Step.Integration)
	// to a distinct provider/providerID/integrationID, standing in for
	// connectivity.Service.ResolveIntegration. An identifier with no entry
	// returns errUnknownIntegration, mirroring ErrIntegrationUnusable.
	integrations map[string]stubIntegrationRoute
}

// stubIntegrationRoute is one (provider, providerID, integrationID) answer a
// stub resolver hands back for a specific "integration" identifier. from is
// only meaningful for the email stub's ResolveEmailIntegration, standing in
// for the integration's configured From address; other channels leave it "".
type stubIntegrationRoute struct {
	provider      any
	providerID    string
	integrationID string
	from          string
}

// errUnknownIntegration stands in for connectivity.ErrIntegrationUnusable: a
// permanent fault (delivery never wraps it in a retryable error) that names
// the identifier in its message.
func errUnknownIntegration(channel, identifier string) error {
	return fmt.Errorf("stub: integration %q unusable for channel %q: no integration with this identifier", identifier, channel)
}

func (s stubChatResolver) ResolveChat(_ context.Context, _ domain.Scope) (providers.ChatProvider, string, string, error) {
	iid := s.integrationID
	if iid == "" {
		iid = "integ-chat-test"
	}
	return s.p, s.id, iid, s.err
}

func (s stubChatResolver) ResolveIntegration(ctx context.Context, scope domain.Scope, channel, identifier string) (any, string, string, error) {
	if identifier == "" {
		return s.ResolveChat(ctx, scope)
	}
	route, ok := s.integrations[identifier]
	if !ok {
		return nil, "", "", errUnknownIntegration(channel, identifier)
	}
	return route.provider, route.providerID, route.integrationID, nil
}

// chatEnvelope mirrors inAppEnvelope for the chat channel.
func chatEnvelope() runenv.InAppEnvelope {
	return runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		TransactionID:  "txn_chat",
		WorkflowID:     "wf_1",
		NotificationID: "ntf_1",
		SubscriberID:   "user_123",
		Subscriber:     json.RawMessage(`{"subscriberId":"user_123","firstName":"Ada"}`),
		Payload:        json.RawMessage(`{"itemCount":3}`),
		Step: runenv.Step{
			StepID:          "C1",
			Channel:         "chat",
			SubjectTemplate: "Hi {{subscriber.firstName}}",
			BodyTemplate:    "You have {{payload.itemCount}} items",
		},
	}
}

// seedChatAddress registers the subscriber's chat address (telegram chat_id or
// a discord webhook override) in subscriber_channels for providerID.
func seedChatAddress(t *testing.T, s *store.Store, providerID, address string) {
	t.Helper()
	if _, err := s.SubscriberChannels().Upsert(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, domain.SubscriberChannel{
		SubscriberID: "user_123",
		ProviderID:   providerID,
		Type:         "chat",
		Credentials:  json.RawMessage(`{"address":"` + address + `"}`),
	}); err != nil {
		t.Fatalf("SubscriberChannels.Upsert: %v", err)
	}
}

func TestHandleChat_Telegram_RendersSendsPersists(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedChatAddress(t, s, "telegram", "987654321")
	capt := providers.NewCaptureChat()
	h := NewChat(s, stubChatResolver{p: capt, id: "telegram"})

	input, err := json.Marshal(chatEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleChat(ctx, engine.TaskPayload{RunID: "run_c1", StepID: "C1", Input: input}); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}

	// --- persisted message oracle ---
	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "chat")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Channel != "chat" {
		t.Errorf("Channel = %q, want chat", m.Channel)
	}
	if m.ProviderID != "telegram" {
		t.Errorf("ProviderID = %q, want telegram", m.ProviderID)
	}
	if m.IntegrationID != "integ-chat-test" {
		t.Errorf("IntegrationID = %q, want integ-chat-test", m.IntegrationID)
	}
	if m.Status != "sent" {
		t.Errorf("Status = %q, want sent", m.Status)
	}
	if m.Content != "You have 3 items" {
		t.Errorf("Content = %q, want %q", m.Content, "You have 3 items")
	}

	// --- capture oracle: address from subscriber_channels, rendered content ---
	sent := capt.Sent()
	if len(sent) != 1 {
		t.Fatalf("capt.Sent() len = %d, want 1", len(sent))
	}
	if sent[0].Address != "987654321" {
		t.Errorf("Address = %q, want %q", sent[0].Address, "987654321")
	}
	if sent[0].Content != "You have 3 items" {
		t.Errorf("Content = %q, want %q", sent[0].Content, "You have 3 items")
	}
}

// TestHandleChat_IncrementsDeliveredCounter guards the metrics wiring: a
// successfully handled chat send must bump notify_delivery_total{chat.delivered}
// by exactly one.
func TestHandleChat_IncrementsDeliveredCounter(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedChatAddress(t, s, "telegram", "987654321")
	h := NewChat(s, stubChatResolver{p: providers.NewCaptureChat(), id: "telegram"})

	input, err := json.Marshal(chatEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	before := deliveryCounter(t, "chat.delivered")
	if _, err := h.HandleChat(ctx, engine.TaskPayload{RunID: "run_metric", StepID: "C1", Input: input}); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}
	if got := deliveryCounter(t, "chat.delivered") - before; got != 1 {
		t.Fatalf("chat.delivered delta = %d, want 1", got)
	}
}

// TestHandleChat_Discord_FallsBackToIntegrationWebhook: Discord needs no
// per-subscriber address, so an absent subscriber_channels row yields an empty
// ChatMessage.Address (the provider then uses its configured webhook) and the
// send still persists a sent message.
func TestHandleChat_Discord_FallsBackToIntegrationWebhook(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	capt := providers.NewCaptureChat()
	h := NewChat(s, stubChatResolver{p: capt, id: "discord"})

	input, err := json.Marshal(chatEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleChat(ctx, engine.TaskPayload{RunID: "run_c2", StepID: "C1", Input: input}); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "chat")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "sent" {
		t.Fatalf("expected one sent message, got %+v", msgs)
	}
	sent := capt.Sent()
	if len(sent) != 1 {
		t.Fatalf("capt.Sent() len = %d, want 1", len(sent))
	}
	if sent[0].Address != "" {
		t.Errorf("Address = %q, want empty (webhook fallback)", sent[0].Address)
	}
}

// TestHandleChat_Telegram_MissingChatIDIsError: a telegram subscriber with no
// chat_id is a permanent fault — an "error" message is recorded and the
// provider is never invoked.
func TestHandleChat_Telegram_MissingChatIDIsError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	capt := providers.NewCaptureChat()
	h := NewChat(s, stubChatResolver{p: capt, id: "telegram"})

	input, err := json.Marshal(chatEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleChat(ctx, engine.TaskPayload{RunID: "run_c3", StepID: "C1", Input: input}); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "chat")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "error" {
		t.Fatalf("expected one error message, got %+v", msgs)
	}
	if msgs[0].ErrorText == "" {
		t.Error("ErrorText empty, want a missing-chat_id explanation")
	}
	if got := len(capt.Sent()); got != 0 {
		t.Errorf("provider invoked %d times with no chat_id, want 0", got)
	}
}

func TestHandleChat_SkipsWhenChannelDisabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedChatAddress(t, s, "telegram", "987654321")
	capt := providers.NewCaptureChat()
	h := NewChat(s, stubChatResolver{p: capt, id: "telegram"}).WithPreferences(stubChecker{enabled: false})

	input, err := json.Marshal(chatEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleChat(ctx, engine.TaskPayload{RunID: "run_c4", StepID: "C1", Input: input}); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "chat")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "skipped" {
		t.Fatalf("expected one skipped message, got %+v", msgs)
	}
	if got := len(capt.Sent()); got != 0 {
		t.Errorf("provider invoked %d times on skip, want 0", got)
	}
}

// TestHandleChat_ScopeIsolation: a chat sent in env_1 is invisible to env_2.
func TestHandleChat_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedChatAddress(t, s, "telegram", "987654321")
	capt := providers.NewCaptureChat()
	h := NewChat(s, stubChatResolver{p: capt, id: "telegram"})

	input, err := json.Marshal(chatEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleChat(ctx, engine.TaskPayload{RunID: "run_c5", StepID: "C1", Input: input}); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}

	other, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_2"}, "user_123", "chat")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("env_2 sees %d messages from env_1, want 0", len(other))
	}
}

// chatEnvelopeWithIntegration returns chatEnvelope with the step's
// "integration" control set to identifier.
func chatEnvelopeWithIntegration(identifier string) runenv.InAppEnvelope {
	env := chatEnvelope()
	env.Step.Integration = identifier
	return env
}

// TestHandleChat_ExplicitIntegration_RoutesToThatProvider proves that a step
// with a non-empty "integration" control resolves through ResolveIntegration
// (not the primary ResolveChat path) and sends via that integration's
// provider, capturing its own providerID/integrationID on the persisted
// message.
func TestHandleChat_ExplicitIntegration_RoutesToThatProvider(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	primary := providers.NewCaptureChat()
	secondary := providers.NewCaptureChat()
	h := NewChat(s, stubChatResolver{
		p:  primary,
		id: "discord",
		integrations: map[string]stubIntegrationRoute{
			"disc-secondary": {provider: providers.ChatProvider(secondary), providerID: "discord", integrationID: "integ-secondary"},
		},
	})

	input, err := json.Marshal(chatEnvelopeWithIntegration("disc-secondary"))
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleChat(ctx, engine.TaskPayload{RunID: "run_c_integ", StepID: "C1", Input: input}); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}

	if got := len(primary.Sent()); got != 0 {
		t.Errorf("primary provider invoked %d times, want 0", got)
	}
	if got := len(secondary.Sent()); got != 1 {
		t.Fatalf("secondary provider invoked %d times, want 1", got)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "chat")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	if msgs[0].Status != "sent" {
		t.Errorf("Status = %q, want sent", msgs[0].Status)
	}
	if msgs[0].IntegrationID != "integ-secondary" {
		t.Errorf("IntegrationID = %q, want integ-secondary", msgs[0].IntegrationID)
	}
}

// TestHandleChat_UnknownIntegration_ErrorPersistedNoRetry proves that a step
// naming an identifier the resolver cannot resolve (unknown, inactive, or
// wrong-channel — the stub covers "unknown") persists a permanent "error"
// message naming the identifier and never sends, returning a nil error (no
// engine retry).
func TestHandleChat_UnknownIntegration_ErrorPersistedNoRetry(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	capt := providers.NewCaptureChat()
	h := NewChat(s, stubChatResolver{p: capt, id: "discord"})

	input, err := json.Marshal(chatEnvelopeWithIntegration("does-not-exist"))
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleChat(ctx, engine.TaskPayload{RunID: "run_c_unknown", StepID: "C1", Input: input}); err != nil {
		t.Fatalf("HandleChat returned a retryable error, want nil (permanent fault): %v", err)
	}

	if got := len(capt.Sent()); got != 0 {
		t.Errorf("provider invoked %d times for an unresolvable integration, want 0", got)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "chat")
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
