package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/danmestas/notify/internal/deliveryhealth"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers/telegram"
	"github.com/danmestas/notify/internal/providers/twilio"
	"github.com/danmestas/notify/internal/store"
)

// switchable is a provider stub that answers 503 until ok is set, then
// answers with okBody.
func switchable(t *testing.T, okBody string) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var ok atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ok.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"service unavailable"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &ok
}

// healthRig is a store with one integration whose delivery outcomes a
// deliveryhealth.Tracker observes.
func healthRig(t *testing.T, providerID, channel string) (*store.Store, string) {
	t.Helper()
	s := newStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	in, err := s.Integrations().Create(context.Background(), scope, domain.Integration{
		ProviderID: providerID, Channel: channel, Active: true, Identifier: providerID + "-main",
	})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}
	s.SetDeliveryObserver(deliveryhealth.New(s, deliveryhealth.DefaultSettings(), nil))
	return s, in.ID
}

func wantHealth(t *testing.T, s *store.Store, integrationID string, state deliveryhealth.State, class string) {
	t.Helper()
	h, err := s.IntegrationHealth().Get(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, integrationID)
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	if deliveryhealth.State(h.State) != state || h.LastErrorClass != class {
		t.Fatalf("health = %s/%s, want %s/%s (%+v)", h.State, h.LastErrorClass, state, class, h)
	}
}

func wantRetryable(t *testing.T, out json.RawMessage, err error) {
	t.Helper()
	var re *engine.RetryableError
	if !errors.As(err, &re) || out != nil {
		t.Fatalf("attempt = (%s, %v), want a retryable error and no output", out, err)
	}
}

// An SMS provider outage reaches health even though the engine retries and
// no message is written; the retry that succeeds makes it healthy again.
func TestHandleSMS_TransientOutageCountsAgainstHealth(t *testing.T) {
	ctx := context.Background()
	s, integ := healthRig(t, "twilio", "sms")
	verifySMSSubscriber(t, s, "+15557654321")
	srv, up := switchable(t, `{"sid":"SM1"}`)
	client := &twilio.Client{AccountSID: "AC1", AuthToken: "t", From: "+15550000000", BaseURL: srv.URL}
	h := NewSMS(s, stubSMSResolver{p: client, id: "twilio", integrationID: integ})
	input, _ := json.Marshal(smsEnvelope())
	task := engine.TaskPayload{RunID: "run_1", StepID: "S1", Input: input}

	for range 3 {
		out, err := h.HandleSMS(ctx, task)
		wantRetryable(t, out, err)
	}
	if msgs, _ := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "sms"); len(msgs) != 0 {
		t.Fatalf("transient attempts wrote %d messages", len(msgs))
	}
	wantHealth(t, s, integ, deliveryhealth.Failing, "provider_down")

	up.Store(true)
	if _, err := h.HandleSMS(ctx, task); err != nil {
		t.Fatalf("retry: %v", err)
	}
	wantHealth(t, s, integ, deliveryhealth.Healthy, "provider_down")
}

func TestHandleChat_TransientOutageCountsAgainstHealth(t *testing.T) {
	ctx := context.Background()
	s, integ := healthRig(t, "telegram", "chat")
	seedChatAddress(t, s, "telegram", "987654321")
	srv, up := switchable(t, `{"ok":true,"result":{"message_id":1}}`)
	client := &telegram.Client{BotToken: "123:abc", BaseURL: srv.URL}
	h := NewChat(s, stubChatResolver{p: client, id: "telegram", integrationID: integ})
	input, _ := json.Marshal(chatEnvelope())
	task := engine.TaskPayload{RunID: "run_1", StepID: "C1", Input: input}

	for range 3 {
		out, err := h.HandleChat(ctx, task)
		wantRetryable(t, out, err)
	}
	wantHealth(t, s, integ, deliveryhealth.Failing, "provider_down")

	up.Store(true)
	if _, err := h.HandleChat(ctx, task); err != nil {
		t.Fatalf("retry: %v", err)
	}
	wantHealth(t, s, integ, deliveryhealth.Healthy, "provider_down")
}
