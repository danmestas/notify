package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/credverify"
	"github.com/danmestas/notify/internal/deliveryhealth"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// noEngine satisfies credverify.Engine; these tests never start a run.
type noEngine struct{}

func (noEngine) RegisterWorkflow(context.Context, engine.WorkflowDef) error { return nil }
func (noEngine) StartRun(context.Context, string, json.RawMessage) (string, error) {
	return "", fmt.Errorf("no engine")
}

// healthServer is a notify API wired with delivery health and a credential
// checker, plus a fake ntfy server whose probe answer the test controls.
type healthServer struct {
	srv     *httptest.Server
	auth    string
	otherAu string
	scope   domain.Scope
	st      *store.Store
	tracker *deliveryhealth.Tracker
	probe   *atomic.Int32 // HTTP status the fake ntfy answers with
	hits    *atomic.Int32 // requests the fake ntfy has served
	ntfyURL string
}

func startHealthServer(t *testing.T) *healthServer {
	t.Helper()
	// Most tests flip the provider's answer between checks, so they run
	// unthrottled; the throttle has its own test.
	return startHealthServerWith(t, 0)
}

func startHealthServerWith(t *testing.T, minInterval time.Duration) *healthServer {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var status, hits atomic.Int32
	status.Store(http.StatusOK)
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		code := int(status.Load())
		w.WriteHeader(code)
		if code == http.StatusInternalServerError {
			// A provider that echoes the credential it was sent.
			_, _ = fmt.Fprintf(w, "credentials rejected: bad token %q", strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}
	}))
	t.Cleanup(ntfy.Close)

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	conn := connectivity.New(st, masterKey, connectivity.WithHTTPClient(&http.Client{}))
	tracker := deliveryhealth.New(st, deliveryhealth.DefaultSettings(), nil)
	chk := credverify.New(noEngine{}, conn).WithObserver(tracker).WithMinCheckInterval(minInterval)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	otherKey, _, _, err := id.CreateEnvironment(ctx, scope.OrgID, "Other", "development")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil), Inbox: inboxapi.New(st),
		Connectivity: conn, Authoring: authoring.New(st, nil), CredChecker: chk, Store: st,
	}))
	t.Cleanup(srv.Close)
	return &healthServer{
		srv: srv, auth: "ApiKey " + key, otherAu: "ApiKey " + otherKey, scope: scope,
		st: st, tracker: tracker, probe: &status, hits: &hits, ntfyURL: ntfy.URL,
	}
}

// createNtfy creates an ntfy integration pointing at the fake server and
// returns its id.
func (h *healthServer) createNtfy(t *testing.T, identifier string) string {
	t.Helper()
	body := fmt.Sprintf(`{"providerId":"ntfy","identifier":%q,"credentials":{"base_url":%q,"token":"tk_super_secret"}}`, identifier, h.ntfyURL)
	code, resp := doPost(t, h.srv.URL+"/v1/integrations", h.auth, body)
	if code != http.StatusCreated {
		t.Fatalf("create integration: status=%d body=%s", code, resp)
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(resp), &got); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	return got.ID
}

type healthJSON struct {
	State            string `json:"state"`
	Since            string `json:"since"`
	LastErrorClass   string `json:"lastErrorClass"`
	LastErrorMessage string `json:"lastErrorMessage"`
	LastCheckedAt    string `json:"lastCheckedAt"`
	LastCheckOK      bool   `json:"lastCheckOk"`
	LastCheckDetail  string `json:"lastCheckDetail"`
}

func getHealth(t *testing.T, h *healthServer, id string) healthJSON {
	t.Helper()
	code, body := doGet(t, h.srv.URL+"/v1/integrations/"+id, h.auth)
	if code != http.StatusOK {
		t.Fatalf("GET integration: status=%d body=%s", code, body)
	}
	var got struct {
		Health *healthJSON `json:"health"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Health == nil {
		t.Fatalf("no health in %s (err=%v)", body, err)
	}
	return *got.Health
}

func TestIntegrationHealth_UnverifiedBeforeAnyCheck(t *testing.T) {
	h := startHealthServer(t)
	id := h.createNtfy(t, "ntfy-new")

	got := getHealth(t, h, id)
	if got.State != "unverified" || got.LastErrorMessage != "" || got.LastCheckedAt != "" {
		t.Fatalf("health = %+v, want bare unverified", got)
	}

	code, body := doGet(t, h.srv.URL+"/v1/integrations", h.auth)
	if code != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", code, body)
	}
	var list []struct {
		ID     string      `json:"id"`
		Health *healthJSON `json:"health"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].ID != id || list[0].Health == nil || list[0].Health.State != "unverified" {
		t.Fatalf("list = %s, want one unverified integration with health", body)
	}
}

func TestIntegrationHealth_VerifyRunsTheCheckAndUpdatesState(t *testing.T) {
	h := startHealthServer(t)
	id := h.createNtfy(t, "ntfy-flip")

	// Rejected credentials: the check runs now and the integration is failing.
	h.probe.Store(http.StatusUnauthorized)
	code, body := doPost(t, h.srv.URL+"/v1/integrations/"+id+"/verify", h.auth, "")
	if code != http.StatusOK {
		t.Fatalf("verify: status=%d body=%s", code, body)
	}
	var failing healthJSON
	if err := json.Unmarshal([]byte(body), &failing); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if failing.State != "failing" || failing.LastErrorClass != "auth" ||
		!strings.Contains(failing.LastErrorMessage, "rejected") || failing.LastCheckedAt == "" || failing.Since == "" {
		t.Fatalf("verify health = %+v, want failing/auth with message, since and lastCheckedAt", failing)
	}
	if failing.LastCheckOK || !strings.Contains(failing.LastCheckDetail, "rejected") {
		t.Fatalf("verify health = %+v, want the failed check and its reason", failing)
	}
	if got := getHealth(t, h, id); got != failing {
		t.Fatalf("GET health = %+v, want what verify returned %+v", got, failing)
	}

	// Credentials fixed: verifying again clears failing.
	h.probe.Store(http.StatusOK)
	code, body = doPost(t, h.srv.URL+"/v1/integrations/"+id+"/verify", h.auth, "")
	if code != http.StatusOK {
		t.Fatalf("verify again: status=%d body=%s", code, body)
	}
	var healthy healthJSON
	if err := json.Unmarshal([]byte(body), &healthy); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if healthy.State != "healthy" || healthy.LastCheckedAt == "" || !healthy.LastCheckOK || healthy.LastCheckDetail != "" {
		t.Fatalf("verify health = %+v, want healthy", healthy)
	}
}

func TestIntegrationHealth_NoSecretInAnyHealthField(t *testing.T) {
	h := startHealthServer(t)
	id := h.createNtfy(t, "ntfy-secret")
	h.probe.Store(http.StatusInternalServerError) // the fake echoes the token in its error body

	_, verifyBody := doPost(t, h.srv.URL+"/v1/integrations/"+id+"/verify", h.auth, "")
	_, getBody := doGet(t, h.srv.URL+"/v1/integrations/"+id, h.auth)
	_, listBody := doGet(t, h.srv.URL+"/v1/integrations", h.auth)
	for name, body := range map[string]string{"verify": verifyBody, "get": getBody, "list": listBody} {
		if strings.Contains(body, "tk_super_secret") {
			t.Errorf("%s response leaks the token: %s", name, body)
		}
	}
	// The provider's message still reaches the operator, minus the secret.
	var got healthJSON
	if err := json.Unmarshal([]byte(verifyBody), &got); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if got.State != "failing" || !strings.Contains(got.LastErrorMessage, "[redacted]") {
		t.Fatalf("health = %+v, want failing with the token redacted in the message", got)
	}
	// And the stored row never held it.
	row, err := h.st.IntegrationHealth().Get(context.Background(), h.scope, id)
	if err != nil {
		t.Fatalf("health row: %v", err)
	}
	if strings.Contains(row.LastErrorMessage, "tk_super_secret") {
		t.Fatalf("stored last error leaks the token: %q", row.LastErrorMessage)
	}
}

func TestIntegrationHealth_VerifyIsThrottledPerIntegration(t *testing.T) {
	h := startHealthServerWith(t, time.Minute)
	id := h.createNtfy(t, "ntfy-throttle")
	other := h.createNtfy(t, "ntfy-other")
	h.probe.Store(http.StatusUnauthorized)

	var first healthJSON
	for i := 0; i < 5; i++ {
		code, body := doPost(t, h.srv.URL+"/v1/integrations/"+id+"/verify", h.auth, "")
		if code != http.StatusOK {
			t.Fatalf("verify %d: status=%d body=%s", i, code, body)
		}
		var got healthJSON
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("verify %d = %+v, want the stored health %+v", i, got, first)
		}
	}
	if n := hitsOf(h); n != 1 {
		t.Fatalf("provider probed %d times by 5 verifies, want 1", n)
	}
	// Another integration is not held back by this one's interval.
	if code, _ := doPost(t, h.srv.URL+"/v1/integrations/"+other+"/verify", h.auth, ""); code != http.StatusOK {
		t.Fatalf("verify of another integration: status=%d", code)
	}
	if n := hitsOf(h); n != 2 {
		t.Fatalf("provider probed %d times, want 2", n)
	}
}

func hitsOf(h *healthServer) int32 { return h.hits.Load() }

func TestIntegrationHealth_OutOfScopeIs404(t *testing.T) {
	h := startHealthServer(t)
	id := h.createNtfy(t, "ntfy-mine")

	code, body := doPost(t, h.srv.URL+"/v1/integrations/"+id+"/verify", h.otherAu, "")
	if code != http.StatusNotFound {
		t.Fatalf("verify from another environment: status=%d body=%s, want 404", code, body)
	}
	if code, _ = doPost(t, h.srv.URL+"/v1/integrations/nope/verify", h.auth, ""); code != http.StatusNotFound {
		t.Fatalf("verify of a missing id: status=%d, want 404", code)
	}
	// Nothing was recorded by the refused calls.
	if got := getHealth(t, h, id); got.State != "unverified" || got.LastCheckedAt != "" {
		t.Fatalf("health after refused verify = %+v, want untouched", got)
	}
	// The other environment sees none of this integration or its health.
	if _, listBody := doGet(t, h.srv.URL+"/v1/integrations", h.otherAu); strings.TrimSpace(listBody) != "[]" {
		t.Fatalf("other environment lists %s, want []", listBody)
	}
}

func TestIntegrationHealth_VerifyRequiresAuth(t *testing.T) {
	h := startHealthServer(t)
	if code, _ := doPost(t, h.srv.URL+"/v1/integrations/x/verify", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("verify without a key: status=%d, want 401", code)
	}
}
