package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// startWebPushServer boots a minimal HTTP API — identity, audience, and
// connectivity, no engine — sufficient for the webpush key-mint and
// per-device credentials routes exercised here. Returns the server, a
// bootstrapped API key header, the scope, and the identity service (so
// callers can Bootstrap a second org for scope-isolation checks).
func startWebPushServer(t *testing.T) (*httptest.Server, string, domain.Scope, *identity.Service) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	conn := connectivity.New(st, masterKey)
	trg := trigger.New(aud, st, nil)
	inbox := inboxapi.New(st)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Connectivity: conn}))
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key, scope, id
}

// TestGetWebPushKey_MintsOnFirstUse proves the endpoint mints a VAPID keypair
// on first call and returns it as a base64url publicKey, then returns the
// SAME key on a second call.
func TestGetWebPushKey_MintsOnFirstUse(t *testing.T) {
	srv, authHeader, _, _ := startWebPushServer(t)

	status, body := doGet(t, srv.URL+"/v1/integrations/webpush/key", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/integrations/webpush/key: status=%d body=%s", status, body)
	}
	var first struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if first.PublicKey == "" {
		t.Fatal("publicKey is empty")
	}

	status, body = doGet(t, srv.URL+"/v1/integrations/webpush/key", authHeader)
	if status != http.StatusOK {
		t.Fatalf("second GET: status=%d body=%s", status, body)
	}
	var second struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(body), &second); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if second.PublicKey != first.PublicKey {
		t.Errorf("second call returned a different key: got %q, want %q", second.PublicKey, first.PublicKey)
	}
}

// TestGetWebPushKey_AuthGuard verifies the route sits behind auth.
func TestGetWebPushKey_AuthGuard(t *testing.T) {
	srv, _, _, _ := startWebPushServer(t)

	status, _ := doGet(t, srv.URL+"/v1/integrations/webpush/key", "")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

// TestPutSubscriberCredentials_WebPush_UpsertByEndpoint proves the webpush
// branch accepts a browser PushSubscription payload and that re-registering
// the same endpoint (e.g. a service worker refresh) upserts rather than
// erroring or duplicating. The two-devices-two-rows and same-endpoint-one-row
// assertions are covered at the store layer
// (internal/store/webpush_subscription_repo_test.go); this test is the HTTP
// wire-shape proof.
func TestPutSubscriberCredentials_WebPush_UpsertByEndpoint(t *testing.T) {
	srv, authHeader, _, _ := startWebPushServer(t)

	subBody := `{"subscriberId":"user_wp","email":"wp@acme.io"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	credBody := `{"providerId":"webpush","credentials":{"endpoint":"https://fcm.googleapis.com/fcm/send/device-a","keys":{"p256dh":"p256dh-a","auth":"auth-a"}}}`
	status, body := doPut(t, srv.URL+"/v1/subscribers/user_wp/credentials", authHeader, credBody)
	if status != http.StatusNoContent {
		t.Fatalf("PUT credentials (webpush, device a): status=%d body=%s", status, body)
	}

	// Re-registering the same endpoint (e.g. a service worker refresh) must
	// upsert in place, not error and not duplicate.
	credBody = `{"providerId":"webpush","credentials":{"endpoint":"https://fcm.googleapis.com/fcm/send/device-a","keys":{"p256dh":"p256dh-a-refreshed","auth":"auth-a-refreshed"}}}`
	status, body = doPut(t, srv.URL+"/v1/subscribers/user_wp/credentials", authHeader, credBody)
	if status != http.StatusNoContent {
		t.Fatalf("PUT credentials (webpush, re-register device a): status=%d body=%s", status, body)
	}
}

// TestPutSubscriberCredentials_WebPush_MissingFields_400 proves malformed
// PushSubscription JSON is rejected with 400, not a 500 or a silently
// half-written row.
func TestPutSubscriberCredentials_WebPush_MissingFields_400(t *testing.T) {
	srv, authHeader, _, _ := startWebPushServer(t)

	subBody := `{"subscriberId":"user_wp2"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	credBody := `{"providerId":"webpush","credentials":{"endpoint":""}}`
	status, body := doPut(t, srv.URL+"/v1/subscribers/user_wp2/credentials", authHeader, credBody)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT credentials (webpush, missing fields): status=%d body=%s, want 400", status, body)
	}
}

// TestGetWebPushKey_ScopeIsolation proves two orgs mint independent VAPID
// keypairs and never see each other's key.
func TestGetWebPushKey_ScopeIsolation(t *testing.T) {
	srv, authHeaderA, _, id := startWebPushServer(t)

	ctx := context.Background()
	keyB, _, err := id.Bootstrap(ctx, "OtherCorp", "prod")
	if err != nil {
		t.Fatalf("Bootstrap second org: %v", err)
	}
	authHeaderB := "ApiKey " + keyB

	_, bodyA := doGet(t, srv.URL+"/v1/integrations/webpush/key", authHeaderA)
	_, bodyB := doGet(t, srv.URL+"/v1/integrations/webpush/key", authHeaderB)

	var pubA, pubB struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(bodyA), &pubA); err != nil {
		t.Fatalf("decode A: %v", err)
	}
	if err := json.Unmarshal([]byte(bodyB), &pubB); err != nil {
		t.Fatalf("decode B: %v", err)
	}
	if pubA.PublicKey == pubB.PublicKey {
		t.Errorf("two orgs minted the same VAPID public key: %q", pubA.PublicKey)
	}
}
