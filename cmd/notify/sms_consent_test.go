package main

// Methodology: smsConsentTo is driven with real flags against a temp SQLite
// file that bootstrapTo provisioned. Its stdout is parsed as the state it
// reports, its stderr checked for the change log, and the key's permission is
// proven end to end by attesting through the real HTTP API, served from a
// second store opened on the same file the command writes.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/phoneverify"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

const testPolicyURL = "https://shop.example/privacy"

type consentTenant struct {
	db     string
	apiKey string
	scope  domain.Scope
}

func newConsentTenant(t *testing.T) consentTenant {
	t.Helper()
	db := filepath.Join(t.TempDir(), "n.db")
	var out bytes.Buffer
	if err := bootstrapTo(&out, bootstrapArgs(db)); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var got struct{ APIKey, OrgID, EnvID string }
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("bootstrap output: %q: %v", out.String(), err)
	}
	return consentTenant{db: db, apiKey: got.APIKey, scope: domain.Scope{OrgID: got.OrgID, EnvID: got.EnvID}}
}

// consentState mirrors the JSON the command prints, by its literal keys.
type consentState struct {
	OrgID     string `json:"orgId"`
	EnvID     string `json:"envId"`
	Enabled   bool   `json:"enabled"`
	PolicyURL string `json:"policyUrl"`
	Keys      []struct {
		ID               string `json:"id"`
		AttestSMSConsent bool   `json:"attestSmsConsent"`
	} `json:"keys"`
}

// run invokes the command for the tenant with extra flags, returning the
// parsed state, the log written to stderr, and the error.
func (c consentTenant) run(t *testing.T, extra ...string) (consentState, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"-db", c.db, "-org", "Acme", "-env", "production"}, extra...)
	err := smsConsentTo(&stdout, &stderr, args)
	var st consentState
	if err == nil {
		if jerr := json.Unmarshal(stdout.Bytes(), &st); jerr != nil {
			t.Fatalf("stdout is not the state JSON: %q: %v", stdout.String(), jerr)
		}
	}
	return st, stderr.String(), err
}

func (c consentTenant) mustRun(t *testing.T, extra ...string) (consentState, string) {
	t.Helper()
	st, log, err := c.run(t, extra...)
	if err != nil {
		t.Fatalf("sms-consent %v: %v", extra, err)
	}
	return st, log
}

func TestSMSConsent_ShowsStateAndKeyIDsNeverTheKey(t *testing.T) {
	c := newConsentTenant(t)
	var stdout, stderr bytes.Buffer
	if err := smsConsentTo(&stdout, &stderr, []string{"-db", c.db, "-org", "Acme", "-env", "production"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	if strings.Contains(stdout.String()+stderr.String(), c.apiKey) {
		t.Fatalf("output leaks the API key: %q", stdout.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("stdout: %v", err)
	}
	for _, k := range []string{"orgId", "envId", "enabled", "policyUrl", "keys"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("state lacks %q: %s", k, stdout.String())
		}
	}
	st, _ := c.mustRun(t)
	if st.OrgID != c.scope.OrgID || st.EnvID != c.scope.EnvID || st.Enabled || st.PolicyURL != "" {
		t.Fatalf("fresh state = %+v", st)
	}
	if len(st.Keys) != 1 || st.Keys[0].ID != keyHash(t, c) || st.Keys[0].AttestSMSConsent {
		t.Fatalf("keys = %+v, want the one key's hash without the permission", st.Keys)
	}
}

func TestSMSConsent_EnableAndDisable(t *testing.T) {
	c := newConsentTenant(t)

	for _, bad := range [][]string{
		{"-enable"},
		{"-enable", "-policy-url", "http://shop.example/privacy"},
		{"-enable", "-policy-url", "not a url"},
		{"-enable", "-disable", "-policy-url", testPolicyURL},
	} {
		if _, _, err := c.run(t, bad...); err == nil {
			t.Errorf("sms-consent %v: want an error", bad)
		}
	}
	if st, _ := c.mustRun(t); st.Enabled || st.PolicyURL != "" {
		t.Fatalf("a refused enable changed the setting: %+v", st)
	}

	st, log := c.mustRun(t, "-enable", "-policy-url", testPolicyURL)
	if !st.Enabled || st.PolicyURL != testPolicyURL {
		t.Fatalf("enabled state = %+v", st)
	}
	for _, want := range []string{"sms consent attestation set", c.scope.OrgID, c.scope.EnvID, "enabled=true", testPolicyURL} {
		if !strings.Contains(log, want) {
			t.Errorf("enable log %q lacks %q", log, want)
		}
	}
	if again, _ := c.mustRun(t, "-enable", "-policy-url", testPolicyURL); again.Enabled != st.Enabled || again.PolicyURL != st.PolicyURL {
		t.Fatalf("repeat enable = %+v, want %+v", again, st)
	}

	st, log = c.mustRun(t, "-disable")
	if st.Enabled || st.PolicyURL != testPolicyURL {
		t.Fatalf("disabled state = %+v, want off with the policy URL kept", st)
	}
	if !strings.Contains(log, "enabled=false") {
		t.Errorf("disable log %q lacks enabled=false", log)
	}
	if again, _ := c.mustRun(t, "-disable"); again.Enabled || again.PolicyURL != testPolicyURL {
		t.Fatalf("repeat disable = %+v", again)
	}

	// Re-enabling may lean on the policy URL already stored.
	if st, _ := c.mustRun(t, "-enable"); !st.Enabled || st.PolicyURL != testPolicyURL {
		t.Fatalf("re-enable = %+v", st)
	}
}

func TestSMSConsent_GrantAndRevokeKeepTheKey(t *testing.T) {
	c := newConsentTenant(t)
	srv := c.serve(t)
	hash := keyHash(t, c)
	c.mustRun(t, "-enable", "-policy-url", testPolicyURL)

	if code := c.attest(t, srv); code != http.StatusForbidden {
		t.Fatalf("attest before grant = %d, want 403", code)
	}

	for _, bad := range [][]string{
		{"-grant-key", "nope"},                    // too short to identify a key
		{"-grant-key", strings.Repeat("0", 64)},   // no such key
		{"-grant-key", c.apiKey},                  // plaintext is not an id
		{"-grant-key", hash, "-revoke-key", hash}, // contradictory
	} {
		if _, _, err := c.run(t, bad...); err == nil {
			t.Errorf("sms-consent %v: want an error", bad)
		}
	}

	st, log := c.mustRun(t, "-grant-key", hash[:12])
	if len(st.Keys) != 1 || st.Keys[0].ID != hash || !st.Keys[0].AttestSMSConsent {
		t.Fatalf("after grant keys = %+v, want %s granted", st.Keys, hash)
	}
	for _, want := range []string{"sms consent attest permission set", hash, "allowed=true"} {
		if !strings.Contains(log, want) {
			t.Errorf("grant log %q lacks %q", log, want)
		}
	}
	if strings.Contains(log, c.apiKey) {
		t.Fatalf("grant log leaks the key")
	}
	if keyHash(t, c) != hash {
		t.Fatalf("grant changed the key")
	}
	if again, _ := c.mustRun(t, "-grant-key", hash); !again.Keys[0].AttestSMSConsent {
		t.Fatalf("repeat grant = %+v", again.Keys)
	}
	if code := c.attest(t, srv); code != http.StatusCreated {
		t.Fatalf("attest after grant = %d, want 201", code)
	}

	st, log = c.mustRun(t, "-revoke-key", hash)
	if st.Keys[0].AttestSMSConsent || !strings.Contains(log, "allowed=false") {
		t.Fatalf("after revoke keys = %+v, log %q", st.Keys, log)
	}
	if keyHash(t, c) != hash {
		t.Fatalf("revoke changed the key")
	}
	if code := c.attest(t, srv); code != http.StatusForbidden {
		t.Fatalf("attest after revoke = %d, want 403", code)
	}
}

// TestSMSConsent_TenantKeyCannotReachIt: the switch exists only as this
// command, run against the database. Nothing on the HTTP API a tenant key
// authenticates to changes the setting or the permission.
func TestSMSConsent_TenantKeyCannotReachIt(t *testing.T) {
	c := newConsentTenant(t)
	srv := c.serve(t)
	hash := keyHash(t, c)
	body := `{"enabled":true,"policyUrl":"` + testPolicyURL + `","attestSmsConsent":true,"keyId":"` + hash + `"}`
	for _, path := range []string{
		"/v1/sms-consent",
		"/v1/environments/sms-consent",
		"/v1/environments/" + c.scope.EnvID + "/sms-consent",
		"/v1/api-keys/" + hash + "/attest-permission",
		"/v1/api-keys/attest-sms-consent",
		"/v1/admin/sms-consent",
		"/admin/sms-consent",
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
			code := do(t, method, srv.URL+path, c.apiKey, body)
			if code < 400 {
				t.Errorf("%s %s with a tenant key = %d, want a refusal", method, path, code)
			}
		}
	}
	st, _ := c.mustRun(t)
	if st.Enabled || st.Keys[0].AttestSMSConsent {
		t.Fatalf("a tenant key changed the state: %+v", st)
	}
	if code := c.attest(t, srv); code != http.StatusForbidden {
		t.Fatalf("attest = %d, want 403", code)
	}
}

// serve runs the HTTP API on a store of its own over the tenant's database,
// as a running notify serve would while an operator runs the command.
func (c consentTenant) serve(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, c.db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mk, err := masterKey("", true)
	if err != nil {
		t.Fatalf("masterKey: %v", err)
	}
	if _, err := st.Subscribers().Upsert(ctx, c.scope, domain.Subscriber{SubscriberID: "cust-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	aud := audience.New(st)
	pv := phoneverify.New(st, nil)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: identity.New(st, mk), Audience: aud, Trigger: trigger.New(aud, st, nil),
		Inbox: inboxapi.New(st), Store: st, Phone: pv,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (c consentTenant) attest(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	return do(t, http.MethodPost, srv.URL+"/v1/subscribers/cust-1/phone/consent", c.apiKey,
		`{"phone":"+15555550123","source":"in-store sign-up"}`)
}

func do(t *testing.T, method, url, key, body string) int {
	t.Helper()
	r, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "ApiKey "+key)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// keyHash reads the tenant's one key id straight from the database.
func keyHash(t *testing.T, c consentTenant) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, c.db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	keys, err := st.ApiKeys().List(ctx, c.scope)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %+v, %v; want one", keys, err)
	}
	return keys[0].Hash
}
