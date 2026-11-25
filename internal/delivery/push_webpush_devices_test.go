package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	webpushprovider "github.com/danmestas/notify/internal/providers/webpush"
	"github.com/danmestas/notify/internal/webpushevent"
)

// removedEvents decodes the webpush.subscription_removed events among the
// publisher's delivery events.
func removedEvents(t *testing.T, p *stubPublisher) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range p.deliveryCalls {
		var ev map[string]any
		if err := json.Unmarshal(c.data, &ev); err != nil {
			t.Fatalf("decode %s: %v", c.data, err)
		}
		if ev["type"] == "webpush.subscription_removed" {
			out = append(out, ev)
		}
	}
	return out
}

// TestWebPush_GoneDeletesAndPublishesRemoved is #172: a device the push
// service reports gone is deleted, and the tenant's webhooks are told.
func TestWebPush_GoneDeletesAndPublishesRemoved(t *testing.T) {
	s := newStore(t)
	webPushSubscription(t, s, googleEP, "p", "a")
	webPushSubscription(t, s, appleEP, "p", "a")
	sender := newScripted(map[string][]error{appleEP: {webpushprovider.ErrSubscriptionGone}})
	pub := &stubPublisher{}
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"}).WithEventBus(pub)

	sendWebPush(t, h)

	evs := removedEvents(t, pub)
	if len(evs) != 1 {
		t.Fatalf("removed events = %v, want 1", evs)
	}
	sum := sha256.Sum256([]byte(appleEP))
	ev := evs[0]
	if ev["reason"] != "gone" || ev["endpointHash"] != hex.EncodeToString(sum[:]) || ev["subscriberId"] != "user_123" ||
		ev["OrgID"] != "org_1" || ev["EnvID"] != "env_1" || ev["deviceId"] == "" {
		t.Errorf("event = %v", ev)
	}
}

// TestWebPush_RetiredPublishesRemoved: a device refused webPushRetireAfter
// times in a row is deleted and the tenant is told, reason "retired".
func TestWebPush_RetiredPublishesRemoved(t *testing.T) {
	s := newStore(t)
	webPushSubscription(t, s, appleEP, "p", "a")
	sender := newScripted(map[string][]error{appleEP: {&webpushprovider.StatusError{Code: 403, Reason: "BadJwtToken"}}})
	pub := &stubPublisher{}
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"}).WithEventBus(pub)

	for i := 0; i < webPushRetireAfter; i++ {
		if n := len(removedEvents(t, pub)); n != 0 {
			t.Fatalf("event before retirement, after %d refusals", i)
		}
		sendWebPush(t, h)
	}

	evs := removedEvents(t, pub)
	if len(evs) != 1 || evs[0]["reason"] != "retired" || evs[0]["endpointHash"] != webpushevent.EndpointHash(appleEP) {
		t.Fatalf("removed events = %v, want one retired", evs)
	}
}

// TestWebPush_DeliveredStampsLastSuccessAndFailureClass is #172: the device
// row records when it last received a push and the class of its last failure.
func TestWebPush_DeliveredStampsLastSuccessAndFailureClass(t *testing.T) {
	s := newStore(t)
	webPushSubscription(t, s, googleEP, "p", "a")
	webPushSubscription(t, s, appleEP, "p", "a")
	sender := newScripted(map[string][]error{appleEP: {&webpushprovider.StatusError{Code: 403, Reason: "BadJwtToken"}}})
	h := NewPush(s, stubPushResolver{p: sender, id: "webpush"})

	sendWebPush(t, h)

	subs, err := s.WebPushSubscriptions().ListBySubscriber(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, sub := range subs {
		switch sub.Endpoint {
		case googleEP:
			if sub.LastSuccessAt == "" || sub.LastErrorClass != "" {
				t.Errorf("google = %+v, want success stamp and no error class", sub)
			}
		case appleEP:
			if sub.LastSuccessAt != "" || sub.LastErrorClass == "" {
				t.Errorf("apple = %+v, want an error class and no success stamp", sub)
			}
		}
	}
}
