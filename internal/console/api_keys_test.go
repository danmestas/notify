// api_keys_test.go proves the W4-E2 API Keys console page.
//
// Test index:
//
//	TestAPIKeys_PageRenders                      — page renders with masked key (not raw)
//	TestAPIKeys_MaskedKeyInList                  — list shows mask, never raw key
//	TestAPIKeys_UnauthenticatedRedirectsToLogin  — no cookie → 303 to /console/login
//	TestAPIKeys_NavEntryIsLive                   — nav link present + is-active
//	TestAPIKeys_Regenerate_ReadOnlyBlocked       — POST regenerate → 403 when ReadOnly
//	TestAPIKeys_Regenerate_Redirect              — regenerate → 303 with newKey param
//	TestAPIKeys_RawKeyShownOnceAfterRegenerate   — newKey visible in reveal card
//	TestAPIKeys_MaskedInNormalView               — raw key NOT in normal page body
//	TestAPIKeys_RotateMintNewKey                 — old key no longer authenticates
//	TestAPIKeys_NonDatastarPOSTRedirects         — native form POST gets 303 not SSE
//	TestAPIKeys_RouteAuth                        — routes → 303 /console/login without session
package console

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

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

	// The raw key (bootstrapped from tc.key) must NOT appear in the normal page.
	if strings.Contains(body, tc.key) {
		t.Errorf("api-keys page leaks raw key %q in body", tc.key)
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
	// The new raw key must appear in the redirect URL (shown-once param).
	if !strings.Contains(loc, "newKey=") {
		t.Errorf("regenerate redirect %q missing 'newKey=' param", loc)
	}
	if !strings.Contains(loc, "rotated=1") {
		t.Errorf("regenerate redirect %q missing 'rotated=1' param", loc)
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

	// Parse the new raw key from the redirect location.
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	newRaw := parsed.Query().Get("newKey")
	if newRaw == "" {
		t.Fatal("newKey param missing from redirect location")
	}

	// After rotation the old session cookie (which holds the old key) is
	// invalidated. Re-login with the new key to follow the redirect.
	tc.key = newRaw
	newCookie, _ := tc.login(t)

	// GET the redirect target — the reveal card must show the raw key.
	rr2 := tc.get(t, loc, newCookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("reveal page: status = %d, want 200 (body: %s)", rr2.Code, rr2.Body.String())
	}
	body := rr2.Body.String()
	if !strings.Contains(body, newRaw) {
		t.Errorf("reveal page does not contain the new raw key")
	}
	// Also confirm the "rotated" copy.
	if !strings.Contains(body, "API key rotated") {
		t.Errorf("reveal page missing 'API key rotated' heading")
	}
}

func TestAPIKeys_MaskedInNormalView(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Rotate to establish a known raw key.
	rr := tc.postForm(t, "/console/settings/api-keys/regenerate",
		url.Values{"csrf_token": {csrf}}, cookie)
	loc := rr.Header().Get("Location")
	parsed, _ := url.Parse(loc)
	newRaw := parsed.Query().Get("newKey")

	// Re-login with the new key (old session invalidated by rotation).
	tc.key = newRaw
	newCookie, _ := tc.login(t)

	// GET the normal page (not the reveal redirect) — raw key must not appear.
	rr2 := tc.get(t, "/console/settings/api-keys", newCookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("normal page after rotate: status = %d, want 200", rr2.Code)
	}
	body := rr2.Body.String()
	if strings.Contains(body, newRaw) {
		t.Errorf("normal api-keys page leaks raw key %q", newRaw)
	}
	// Masked version must still be present.
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

	// New key from the redirect.
	loc := rr.Header().Get("Location")
	parsed, _ := url.Parse(loc)
	newRaw := parsed.Query().Get("newKey")
	if newRaw == "" {
		t.Fatal("newKey param missing from redirect")
	}

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
