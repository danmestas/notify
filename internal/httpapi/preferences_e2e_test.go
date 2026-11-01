package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// preferencesResponse mirrors the GET/PUT /v1/subscribers/{id}/preferences
// JSON shape for test decoding.
type preferencesResponse struct {
	Global    map[string]bool            `json:"global"`
	Workflows map[string]map[string]bool `json:"workflows"`
}

// setupPreferencesSuite spins up a complete stack including the preferences
// service, wired the same way delivery handlers enforce it (WithPreferences),
// and creates+publishes one in_app workflow "quarry-mention". It returns the
// server, auth header, scope, the authoring/preferences services, and store.
func setupPreferencesSuite(t *testing.T) (srv *httptest.Server, authHeader string, scope domain.Scope, au *authoring.Service, prefs *preferences.Service, aud *audience.Service, st *store.Store) {
	t.Helper()
	natsURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	ctx := context.Background()
	st, err = store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	prefs = preferences.New(st)
	eng.Handle("notify.send.in_app", delivery.New(st).WithPreferences(prefs).HandleInApp)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud = audience.New(st)
	au = authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)

	key, sc, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	scope = sc

	controls := json.RawMessage(`{"subject":"Hi","body":"hello"}`)
	if _, err := au.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Mention",
		TriggerIdentifier: "quarry-mention",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "in_app",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := au.Publish(ctx, scope, "quarry-mention"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv = httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity:    id,
		Audience:    aud,
		Trigger:     trg,
		Inbox:       inbox,
		Authoring:   au,
		Preferences: prefs,
	}))
	t.Cleanup(srv.Close)

	authHeader = "ApiKey " + key
	return srv, authHeader, scope, au, prefs, aud, st
}

// createPrefSubscriber POSTs a bare subscriber and returns its external
// subscriberId — the identifier the preferences route resolves {id} by (the
// same one PUT/DELETE .../credentials resolve subscriberId by), not the
// internal ULID id.
func createPrefSubscriber(t *testing.T, srv *httptest.Server, authHeader, subscriberID string) string {
	t.Helper()
	body := `{"subscriberId":"` + subscriberID + `"}`
	status, respBody := doPost(t, srv.URL+"/v1/subscribers", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, respBody)
	}
	var sub domain.Subscriber
	if err := json.Unmarshal([]byte(respBody), &sub); err != nil {
		t.Fatalf("decode subscriber: %v (body=%s)", err, respBody)
	}
	return sub.SubscriberID
}

// TestSubscriberPreferences_GetDefaults verifies a fresh subscriber with no
// stored preferences reports every channel enabled globally and no per-workflow
// overrides.
func TestSubscriberPreferences_GetDefaults(t *testing.T) {
	srv, authHeader, _, _, _, _, _ := setupPreferencesSuite(t)
	id := createPrefSubscriber(t, srv, authHeader, "sub-defaults")

	status, body := doGet(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET preferences: status=%d body=%s", status, body)
	}
	var resp preferencesResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	for _, ch := range []string{"in_app", "push", "email", "sms", "chat"} {
		if !resp.Global[ch] {
			t.Errorf("Global[%q] = %v, want true (default enabled)", ch, resp.Global[ch])
		}
	}
	if len(resp.Workflows) != 0 {
		t.Errorf("Workflows = %v, want empty", resp.Workflows)
	}
}

// TestSubscriberPreferences_PutThenGetRoundTrip verifies PUT persists the
// submitted global + per-workflow shape, keyed by trigger identifier, and GET
// reflects it back exactly.
func TestSubscriberPreferences_PutThenGetRoundTrip(t *testing.T) {
	srv, authHeader, _, _, _, _, _ := setupPreferencesSuite(t)
	id := createPrefSubscriber(t, srv, authHeader, "sub-roundtrip")

	putBody := `{"global":{"push":true,"email":false},"workflows":{"quarry-mention":{"push":false}}}`
	status, body := doPut(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader, putBody)
	if status != http.StatusOK {
		t.Fatalf("PUT preferences: status=%d body=%s", status, body)
	}

	status, body = doGet(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET preferences: status=%d body=%s", status, body)
	}
	var resp preferencesResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if resp.Global["push"] != true {
		t.Errorf("Global[push] = %v, want true", resp.Global["push"])
	}
	if resp.Global["email"] != false {
		t.Errorf("Global[email] = %v, want false", resp.Global["email"])
	}
	wf, ok := resp.Workflows["quarry-mention"]
	if !ok {
		t.Fatalf("Workflows[quarry-mention] missing; got %v", resp.Workflows)
	}
	if wf["push"] != false {
		t.Errorf("Workflows[quarry-mention][push] = %v, want false", wf["push"])
	}
	if _, ok := wf["email"]; ok {
		t.Errorf("Workflows[quarry-mention] should not carry inherited email key, got %v", wf)
	}
}

// TestSubscriberPreferences_PutPartialUpdatePreservesOtherKeys verifies a
// second PUT that touches only one channel does not clobber previously-set
// keys at either the global or per-workflow level.
func TestSubscriberPreferences_PutPartialUpdatePreservesOtherKeys(t *testing.T) {
	srv, authHeader, _, _, _, _, _ := setupPreferencesSuite(t)
	id := createPrefSubscriber(t, srv, authHeader, "sub-partial")

	first := `{"global":{"push":false,"email":false},"workflows":{"quarry-mention":{"push":false,"sms":false}}}`
	if status, body := doPut(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader, first); status != http.StatusOK {
		t.Fatalf("PUT #1: status=%d body=%s", status, body)
	}

	second := `{"global":{"email":true},"workflows":{"quarry-mention":{"sms":true}}}`
	if status, body := doPut(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader, second); status != http.StatusOK {
		t.Fatalf("PUT #2: status=%d body=%s", status, body)
	}

	status, body := doGet(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET preferences: status=%d body=%s", status, body)
	}
	var resp preferencesResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	// push:false from PUT #1 must survive PUT #2, which only touched email.
	if resp.Global["push"] != false {
		t.Errorf("Global[push] = %v, want false (preserved from PUT #1)", resp.Global["push"])
	}
	if resp.Global["email"] != true {
		t.Errorf("Global[email] = %v, want true (updated by PUT #2)", resp.Global["email"])
	}
	wf := resp.Workflows["quarry-mention"]
	if wf["push"] != false {
		t.Errorf("Workflows[quarry-mention][push] = %v, want false (preserved from PUT #1)", wf["push"])
	}
	if wf["sms"] != true {
		t.Errorf("Workflows[quarry-mention][sms] = %v, want true (updated by PUT #2)", wf["sms"])
	}
}

// TestSubscriberPreferences_PutUnknownTriggerIdentifier verifies an unknown
// workflow key in the request body fails closed with 422 and lists it.
func TestSubscriberPreferences_PutUnknownTriggerIdentifier(t *testing.T) {
	srv, authHeader, _, _, _, _, _ := setupPreferencesSuite(t)
	id := createPrefSubscriber(t, srv, authHeader, "sub-unknown-wf")

	body := `{"workflows":{"does-not-exist":{"push":false}}}`
	status, respBody := doPut(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader, body)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("PUT preferences: status=%d body=%s, want 422", status, respBody)
	}
	if !strings.Contains(respBody, "does-not-exist") {
		t.Errorf("422 body does not mention the unknown identifier: %s", respBody)
	}
}

// TestSubscriberPreferences_PutUnknownChannel verifies an unknown channel name
// fails closed with 422.
func TestSubscriberPreferences_PutUnknownChannel(t *testing.T) {
	srv, authHeader, _, _, _, _, _ := setupPreferencesSuite(t)
	id := createPrefSubscriber(t, srv, authHeader, "sub-unknown-ch")

	body := `{"global":{"carrier_pigeon":false}}`
	status, respBody := doPut(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader, body)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("PUT preferences: status=%d body=%s, want 422", status, respBody)
	}
}

// TestSubscriberPreferences_UnknownSubscriber verifies GET/PUT against a
// nonexistent subscriber id answers 404.
func TestSubscriberPreferences_UnknownSubscriber(t *testing.T) {
	srv, authHeader, _, _, _, _, _ := setupPreferencesSuite(t)

	status, body := doGet(t, srv.URL+"/v1/subscribers/does-not-exist/preferences", authHeader)
	if status != http.StatusNotFound {
		t.Fatalf("GET preferences (unknown subscriber): status=%d body=%s, want 404", status, body)
	}

	status, body = doPut(t, srv.URL+"/v1/subscribers/does-not-exist/preferences", authHeader, `{}`)
	if status != http.StatusNotFound {
		t.Fatalf("PUT preferences (unknown subscriber): status=%d body=%s, want 404", status, body)
	}
}

// TestSubscriberPreferences_ResolvesByExternalSubscriberID verifies {id} on
// the preferences route resolves the same external subscriberId that PUT
// /v1/subscribers/{subscriberId}/credentials and DELETE
// .../credentials/webpush resolve by — not the internal ULID row id — so a
// single client-held identifier works for triggering, credentials, and
// preferences alike.
func TestSubscriberPreferences_ResolvesByExternalSubscriberID(t *testing.T) {
	srv, authHeader, _, _, _, _, _ := setupPreferencesSuite(t)

	body := `{"subscriberId":"sub-external-id"}`
	status, respBody := doPost(t, srv.URL+"/v1/subscribers", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, respBody)
	}
	var sub domain.Subscriber
	if err := json.Unmarshal([]byte(respBody), &sub); err != nil {
		t.Fatalf("decode subscriber: %v (body=%s)", err, respBody)
	}

	// The external subscriberId resolves.
	status, body2 := doGet(t, srv.URL+"/v1/subscribers/"+sub.SubscriberID+"/preferences", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET preferences by external subscriberId: status=%d body=%s", status, body2)
	}

	// The internal ULID id does not — it is a different identifier space,
	// same as the credentials routes.
	status, body2 = doGet(t, srv.URL+"/v1/subscribers/"+sub.ID+"/preferences", authHeader)
	if status != http.StatusNotFound {
		t.Fatalf("GET preferences by internal ULID id: status=%d body=%s, want 404", status, body2)
	}
}

// TestSubscriberPreferences_TenancyIsolation verifies one org cannot read or
// write another org's subscriber preferences: cross-org GET/PUT both 404.
func TestSubscriberPreferences_TenancyIsolation(t *testing.T) {
	srv, authHeaderA, _, _, _, _, st := setupPreferencesSuite(t)
	idA := createPrefSubscriber(t, srv, authHeaderA, "sub-tenancy")

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	keyB, _, err := id.Bootstrap(context.Background(), "Other Org", "dev")
	if err != nil {
		t.Fatalf("Bootstrap org B: %v", err)
	}
	authHeaderB := "ApiKey " + keyB

	status, body := doGet(t, srv.URL+"/v1/subscribers/"+idA+"/preferences", authHeaderB)
	if status != http.StatusNotFound {
		t.Fatalf("GET cross-org: status=%d body=%s, want 404", status, body)
	}
	status, body = doPut(t, srv.URL+"/v1/subscribers/"+idA+"/preferences", authHeaderB, `{"global":{"push":false}}`)
	if status != http.StatusNotFound {
		t.Fatalf("PUT cross-org: status=%d body=%s, want 404", status, body)
	}
}

// TestSubscriberPreferences_EnforcedByDeliveryHandler is the enforcement
// proof: a subscriber with push:false for a workflow via the preferences
// route causes the in_app delivery handler to record the message as
// "skipped" when the global in_app channel is off, mirroring
// preference_skip_e2e_test.go but driven entirely through the HTTP route.
func TestSubscriberPreferences_EnforcedByDeliveryHandler(t *testing.T) {
	srv, authHeader, _, _, _, _, _ := setupPreferencesSuite(t)
	id := createPrefSubscriber(t, srv, authHeader, "sub-enforced")

	putBody := `{"global":{"in_app":false}}`
	if status, body := doPut(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader, putBody); status != http.StatusOK {
		t.Fatalf("PUT preferences: status=%d body=%s", status, body)
	}

	trigBody := `{"name":"quarry-mention","to":{"subscriberId":"sub-enforced"},"payload":{},"transactionId":"pref-tx-1"}`
	status, respJSON := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, respJSON)
	}

	msg := pollMessage(t, srv.URL, authHeader, "sub-enforced")
	if msg.Status != "skipped" {
		t.Errorf("Status = %q, want %q (disabled via the preferences HTTP route)", msg.Status, "skipped")
	}
}

// TestSubscriberPreferences_ConcurrentPartialPutsDoNotLoseUpdates proves the
// PUT handler's read-merge-write is race-safe: N goroutines each PUT a
// distinct global channel key concurrently for the same subscriber, and the
// final GET must reflect all N keys. Each partial PUT only reads existing
// rows and rewrites the whole blob (there is no store-level merge), so an
// unguarded handler drops whichever writer's read predates another writer's
// write — this test fails against that unguarded code.
func TestSubscriberPreferences_ConcurrentPartialPutsDoNotLoseUpdates(t *testing.T) {
	srv, authHeader, _, _, _, _, _ := setupPreferencesSuite(t)
	id := createPrefSubscriber(t, srv, authHeader, "sub-concurrent")

	channels := []string{"in_app", "push", "email", "sms", "chat"}

	var wg sync.WaitGroup
	for _, ch := range channels {
		wg.Add(1)
		go func(ch string) {
			defer wg.Done()
			body := `{"global":{"` + ch + `":false}}`
			status, respBody := doPut(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader, body)
			if status != http.StatusOK {
				t.Errorf("PUT preferences (%s): status=%d body=%s", ch, status, respBody)
			}
		}(ch)
	}
	wg.Wait()

	status, body := doGet(t, srv.URL+"/v1/subscribers/"+id+"/preferences", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET preferences: status=%d body=%s", status, body)
	}
	var resp preferencesResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	for _, ch := range channels {
		if resp.Global[ch] != false {
			t.Errorf("Global[%q] = %v, want false — concurrent PUT for this channel was lost", ch, resp.Global[ch])
		}
	}
}
