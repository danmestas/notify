package console

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestLogin_StylesheetUsesHashedAssetURL guards the login page against
// regressing to a bare /console/assets/app.css href. Assets are content-
// fingerprinted (assets_manifest.go), so the bare path 404s and the login
// page renders unstyled — exactly the bug where only the pre-auth login
// page lost its CSS while every authed page (layout.html) stayed styled.
// The login template MUST use {{ assetURL "app.css" }} like layout.html.
func TestLogin_StylesheetUsesHashedAssetURL(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/login", nil) // login is pre-auth: no session cookie
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/login: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, `href="/console/assets/app.css"`) {
		t.Error(`login page references the BARE "/console/assets/app.css" — it 404s under content-hashing; use {{ assetURL "app.css" }}`)
	}
	if !regexp.MustCompile(`href="/console/assets/app\.[0-9a-f]+\.css"`).MatchString(body) {
		t.Error("login page must reference a content-hashed app.<hash>.css URL (via assetURL), not a bare path")
	}
}
