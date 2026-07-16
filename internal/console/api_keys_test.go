// api_keys_test.go proves the W4-E2 API Keys console page.
//
// Test index:
//
//	TestAPIKeys_PageRenders                      — page renders with masked key (not raw)
//	TestAPIKeys_MaskedKeyInList                  — list shows mask, never raw key
//	TestAPIKeys_InitialPageOmitsRawKey           — H1: initial render never ships the plaintext secret
//	TestAPIKeys_RevealEndpointReturnsRawKey      — ?reveal=1 is a separate authenticated request that returns plaintext
//	TestAPIKeys_UnauthenticatedRedirectsToLogin  — no cookie → 303 to /console/login
//	TestAPIKeys_NavEntryIsLive                   — nav link present + is-active
//	TestAPIKeys_Regenerate_ReadOnlyBlocked       — POST regenerate → 403 when ReadOnly
//	TestAPIKeys_Regenerate_Redirect              — regenerate → 303 with a one-shot reveal token, never the raw key
//	TestAPIKeys_Regenerate_RawKeyNeverInLocation — redirect Location never contains a raw-key-shaped value
//	TestAPIKeys_RawKeyShownOnceAfterRegenerate   — reveal token resolves to the raw key exactly once
//	TestAPIKeys_RevealTokenConsumedOnce          — a second GET with the same (now-consumed) token shows no secret
//	TestAPIKeys_MaskedInNormalView               — raw key NOT in normal page body
//	TestAPIKeys_RotateMintNewKey                 — old key no longer authenticates
//	TestAPIKeys_NonDatastarPOSTRedirects         — native form POST gets 303 not SSE
//	TestAPIKeys_RouteAuth                        — routes (incl. ?reveal=1) → 303 /console/login without session
package console

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// hexKeyRE matches a 64-hex-char value — the exact shape of a raw API key
// (or stream-source secret). Used to prove a redirect Location never carries
// a raw secret, only an opaque (shorter) reveal token.
var hexKeyRE = regexp.MustCompile(`[0-9a-f]{64}`)

// newAPIKeyRE captures the raw key rendered in the one-time reveal card.
var newAPIKeyRE = regexp.MustCompile(`id="new-api-key-value" class="console-secret-value">([0-9a-f]{64})</code>`)

// ─── page render ──────────────────────────────────────────────────────────────

func TestAPIKeys_PageRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/settings/api-keys", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/settings/api-keys: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "API Keys") {
		t.Errorf("api-keys page missing 'API Keys' heading")
	}
	// Must show the Secret Key section.
	if !strings.Contains(body, "Secret Key") {
		t.Errorf("api-keys page missing 'Secret Key' card")
	}
}

func TestAPIKeys_MaskedKeyInList(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/settings/api-keys", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// The masked display must contain the bullet-mask prefix.
	if !strings.Contains(body, "••") {
		t.Errorf("api-keys page missing masked key (•• sentinel not found)")
	}
}

// TestAPIKeys_InitialPageOmitsRawKey is the regression test for H1: the
// plaintext API key secret must never be shipped in the initial page HTML,
// masked-or-not-via-client-toggle. Only the mask (plus last4, which is
// already safe to display) may appear until an explicit ?reveal=1 request.
func TestAPIKeys_InitialPageOmitsRawKey(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/settings/api-keys", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	if strings.Contains(body, tc.key) {
		t.Errorf("H1 regression: initial /console/settings/api-keys HTML contains the plaintext secret %q", tc.key)
	}
	// The masked sentinel must be present — that's the only thing that should
	// render before an explicit reveal.
	if !strings.Contains(body, "••") {
		t.Errorf("masked key sentinel •• missing from api-keys page")
	}
	// The reveal control must be a link to the on-demand reveal endpoint, not
	// a client-side toggle carrying the secret.
	if !strings.Contains(body, `href="/console/settings/api-keys?reveal=1"`) {
		t.Errorf("api-keys page missing reveal link to ?reveal=1")
	}
}

// TestAPIKeys_RevealEndpointReturnsRawKey proves the reveal-on-demand
// pattern mirrored from Environment Variables: GET
// /console/settings/api-keys?reveal=1 is a separate authenticated request
// that returns the plaintext secret only when explicitly asked for.
func TestAPIKeys_RevealEndpointReturnsRawKey(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/settings/api-keys?reveal=1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	if !strings.Contains(body, tc.key) {
		t.Errorf("reveal endpoint does not contain raw key %q", tc.key)
	}
	// A "Hide" link must be present to fold the secret back away.
	if !strings.Contains(body, `href="/console/settings/api-keys"`) {
		t.Errorf("revealed page missing Hide link back to masked view")
	}
}

func TestAPIKeys_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)

	rr := tc.get(t, "/console/settings/api-keys", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated: status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/console/login") {
		t.Errorf("redirect = %q, want /console/login", loc)
	}
}

func TestAPIKeys_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/settings/api-keys", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/settings/api-keys"`) {
		t.Error("nav link /console/settings/api-keys missing from page")
	}
	if !strings.Contains(body, "is-active") {
		t.Error("is-active class missing from active nav entry")
	}
}

// ─── mutation: regenerate ─────────────────────────────────────────────────────

func TestAPIKeys_Regenerate_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/settings/api-keys/regenerate",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("regenerate read-only: status = %d, want 403", rr.Code)
	}
}

func TestAPIKeys_Regenerate_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/settings/api-keys/regenerate",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("regenerate: status = %d, want 303", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/console/settings/api-keys") {
		t.Errorf("regenerate redirect = %q, want /console/settings/api-keys", loc)
	}
	// The redirect must carry a one-shot reveal TOKEN, not the raw key.
	if !strings.Contains(loc, "revealed=") {
		t.Errorf("regenerate redirect %q missing 'revealed=' token param", loc)
	}
	if strings.Contains(loc, "newKey=") {
		t.Errorf("regenerate redirect %q still carries the legacy 'newKey=' param", loc)
	}
	if !strings.Contains(loc, "rotated=1") {
		t.Errorf("regenerate redirect %q missing 'rotated=1' param", loc)
	}
}

// TestAPIKeys_Regenerate_RawKeyNeverInLocation is the H-series regression for
// this hardening: the raw secret must never transit the redirect Location —
// it would otherwise land in browser history and any reverse-proxy/CDN
// access log fronting the console (e.g. a Cloudflare Tunnel).
func TestAPIKeys_Regenerate_RawKeyNeverInLocation(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/settings/api-keys/regenerate",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("regenerate: status = %d, want 303", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if hexKeyRE.MatchString(loc) {
		t.Errorf("regenerate redirect leaks a raw-key-shaped value: %q", loc)
	}
}

func TestAPIKeys_RawKeyShownOnceAfterRegenerate(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Rotate the key.
	rr := tc.postForm(t, "/console/settings/api-keys/regenerate",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("regenerate: status = %d, want 303", rr.Code)
	}
	loc := rr.Header().Get("Location")

	// Rotating the operator's OWN active key invalidates the old session
	// cookie (the old key's row is gone). The handler re-seals a fresh
	// cookie carrying the new key on the SAME response; a browser's jar
	// carries it forward to the redirect target. Mirror that here.
	cookie = sessionCookie(rr)
	if cookie == nil {
		t.Fatal("regenerate did not re-seal a session cookie")
	}
	rr2 := tc.get(t, loc, cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("reveal page: status = %d, want 200 (body: %s)", rr2.Code, rr2.Body.String())
	}
	body := rr2.Body.String()
	m := newAPIKeyRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("reveal page missing new API key. body:\n%s", body)
	}
	newRaw := m[1]
	// Shown exactly once.
	if got := strings.Count(body, newRaw); got != 1 {
		t.Errorf("new key should appear exactly once in reveal page, found %d", got)
	}
	// Also confirm the "rotated" copy.
	if !strings.Contains(body, "API key rotated") {
		t.Errorf("reveal page missing 'API key rotated' heading")
	}
	// The revealed key must actually authenticate (rotation truly happened).
	if _, err := tc.deps.Identity.Authenticate(testBackground(), newRaw); err != nil {
		t.Errorf("new raw key does not authenticate: %v", err)
	}
}

// TestAPIKeys_RevealTokenConsumedOnce proves the one-shot semantics: a
// second GET against the same redirect Location (the token already consumed
// by the first GET) must render no secret at all.
func TestAPIKeys_RevealTokenConsumedOnce(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/settings/api-keys/regenerate",
		url.Values{"csrf_token": {csrf}}, cookie)
	loc := rr.Header().Get("Location")

	// Carry the re-sealed cookie forward (the old key is now revoked).
	cookie = sessionCookie(rr)
	if cookie == nil {
		t.Fatal("regenerate did not re-seal a session cookie")
	}

	// First GET consumes the token and shows the secret.
	first := tc.get(t, loc, cookie)
	m := newAPIKeyRE.FindStringSubmatch(first.Body.String())
	if m == nil {
		t.Fatalf("first reveal GET missing new API key. body:\n%s", first.Body.String())
	}
	newRaw := m[1]

	// Second GET against the identical URL (same token) must NOT re-reveal.
	second := tc.get(t, loc, cookie)
	if second.Code != http.StatusOK {
		t.Fatalf("second reveal GET: status = %d, want 200", second.Code)
	}
	body := second.Body.String()
	if strings.Contains(body, newRaw) {
		t.Errorf("reload with a consumed reveal token re-showed the raw key")
	}
	if newAPIKeyRE.MatchString(body) {
		t.Errorf("reload with a consumed reveal token still rendered a reveal card")
	}
}

func TestAPIKeys_MaskedInNormalView(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Rotate to establish a known raw key, discovered from the one-time
	// reveal card (the redirect Location itself only carries a token).
	rr := tc.postForm(t, "/console/settings/api-keys/regenerate",
		url.Values{"csrf_token": {csrf}}, cookie)
	loc := rr.Header().Get("Location")
	cookie = sessionCookie(rr) // re-sealed with the new key
	if cookie == nil {
		t.Fatal("regenerate did not re-seal a session cookie")
	}
	reveal := tc.get(t, loc, cookie)
	m := newAPIKeyRE.FindStringSubmatch(reveal.Body.String())
	if m == nil {
		t.Fatalf("reveal page missing new API key. body:\n%s", reveal.Body.String())
	}
	newRaw := m[1]

	// GET the normal page (not the reveal redirect) — raw key must not appear.
	rr2 := tc.get(t, "/console/settings/api-keys", cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("normal page after rotate: status = %d, want 200", rr2.Code)
	}
	body := rr2.Body.String()
	if strings.Contains(body, newRaw) {
		t.Errorf("H1 regression: normal api-keys page contains the plaintext secret %q", newRaw)
	}
	if !strings.Contains(body, "••") {
		t.Errorf("normal api-keys page missing masked key")
	}
}

func TestAPIKeys_RotateMintNewKey(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	oldKey := tc.key

	rr := tc.postForm(t, "/console/settings/api-keys/regenerate",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("regenerate: status = %d, want 303", rr.Code)
	}

	// New key discovered from the reveal card. The handler re-seals the
	// session cookie with the new key; carry it forward as a browser would.
	loc := rr.Header().Get("Location")
	newCookie := sessionCookie(rr)
	if newCookie == nil {
		t.Fatal("regenerate did not re-seal a session cookie")
	}
	reveal := tc.get(t, loc, newCookie)
	m := newAPIKeyRE.FindStringSubmatch(reveal.Body.String())
	if m == nil {
		t.Fatalf("reveal page missing new API key. body:\n%s", reveal.Body.String())
	}
	newRaw := m[1]

	// Old key must no longer authenticate.
	if _, err := tc.deps.Identity.Authenticate(testBackground(), oldKey); err == nil {
		t.Error("old key still authenticates after rotation — revoke broken")
	}

	// New key must authenticate.
	if _, err := tc.deps.Identity.Authenticate(testBackground(), newRaw); err != nil {
		t.Errorf("new key does not authenticate: %v", err)
	}
}

func TestAPIKeys_NonDatastarPOSTRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/settings/api-keys/regenerate",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("regenerate POST: status = %d, want 303", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		t.Error("regenerate POST: Content-Type is SSE — should be redirect")
	}
	if body := rr.Body.String(); strings.Contains(body, "datastar-patch-elements") {
		t.Error("regenerate POST body contains 'datastar-patch-elements' — SSE leaked")
	}
}

// ─── route auth ───────────────────────────────────────────────────────────────

func TestAPIKeys_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)
	routes := []string{
		"/console/settings/api-keys",
		"/console/settings/api-keys?reveal=1",
	}
	for _, path := range routes {
		rr := tc.get(t, path, nil)
		if rr.Code != http.StatusSeeOther {
			t.Errorf("unauthenticated %s: status = %d, want 303", path, rr.Code)
		}
		if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/console/login") {
			t.Errorf("%s redirect = %q, want /console/login", path, loc)
		}
	}
}
