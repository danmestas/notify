package console

// stream_sources_test.go covers the S5 console "Stream Sources" page: list
// with masked secret + limits + activity, create → one-time reveal, rotate →
// new one-time reveal, delete/toggle, limit clamps, cross-tenant isolation,
// and the discoverability affordances (integrations-page link + sidebar nav).
//
// No sidecar: the streamsource.Service runs over the same temp-SQLite store the
// rest of the console tests use (newTestConsole wires it into Deps).

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// secretRE captures a minted source secret from the one-time reveal snippet.
// It anchors on the SourceKey scheme so it never collides with the equally
// 64-hex CSRF token that appears in every form on the page.
var secretRE = regexp.MustCompile(`SourceKey ([0-9a-f]{64})`)

// createSource mints a source through the service under the test scope and
// returns the row plus its one-time raw secret.
func (tc *testConsole) createSource(t *testing.T, name, topic string) (domain.StreamSource, string) {
	t.Helper()
	src, raw, err := tc.deps.StreamSources.Create(context.Background(), tc.scope, name, topic)
	if err != nil {
		t.Fatalf("createSource(%q,%q): %v", name, topic, err)
	}
	return src, raw
}

func TestStreamSources_EmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/stream-sources", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/stream-sources: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Stream Sources") {
		t.Errorf("empty page missing title. body:\n%s", body)
	}
	// The list fragment initial render should show the no-sources message.
	frag := tc.get(t, "/console/api/fragments/stream-sources-list", cookie)
	if frag.Code != http.StatusOK {
		t.Fatalf("GET stream-sources-list: status = %d, want 200", frag.Code)
	}
	if !strings.Contains(strings.ToLower(frag.Body.String()), "no stream sources") {
		t.Errorf("empty fragment missing empty-state copy. body:\n%s", frag.Body.String())
	}
}

func TestStreamSources_ListMaskedNeverRaw(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	src, raw := tc.createSource(t, "tokens", "agent.tokens")

	frag := tc.get(t, "/console/api/fragments/stream-sources-list", cookie)
	if frag.Code != http.StatusOK {
		t.Fatalf("GET stream-sources-list: status = %d, want 200", frag.Code)
	}
	body := frag.Body.String()
	if !strings.Contains(body, "tokens") {
		t.Errorf("list missing source name. body:\n%s", body)
	}
	if !strings.Contains(body, "agent.tokens") {
		t.Errorf("list missing source topic. body:\n%s", body)
	}
	masked := tc.deps.StreamSources.MaskedSecret(src)
	if !strings.Contains(body, masked) {
		t.Errorf("list missing masked secret %q. body:\n%s", masked, body)
	}
	// The raw secret must NEVER appear in a list view.
	if strings.Contains(body, raw) {
		t.Errorf("RAW SECRET leaked into list HTML")
	}
	// Limits summary + activity affordance present.
	if !strings.Contains(body, "600") { // default rate limit/min
		t.Errorf("list missing rate limit. body:\n%s", body)
	}
}

func TestStreamSources_CreateRevealsSecretOnce(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/stream-sources", url.Values{
		"name":       {"relay"},
		"topic":      {"llm.relay"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create POST: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "newSecret=") {
		t.Fatalf("create redirect missing newSecret: %q", loc)
	}

	page := tc.get(t, loc, cookie)
	if page.Code != http.StatusOK {
		t.Fatalf("reveal page: status = %d, want 200", page.Code)
	}
	body := page.Body.String()
	m := secretRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("reveal page missing SourceKey secret. body:\n%s", body)
	}
	secret := m[1]
	// The one-time reveal must carry a ready-to-copy producer snippet.
	if !strings.Contains(body, "/v1/sources/") {
		t.Errorf("reveal page missing curl producer snippet. body:\n%s", body)
	}
	// Shown exactly once (the raw secret literal appears a single time).
	if got := strings.Count(body, secret); got != 1 {
		t.Fatalf("reveal secret should appear exactly once, found %d", got)
	}
	// The revealed secret must authenticate.
	if _, err := tc.deps.StreamSources.Authenticate(context.Background(), secret); err != nil {
		t.Errorf("revealed secret does not authenticate: %v", err)
	}
	// A subsequent plain page load must NOT re-reveal the secret.
	plain := tc.get(t, "/console/stream-sources", cookie)
	if strings.Contains(plain.Body.String(), "stream-source-reveal-card") {
		t.Errorf("plain page still renders the reveal card")
	}
	if strings.Contains(plain.Body.String(), secret) {
		t.Errorf("plain page re-revealed the raw secret")
	}

	// Invalid topic → error flash, no row created.
	before, _ := tc.deps.StreamSources.List(context.Background(), tc.scope)
	bad := tc.postForm(t, "/console/stream-sources", url.Values{
		"name":       {"bad"},
		"topic":      {"orders.*"},
		"csrf_token": {csrf},
	}, cookie)
	if bad.Code != http.StatusSeeOther {
		t.Fatalf("bad create: status = %d, want 303", bad.Code)
	}
	if !strings.Contains(bad.Header().Get("Location"), "createError=") {
		t.Errorf("bad create redirect missing createError: %q", bad.Header().Get("Location"))
	}
	after, _ := tc.deps.StreamSources.List(context.Background(), tc.scope)
	if len(after) != len(before) {
		t.Errorf("invalid topic created a row: before=%d after=%d", len(before), len(after))
	}
}

func TestStreamSources_Rotate(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	src, oldRaw := tc.createSource(t, "rot", "a.b")

	rr := tc.postForm(t, "/console/stream-sources/"+src.ID+"/rotate", url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("rotate POST: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	page := tc.get(t, loc, cookie)
	m := secretRE.FindStringSubmatch(page.Body.String())
	if m == nil {
		t.Fatalf("rotate reveal missing SourceKey secret. body:\n%s", page.Body.String())
	}
	newSecret := m[1]
	if newSecret == oldRaw {
		t.Fatalf("rotate returned the same secret")
	}
	// Old secret must stop authenticating; new one must work.
	if _, err := tc.deps.StreamSources.Authenticate(context.Background(), oldRaw); err == nil {
		t.Errorf("old secret still authenticates after rotate")
	}
	if _, err := tc.deps.StreamSources.Authenticate(context.Background(), newSecret); err != nil {
		t.Errorf("new secret does not authenticate: %v", err)
	}
}

func TestStreamSources_ToggleDeleteAndLimitClamps(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	src, _ := tc.createSource(t, "svc", "s.one")
	ctx := context.Background()

	// Toggle inactive.
	if rr := tc.postForm(t, "/console/stream-sources/"+src.ID+"/active", url.Values{
		"active":     {"false"},
		"csrf_token": {csrf},
	}, cookie); rr.Code != http.StatusSeeOther {
		t.Fatalf("toggle: status = %d, want 303", rr.Code)
	}
	got, _ := tc.deps.StreamSources.Get(ctx, tc.scope, src.ID)
	if got.Active {
		t.Errorf("source still active after toggle-off")
	}

	// Edit limits with out-of-range values → server clamps.
	if rr := tc.postForm(t, "/console/stream-sources/"+src.ID, url.Values{
		"rate_limit_per_min": {"99999"},
		"max_event_bytes":    {"4096"},
		"max_batch_events":   {"50"},
		"byte_budget":        {"999999999"},
		"csrf_token":         {csrf},
	}, cookie); rr.Code != http.StatusSeeOther {
		t.Fatalf("limits: status = %d, want 303", rr.Code)
	}
	got, _ = tc.deps.StreamSources.Get(ctx, tc.scope, src.ID)
	if got.RateLimitPerMin != 6000 {
		t.Errorf("rate limit not clamped to 6000: got %d", got.RateLimitPerMin)
	}
	if got.ByteBudget != 16<<20 {
		t.Errorf("byte budget not clamped to 16 MiB: got %d", got.ByteBudget)
	}
	if got.MaxEventBytes != 4096 || got.MaxBatchEvents != 50 {
		t.Errorf("in-range limits not saved: eventBytes=%d batch=%d", got.MaxEventBytes, got.MaxBatchEvents)
	}

	// Delete.
	if rr := tc.postForm(t, "/console/stream-sources/"+src.ID+"/delete", url.Values{
		"csrf_token": {csrf},
	}, cookie); rr.Code != http.StatusSeeOther {
		t.Fatalf("delete: status = %d, want 303", rr.Code)
	}
	if list, _ := tc.deps.StreamSources.List(ctx, tc.scope); len(list) != 0 {
		t.Errorf("source not deleted: %d rows remain", len(list))
	}
}

func TestStreamSources_CrossTenant404(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	alt := tc.altScope(t)

	// A source that belongs to another tenant.
	foreign, _, err := tc.deps.StreamSources.Create(context.Background(), alt, "foreign", "x.y")
	if err != nil {
		t.Fatalf("create foreign source: %v", err)
	}

	// It must not appear in the logged-in scope's list.
	frag := tc.get(t, "/console/api/fragments/stream-sources-list", cookie)
	if strings.Contains(frag.Body.String(), foreign.ID) {
		t.Errorf("foreign source id leaked into this tenant's list")
	}

	// Mutating it must 404, not silently succeed.
	for _, p := range []string{
		"/console/stream-sources/" + foreign.ID + "/rotate",
		"/console/stream-sources/" + foreign.ID + "/delete",
		"/console/stream-sources/" + foreign.ID + "/active",
		"/console/stream-sources/" + foreign.ID,
	} {
		rr := tc.postForm(t, p, url.Values{
			"active":             {"false"},
			"rate_limit_per_min": {"100"},
			"max_event_bytes":    {"1024"},
			"max_batch_events":   {"10"},
			"byte_budget":        {"1024"},
			"csrf_token":         {csrf},
		}, cookie)
		if rr.Code != http.StatusNotFound {
			t.Errorf("POST %s cross-tenant: status = %d, want 404", p, rr.Code)
		}
	}
	// The foreign source is untouched.
	still, err := tc.deps.StreamSources.Get(context.Background(), alt, foreign.ID)
	if err != nil {
		t.Fatalf("foreign source vanished: %v", err)
	}
	if !still.Active {
		t.Errorf("foreign source was mutated across tenants")
	}
}

func TestStreamSources_Discoverability(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	// (a) integrations-page toolbar carries the signpost link.
	integ := tc.get(t, "/console/integrations", cookie)
	if integ.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations: status = %d, want 200", integ.Code)
	}
	if !strings.Contains(integ.Body.String(), `href="/console/stream-sources"`) {
		t.Errorf("integrations page missing 'Stream sources' link")
	}

	// (b) sidebar nav carries the entry (rendered on every page).
	page := tc.get(t, "/console/stream-sources", cookie)
	nav := page.Body.String()
	if !strings.Contains(nav, "Stream Sources") {
		t.Errorf("sidebar nav missing 'Stream Sources' entry")
	}
	if !strings.Contains(nav, `href="/console/stream-sources"`) {
		t.Errorf("sidebar nav missing stream-sources href")
	}
}

func TestStreamSources_CreateFormSubmitsNatively(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/stream-sources", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/stream-sources: status = %d, want 200", rr.Code)
	}
	form := formSlice(t, rr.Body.String(), `action="/console/stream-sources"`)
	assertNativeFormSubmits(t, form, "stream source create")
}
