package delivery_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	vapidpush "github.com/SherClockHolmes/webpush-go"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// This file lives in package delivery_test (an external test package) rather
// than package delivery: internal/connectivity imports
// internal/delivery/httpaction, which imports internal/delivery itself, so a
// package-delivery test file importing internal/connectivity would be an
// import cycle. connectivity has no such cycle with the exported
// delivery_test package, since only in-package (non-_test) imports count.

// testMasterKey returns a fixed 32-byte AES-256-GCM key for connectivity.New.
func testMasterKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

// TestHandlePush_WebPush_ReachableThroughRealConnectivityResolver is the
// end-to-end regression for issue #302: it seeds a real store and a real
// *connectivity.Service (not a stub resolver), mints the scope's VAPID
// keypair through EnsureWebPushKeys exactly as GET /v1/integrations/webpush/key
// does, registers one device subscription, and proves HandlePush — routed
// through the real ResolvePush (GetActiveByChannel, active=1) — resolves
// "webpush" and actually reaches SendToSubscription (observed as a real HTTP
// POST landing on a stub push endpoint).
func TestHandlePush_WebPush_ReachableThroughRealConnectivityResolver(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// WithHTTPClient bypasses the SSRF guard deliveryClient() otherwise applies
	// to outbound provider requests (it blocks loopback addresses in
	// production) so the test's httptest.Server, which is loopback-only, is
	// reachable; this mirrors how the other connectivity provider tests that
	// exercise a real HTTP round trip are set up.
	conn := connectivity.New(s, testMasterKey(), connectivity.WithHTTPClient(http.DefaultClient), connectivity.WithPublicBaseURL("https://notify.example.com"))
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	// GET /v1/integrations/webpush/key: mint the org's VAPID keypair with no
	// other push provider configured for the scope.
	if _, _, err := conn.EnsureWebPushKeys(ctx, scope); err != nil {
		t.Fatalf("EnsureWebPushKeys: %v", err)
	}

	// Stub push service: any well-formed VAPID-signed POST succeeds.
	received := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		select {
		case received <- r:
		default:
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	// PUT /v1/subscribers/{id}/credentials, providerId "webpush": register one
	// device subscription with keys shaped so webpush-go's RFC8291 encryption
	// step succeeds (a valid uncompressed P-256 point + 16-byte auth secret).
	_, pub, err := vapidpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if _, err := s.WebPushSubscriptions().UpsertByEndpoint(ctx, scope, domain.WebPushSubscription{
		SubscriberID: "user_123",
		Endpoint:     srv.URL,
		P256dh:       pub,
		Auth:         base64.RawURLEncoding.EncodeToString(authSecret),
	}); err != nil {
		t.Fatalf("WebPushSubscriptions.UpsertByEndpoint: %v", err)
	}

	env := runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: scope.OrgID, EnvID: scope.EnvID},
		TransactionID:  "txn_push_real",
		WorkflowID:     "wf_1",
		NotificationID: "ntf_1",
		SubscriberID:   "user_123",
		Subscriber:     json.RawMessage(`{"subscriberId":"user_123","firstName":"Ada"}`),
		Payload:        json.RawMessage(`{"itemCount":3}`),
		Step: runenv.Step{
			StepID:          "P1",
			Channel:         "push",
			SubjectTemplate: "Hi {{subscriber.firstName}}",
			BodyTemplate:    "You have {{payload.itemCount}} items",
		},
	}
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	h := delivery.NewPush(s, conn)
	out, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_wp_real", StepID: "P1", Input: input})
	if err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	select {
	case <-received:
	default:
		t.Fatal("stub push server never received a request: HandlePush did not reach SendToSubscription")
	}

	var m domain.Message
	if uerr := json.Unmarshal(out, &m); uerr != nil {
		t.Fatalf("unmarshal handler output: %v", uerr)
	}
	if m.Status != "sent" {
		t.Errorf("Status = %q, want sent", m.Status)
	}
	if m.ProviderID != "webpush" {
		t.Errorf("ProviderID = %q, want webpush (ResolvePush must have resolved the minted row)", m.ProviderID)
	}
}
