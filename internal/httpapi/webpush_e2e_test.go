package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
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
	srv, authHeader, scope, id, _ := startWebPushServerWithStore(t)
	return srv, authHeader, scope, id
}

// startWebPushServerWithStore is startWebPushServer plus the underlying
// *store.Store, for tests that need to assert directly against a repo
// (e.g. WebPushSubscriptions().ListBySubscriber) rather than through the
// HTTP surface alone.
func startWebPushServerWithStore(t *testing.T) (*httptest.Server, string, domain.Scope, *identity.Service, *store.Store) {
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
	conn := connectivity.New(st, masterKey, connectivity.WithPublicBaseURL("https://notify.example.com"))
	trg := trigger.New(aud, st, nil)
	inbox := inboxapi.New(st)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Connectivity: conn}))
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key, scope, id, st
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

// TestDeleteSubscriberWebPushCredentials_RemovesOnlyThatDevice proves a
// per-device delete removes exactly the targeted endpoint's row and leaves
// a second device registered to the same subscriber untouched.
func TestDeleteSubscriberWebPushCredentials_RemovesOnlyThatDevice(t *testing.T) {
	srv, authHeader, scope, _, st := startWebPushServerWithStore(t)
	ctx := context.Background()

	subBody := `{"subscriberId":"user_wp3"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	deviceA := "https://fcm.googleapis.com/fcm/send/device-a3"
	deviceB := "https://fcm.googleapis.com/fcm/send/device-b3"
	for _, endpoint := range []string{deviceA, deviceB} {
		credBody := `{"providerId":"webpush","credentials":{"endpoint":"` + endpoint + `","keys":{"p256dh":"p256dh","auth":"auth"}}}`
		if status, body := doPut(t, srv.URL+"/v1/subscribers/user_wp3/credentials", authHeader, credBody); status != http.StatusNoContent {
			t.Fatalf("PUT credentials (%s): status=%d body=%s", endpoint, status, body)
		}
	}

	before, err := st.WebPushSubscriptions().ListBySubscriber(ctx, scope, "user_wp3")
	if err != nil {
		t.Fatalf("ListBySubscriber (before delete): %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("before delete: got %d rows, want 2 (%+v)", len(before), before)
	}

	status, body := doDelete(t, srv.URL+"/v1/subscribers/user_wp3/credentials/webpush?endpoint="+url.QueryEscape(deviceA), authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE webpush credentials (device a): status=%d body=%s", status, body)
	}

	after, err := st.WebPushSubscriptions().ListBySubscriber(ctx, scope, "user_wp3")
	if err != nil {
		t.Fatalf("ListBySubscriber (after delete): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("after delete: got %d rows, want 1 (%+v)", len(after), after)
	}
	if after[0].Endpoint != deviceB {
		t.Errorf("after delete: remaining row endpoint = %q, want %q (device B survived)", after[0].Endpoint, deviceB)
	}

	// Re-registering device A (now deleted) must succeed as a fresh insert,
	// proving the row is actually gone rather than merely hidden — the row
	// count goes back to 2, not clobbering device B's row.
	credBody := `{"providerId":"webpush","credentials":{"endpoint":"` + deviceA + `","keys":{"p256dh":"p256dh-again","auth":"auth-again"}}}`
	if status, body := doPut(t, srv.URL+"/v1/subscribers/user_wp3/credentials", authHeader, credBody); status != http.StatusNoContent {
		t.Fatalf("PUT credentials (re-register device a after delete): status=%d body=%s", status, body)
	}
	reRegistered, err := st.WebPushSubscriptions().ListBySubscriber(ctx, scope, "user_wp3")
	if err != nil {
		t.Fatalf("ListBySubscriber (after re-register): %v", err)
	}
	if len(reRegistered) != 2 {
		t.Fatalf("after re-register: got %d rows, want 2 (%+v)", len(reRegistered), reRegistered)
	}
}

// TestDeleteSubscriberWebPushCredentials_NotFound_404 proves deleting an
// endpoint that was never registered for the subscriber 404s.
func TestDeleteSubscriberWebPushCredentials_NotFound_404(t *testing.T) {
	srv, authHeader, _, _ := startWebPushServer(t)

	subBody := `{"subscriberId":"user_wp4"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	status, body := doDelete(t, srv.URL+"/v1/subscribers/user_wp4/credentials/webpush?endpoint="+url.QueryEscape("https://fcm.googleapis.com/fcm/send/never-registered"), authHeader)
	if status != http.StatusNotFound {
		t.Fatalf("DELETE webpush credentials (never registered): status=%d body=%s, want 404", status, body)
	}
}

// TestDeleteSubscriberWebPushCredentials_MissingEndpoint_400 proves an
// absent or empty endpoint query param is rejected before any store call.
func TestDeleteSubscriberWebPushCredentials_MissingEndpoint_400(t *testing.T) {
	srv, authHeader, _, _ := startWebPushServer(t)

	subBody := `{"subscriberId":"user_wp5"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	status, body := doDelete(t, srv.URL+"/v1/subscribers/user_wp5/credentials/webpush", authHeader)
	if status != http.StatusBadRequest {
		t.Fatalf("DELETE webpush credentials (missing endpoint): status=%d body=%s, want 400", status, body)
	}

	status, body = doDelete(t, srv.URL+"/v1/subscribers/user_wp5/credentials/webpush?endpoint=", authHeader)
	if status != http.StatusBadRequest {
		t.Fatalf("DELETE webpush credentials (empty endpoint): status=%d body=%s, want 400", status, body)
	}
}

// TestDeleteSubscriberWebPushCredentials_TenancyIsolation proves a device
// registered under one org is invisible to another org's delete — a
// cross-tenant delete attempt 404s rather than removing the row.
func TestDeleteSubscriberWebPushCredentials_TenancyIsolation(t *testing.T) {
	srv, authHeaderA, _, id := startWebPushServer(t)

	ctx := context.Background()
	keyB, _, err := id.Bootstrap(ctx, "OtherCorp", "prod")
	if err != nil {
		t.Fatalf("Bootstrap second org: %v", err)
	}
	authHeaderB := "ApiKey " + keyB

	subBody := `{"subscriberId":"user_wp6"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeaderA, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers (org A): status=%d body=%s", status, body)
	}
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeaderB, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers (org B): status=%d body=%s", status, body)
	}

	endpoint := "https://fcm.googleapis.com/fcm/send/device-tenancy"
	credBody := `{"providerId":"webpush","credentials":{"endpoint":"` + endpoint + `","keys":{"p256dh":"p256dh","auth":"auth"}}}`
	if status, body := doPut(t, srv.URL+"/v1/subscribers/user_wp6/credentials", authHeaderA, credBody); status != http.StatusNoContent {
		t.Fatalf("PUT credentials (org A): status=%d body=%s", status, body)
	}

	// Org B has a same-id subscriber but never registered this endpoint —
	// its delete must 404, not reach into org A's row.
	status, body := doDelete(t, srv.URL+"/v1/subscribers/user_wp6/credentials/webpush?endpoint="+url.QueryEscape(endpoint), authHeaderB)
	if status != http.StatusNotFound {
		t.Fatalf("DELETE webpush credentials (org B, cross-tenant): status=%d body=%s, want 404", status, body)
	}

	// Org A's delete of the same endpoint still succeeds — proves the 404
	// above was tenancy isolation, not the row being gone.
	status, body = doDelete(t, srv.URL+"/v1/subscribers/user_wp6/credentials/webpush?endpoint="+url.QueryEscape(endpoint), authHeaderA)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE webpush credentials (org A, own endpoint): status=%d body=%s", status, body)
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

// TestWebPushSubject_GetDefaultPutOverrideRefuseLocalhost is #358 over HTTP:
// the key endpoint reports the subject pushes are signed with, PUT sets a
// per-environment one, and a localhost contact is refused with 400.
func TestWebPushSubject_GetDefaultPutOverrideRefuseLocalhost(t *testing.T) {
	srv, authHeader, _, _ := startWebPushServer(t)

	var key struct {
		PublicKey      string `json:"publicKey"`
		Subject        string `json:"subject"`
		SubjectDefault bool   `json:"subjectDefault"`
	}
	status, body := doGet(t, srv.URL+"/v1/integrations/webpush/key", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET key = %d (%s)", status, body)
	}
	if err := json.Unmarshal([]byte(body), &key); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if key.Subject != "https://notify.example.com" || !key.SubjectDefault {
		t.Fatalf("default subject = %+v", key)
	}

	status, body = doPut(t, srv.URL+"/v1/integrations/webpush/subject", authHeader, `{"subject":"mailto:ops@example.org"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT subject = %d (%s)", status, body)
	}
	if err := json.Unmarshal([]byte(body), &key); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if key.Subject != "mailto:ops@example.org" || key.SubjectDefault || key.PublicKey == "" {
		t.Fatalf("after PUT = %+v", key)
	}

	status, body = doPut(t, srv.URL+"/v1/integrations/webpush/subject", authHeader, `{"subject":"mailto:notify@localhost"}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "bad_request") {
		t.Fatalf("localhost PUT = %d (%s), want 400 bad_request", status, body)
	}

	status, _ = doPut(t, srv.URL+"/v1/integrations/webpush/subject", "", `{"subject":"mailto:ops@example.org"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PUT = %d, want 401", status)
	}
}
