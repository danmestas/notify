package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	webpushprovider "github.com/danmestas/notify/internal/providers/webpush"
)

// scriptedSender answers each endpoint from a queue of results, one per
// call; the last result repeats once the queue runs out.
type scriptedSender struct {
	mu     sync.Mutex
	script map[string][]error
	calls  map[string]int
}

func newScripted(script map[string][]error) *scriptedSender {
	return &scriptedSender{script: script, calls: map[string]int{}}
}

func (s *scriptedSender) SendToSubscription(_ context.Context, sub webpushprovider.Subscription, _ providers.PushMessage) (providers.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.script[sub.Endpoint]
	n := s.calls[sub.Endpoint]
	s.calls[sub.Endpoint]++
	if len(q) == 0 {
		return providers.Result{}, nil
	}
	if n >= len(q) {
		n = len(q) - 1
	}
	return providers.Result{}, q[n]
}

func (s *scriptedSender) Send(context.Context, providers.PushMessage) (providers.Result, error) {
	return providers.Result{}, errors.New("not used")
}

func (s *scriptedSender) callsTo(endpoint string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[endpoint]
}

const (
	appleEP  = "https://web.push.apple.com/device-apple"
	googleEP = "https://fcm.googleapis.com/fcm/send/device-google"
)

func sendWebPush(t *testing.T, h *PushHandler) domain.Message {
	t.Helper()
	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := h.HandlePush(context.Background(), engine.TaskPayload{RunID: "run_wpo", StepID: "P1", Input: input})
	if err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	var m domain.Message
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func devicesOf(t *testing.T, m domain.Message) map[string]webPushDevice {
	t.Helper()
	var x struct {
		Devices []webPushDevice `json:"devices"`
	}
	if err := json.Unmarshal(m.Extras, &x); err != nil {
		t.Fatalf("extras %s: %v", m.Extras, err)
	}
	out := map[string]webPushDevice{}
	for _, d := range x.Devices {
		out[d.Host] = d
	}
	return out
}

// TestWebPush_PartialDeliveryRecordsEachDevice is #359: a push that reaches
// one device and is refused by another is "sent", and extras.devices says
// which device refused, with the status and the push service's reason.
func TestWebPush_PartialDeliveryRecordsEachDevice(t *testing.T) {
	s := newStore(t)
	webPushSubscription(t, s, googleEP, "p", "a")
	webPushSubscription(t, s, appleEP, "p", "a")
	sender := newScripted(map[string][]error{appleEP: {&webpushprovider.StatusError{Code: 403, Reason: "BadJwtToken"}}})
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})

	m := sendWebPush(t, h)
	if m.Status != "sent" {
		t.Fatalf("Status = %q, want sent", m.Status)
	}
	d := devicesOf(t, m)
	if g := d["fcm.googleapis.com"]; g.Outcome != "delivered" || g.SubscriptionID == "" {
		t.Errorf("google device = %+v", g)
	}
	if a := d["web.push.apple.com"]; a.Outcome != "failed" || a.Status != 403 || a.Reason != "BadJwtToken" {
		t.Errorf("apple device = %+v, want failed 403 BadJwtToken", a)
	}
	for _, dev := range d {
		if dev.Host == "" || dev.Reason == appleEP {
			t.Errorf("device record leaks or lacks the endpoint: %+v", dev)
		}
	}
}

// TestWebPush_RetiresAfterRepeatedRefusalsAndSuccessResets is #360: three
// refusals in a row retire the subscription; a success in between resets
// the count.
func TestWebPush_RetiresAfterRepeatedRefusalsAndSuccessResets(t *testing.T) {
	s := newStore(t)
	webPushSubscription(t, s, googleEP, "p", "a")
	webPushSubscription(t, s, appleEP, "p", "a")
	refuse := &webpushprovider.StatusError{Code: 403, Reason: "BadDeviceToken"}
	sender := newScripted(map[string][]error{appleEP: {refuse, refuse, nil, refuse, refuse, refuse}})
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	want := []string{"failed", "failed", "delivered", "failed", "failed", "retired"}
	for i, w := range want {
		m := sendWebPush(t, h)
		if got := devicesOf(t, m)["web.push.apple.com"].Outcome; got != w {
			t.Fatalf("send %d: apple outcome = %q, want %q", i+1, got, w)
		}
	}
	subs, err := s.WebPushSubscriptions().ListBySubscriber(context.Background(), scope, "user_123")
	if err != nil || len(subs) != 1 || subs[0].Endpoint != googleEP {
		t.Fatalf("subscriptions after retirement = %+v, %v; want only google", subs, err)
	}
	calls := sender.callsTo(appleEP)
	sendWebPush(t, h)
	if sender.callsTo(appleEP) != calls {
		t.Fatal("a retired subscription was tried again")
	}
}

// TestWebPush_PayloadTooLargeNeverRetires: 413 is the message's fault and
// would fail on every device, so it never counts against one.
func TestWebPush_PayloadTooLargeNeverRetires(t *testing.T) {
	s := newStore(t)
	webPushSubscription(t, s, googleEP, "p", "a")
	webPushSubscription(t, s, appleEP, "p", "a")
	sender := newScripted(map[string][]error{appleEP: {&webpushprovider.StatusError{Code: 413, Reason: "PayloadTooLarge"}}})
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})
	for i := 0; i < 5; i++ {
		if got := devicesOf(t, sendWebPush(t, h))["web.push.apple.com"].Outcome; got != "failed" {
			t.Fatalf("send %d: outcome %q, want failed", i+1, got)
		}
	}
}

// TestWebPush_TimeoutOnOneDeviceIsRetriedInTheSameSend is #360's second
// half: a device that times out while another delivers is retried once,
// instead of being lost because the step is "sent" and never retried.
func TestWebPush_TimeoutOnOneDeviceIsRetriedInTheSameSend(t *testing.T) {
	s := newStore(t)
	webPushSubscription(t, s, googleEP, "p", "a")
	webPushSubscription(t, s, appleEP, "p", "a")
	timeout := providers.NewTransientError(errors.New("context deadline exceeded"))
	sender := newScripted(map[string][]error{appleEP: {timeout, nil}})
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})
	h.webPushRetryDelay = 0

	m := sendWebPush(t, h)
	if got := devicesOf(t, m)["web.push.apple.com"].Outcome; got != "delivered" {
		t.Fatalf("apple outcome = %q, want delivered on the retry", got)
	}
	if n := sender.callsTo(appleEP); n != 2 {
		t.Fatalf("apple tried %d times, want 2", n)
	}
	if n := sender.callsTo(googleEP); n != 1 {
		t.Fatalf("google tried %d times, want 1: a delivered device is not re-sent", n)
	}
	if m.ErrorText != "" {
		t.Fatalf("ErrorText = %q, want empty after a successful retry", m.ErrorText)
	}
}

// TestWebPush_TimeoutThatPersistsIsRecordedNotRetired: a device still timing
// out after its retry is recorded as failed and keeps its subscription.
func TestWebPush_TimeoutThatPersistsIsRecordedNotRetired(t *testing.T) {
	s := newStore(t)
	webPushSubscription(t, s, googleEP, "p", "a")
	webPushSubscription(t, s, appleEP, "p", "a")
	timeout := providers.NewTransientError(errors.New("context deadline exceeded"))
	sender := newScripted(map[string][]error{appleEP: {timeout}})
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})
	h.webPushRetryDelay = 0

	for i := 0; i < 4; i++ {
		if got := devicesOf(t, sendWebPush(t, h))["web.push.apple.com"].Outcome; got != "failed" {
			t.Fatalf("send %d: outcome %q, want failed", i+1, got)
		}
	}
	subs, _ := s.WebPushSubscriptions().ListBySubscriber(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123")
	if len(subs) != 2 {
		t.Fatalf("subscriptions = %d, want 2: timeouts never retire a device", len(subs))
	}
}
