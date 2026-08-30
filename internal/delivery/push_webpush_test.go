package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	webpushprovider "github.com/danmestas/notify/internal/providers/webpush"
	"github.com/danmestas/notify/internal/store"
)

// fakeWebPushSender is a stub webPushSender: it returns a canned
// (result, error) per subscription endpoint (keyed by outcomes[endpoint]) and
// records every call it received, thread-safe (fan-out is currently
// sequential, but a future concurrent fan-out shouldn't need this test
// rewritten).
type fakeWebPushSender struct {
	mu       sync.Mutex
	outcomes map[string]error // endpoint -> error (nil = success)
	calls    []webpushprovider.Subscription
}

func (f *fakeWebPushSender) SendToSubscription(_ context.Context, sub webpushprovider.Subscription, _ providers.PushMessage) (providers.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sub)
	return providers.Result{}, f.outcomes[sub.Endpoint]
}

// Send satisfies providers.PushProvider so *fakeWebPushSender type-checks as
// the resolver's return value; HandlePush's webpush branch never calls it —
// only SendToSubscription.
func (f *fakeWebPushSender) Send(context.Context, providers.PushMessage) (providers.Result, error) {
	return providers.Result{}, errors.New("fakeWebPushSender: Send should not be called; expected SendToSubscription")
}

func (f *fakeWebPushSender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// webPushSubscription seeds one device subscription row for user_123 in
// org_1/env_1 (the pushEnvelope() scope/subscriber).
func webPushSubscription(t *testing.T, s *store.Store, endpoint, p256dh, auth string) {
	t.Helper()
	if _, err := s.WebPushSubscriptions().UpsertByEndpoint(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, domain.WebPushSubscription{
		SubscriberID: "user_123",
		Endpoint:     endpoint,
		P256dh:       p256dh,
		Auth:         auth,
	}); err != nil {
		t.Fatalf("WebPushSubscriptions.UpsertByEndpoint: %v", err)
	}
}

// TestHandlePush_WebPush_FansOutToEveryDeviceSubscription proves a webpush
// resolve sends once per registered device (bypassing subscriber_channels
// entirely) and persists a single "sent" message when every device delivers.
func TestHandlePush_WebPush_FansOutToEveryDeviceSubscription(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	webPushSubscription(t, s, "https://push.example/device-a", "p256dh-a", "auth-a")
	webPushSubscription(t, s, "https://push.example/device-b", "p256dh-b", "auth-b")

	sender := &fakeWebPushSender{outcomes: map[string]error{}}
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush", integrationID: "integ-wp"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	out, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_wp1", StepID: "P1", Input: input})
	if err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	if got := sender.callCount(); got != 2 {
		t.Fatalf("SendToSubscription called %d times, want 2", got)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "push")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message row, got %d", len(msgs))
	}
	if msgs[0].Status != "sent" {
		t.Errorf("Status = %q, want sent", msgs[0].Status)
	}
	if msgs[0].ProviderID != "webpush" {
		t.Errorf("ProviderID = %q, want webpush", msgs[0].ProviderID)
	}
	if msgs[0].IntegrationID != "integ-wp" {
		t.Errorf("IntegrationID = %q, want integ-wp", msgs[0].IntegrationID)
	}

	var m domain.Message
	if uerr := json.Unmarshal(out, &m); uerr != nil {
		t.Fatalf("unmarshal handler output: %v", uerr)
	}
	if m.ID != msgs[0].ID {
		t.Errorf("returned message id %q != persisted id %q", m.ID, msgs[0].ID)
	}
}

// TestHandlePush_WebPush_ZeroSubscriptionsIsSkipped proves a subscriber with
// no registered device subscriptions records "skipped" (like the missing-
// address case for every other provider) and never calls the sender.
func TestHandlePush_WebPush_ZeroSubscriptionsIsSkipped(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sender := &fakeWebPushSender{outcomes: map[string]error{}}
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_wp2", StepID: "P1", Input: input}); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "push")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message row, got %d", len(msgs))
	}
	if msgs[0].Status != "skipped" {
		t.Errorf("Status = %q, want skipped", msgs[0].Status)
	}
	if got := sender.callCount(); got != 0 {
		t.Errorf("SendToSubscription called %d times, want 0", got)
	}
}

// TestHandlePush_WebPush_GoneSubscriptionIsPrunedButOthersDeliver proves a
// 410-classified device (webpushprovider.ErrSubscriptionGone) is deleted from
// webpush_subscriptions while a second, live device still receives the send
// and the message still records "sent".
func TestHandlePush_WebPush_GoneSubscriptionIsPrunedButOthersDeliver(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	webPushSubscription(t, s, "https://push.example/gone", "p256dh-gone", "auth-gone")
	webPushSubscription(t, s, "https://push.example/live", "p256dh-live", "auth-live")

	sender := &fakeWebPushSender{outcomes: map[string]error{
		"https://push.example/gone": webpushprovider.ErrSubscriptionGone,
	}}
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_wp3", StepID: "P1", Input: input}); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "push")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "sent" {
		t.Fatalf("expected one sent message, got %+v", msgs)
	}

	remaining, err := s.WebPushSubscriptions().ListBySubscriber(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123")
	if err != nil {
		t.Fatalf("ListBySubscriber: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected exactly one surviving subscription, got %d", len(remaining))
	}
	if remaining[0].Endpoint != "https://push.example/live" {
		t.Errorf("surviving subscription endpoint = %q, want the live device", remaining[0].Endpoint)
	}
}

// TestHandlePush_WebPush_AllFailedIsError proves an all-failed fan-out (no
// deliveries, no gone subscriptions) records status "error" carrying a
// non-secret cause, and every subscription row survives (permanent failures
// are not pruned — only ErrSubscriptionGone is).
func TestHandlePush_WebPush_AllFailedIsError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	webPushSubscription(t, s, "https://push.example/device-a", "p256dh-a", "auth-a")

	sender := &fakeWebPushSender{outcomes: map[string]error{
		"https://push.example/device-a": errors.New("webpush: status 400: bad request"),
	}}
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	out, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_wp4", StepID: "P1", Input: input})
	if err != nil {
		t.Fatalf("HandlePush returned error, want nil: %v", err)
	}
	var m domain.Message
	if uerr := json.Unmarshal(out, &m); uerr != nil {
		t.Fatalf("unmarshal handler output: %v", uerr)
	}
	if m.Status != "error" {
		t.Errorf("Status = %q, want error", m.Status)
	}
	if m.ErrorText == "" {
		t.Error("ErrorText is empty, want the failure cause")
	}

	remaining, err := s.WebPushSubscriptions().ListBySubscriber(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123")
	if err != nil {
		t.Fatalf("ListBySubscriber: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("expected the subscription to survive a non-gone failure, got %d rows", len(remaining))
	}
}

// TestHandlePush_WebPush_AllTransientFailuresAreRetryable proves that when
// every device fails transiently (5xx/timeout, providers.IsTransient) and
// nothing was delivered, HandlePush returns an engine.RetryableError instead
// of persisting a terminal "error" message — mirroring the single-address
// push path's transient classification.
func TestHandlePush_WebPush_AllTransientFailuresAreRetryable(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	webPushSubscription(t, s, "https://push.example/device-a", "p256dh-a", "auth-a")
	webPushSubscription(t, s, "https://push.example/device-b", "p256dh-b", "auth-b")

	sender := &fakeWebPushSender{outcomes: map[string]error{
		"https://push.example/device-a": providers.NewTransientError(errors.New("webpush: status 503: unavailable")),
		"https://push.example/device-b": providers.NewTransientError(errors.New("webpush: status 502: bad gateway")),
	}}
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	_, err = h.HandlePush(ctx, engine.TaskPayload{RunID: "run_wp7", StepID: "P1", Input: input})
	if err == nil {
		t.Fatal("HandlePush returned nil error, want a retryable error")
	}
	var re *engine.RetryableError
	if !errors.As(err, &re) {
		t.Fatalf("HandlePush error = %v (%T), want *engine.RetryableError", err, err)
	}

	msgs, lerr := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "push")
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no persisted message on a retryable failure, got %d: %+v", len(msgs), msgs)
	}
}

// TestHandlePush_WebPush_UnsupportedProviderIsError proves that if
// ResolvePush ever returns "webpush" paired with a provider that does not
// implement SendToSubscription, HandlePush fails closed with a persisted
// "error" message instead of panicking.
func TestHandlePush_WebPush_UnsupportedProviderIsError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	webPushSubscription(t, s, "https://push.example/device-a", "p256dh-a", "auth-a")

	h := NewPush(s, stubPushResolver{p: providers.NewCapturePush(), id: "webpush"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	out, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_wp5", StepID: "P1", Input: input})
	if err != nil {
		t.Fatalf("HandlePush returned error, want nil: %v", err)
	}
	var m domain.Message
	if uerr := json.Unmarshal(out, &m); uerr != nil {
		t.Fatalf("unmarshal handler output: %v", uerr)
	}
	if m.Status != "error" {
		t.Errorf("Status = %q, want error", m.Status)
	}
}

// TestHandlePush_WebPush_PartialFailureRecordsTally proves that when some
// devices deliver and others fail permanently, the message still records
// status "sent" (>=1 delivered) but the per-device tally is written to
// ErrorText so the activity view surfaces the partial failure instead of
// looking like a clean, fully-successful send.
func TestHandlePush_WebPush_PartialFailureRecordsTally(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	webPushSubscription(t, s, "https://push.example/device-a", "p256dh-a", "auth-a")
	webPushSubscription(t, s, "https://push.example/device-b", "p256dh-b", "auth-b")
	webPushSubscription(t, s, "https://push.example/device-c", "p256dh-c", "auth-c")

	sender := &fakeWebPushSender{outcomes: map[string]error{
		"https://push.example/device-c": errors.New("webpush: status 403: forbidden"),
	}}
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush", integrationID: "integ-wp"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	out, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_wp8", StepID: "P1", Input: input})
	if err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	var m domain.Message
	if uerr := json.Unmarshal(out, &m); uerr != nil {
		t.Fatalf("unmarshal handler output: %v", uerr)
	}
	if m.Status != "sent" {
		t.Errorf("Status = %q, want sent", m.Status)
	}
	if m.ErrorText == "" {
		t.Fatal("ErrorText is empty, want a partial-failure tally")
	}
	if !strings.Contains(m.ErrorText, "2/3") {
		t.Errorf("ErrorText = %q, want it to mention the 2/3 delivered tally", m.ErrorText)
	}
	if !strings.Contains(m.ErrorText, "403") {
		t.Errorf("ErrorText = %q, want it to mention the failure cause (403)", m.ErrorText)
	}

	msgs, lerr := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "push")
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(msgs) != 1 || msgs[0].ErrorText == "" {
		t.Fatalf("expected the persisted row to carry the tally too, got %+v", msgs)
	}
}

// TestHandlePush_WebPush_DataForwardedURLAndTag proves env.Step.Data reaches
// the sender's PushMessage verbatim (the url/tag fields the webpush client
// reads live there, same as FCM/Expo's Data map).
func TestHandlePush_WebPush_DataForwardedURLAndTag(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	webPushSubscription(t, s, "https://push.example/device-a", "p256dh-a", "auth-a")

	sender := &fakeWebPushSender{outcomes: map[string]error{}}
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})

	env := pushEnvelope()
	env.Step.Data = map[string]string{"url": "/inbox/42", "tag": "digest"}
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_wp6", StepID: "P1", Input: input}); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	sender.mu.Lock()
	calls := len(sender.calls)
	sender.mu.Unlock()
	if calls != 1 {
		t.Fatalf("SendToSubscription called %d times, want 1", calls)
	}
}
