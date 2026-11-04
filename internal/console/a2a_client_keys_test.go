package console

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var a2aSecretRE = regexp.MustCompile(`a2ack_[0-9a-f]{64}`)

func TestA2AKeys_MintRevealOnceAndRevoke(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	if rr := tc.get(t, "/console/a2a-keys", cookie); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "No client keys yet") {
		t.Fatalf("empty page: %d", rr.Code)
	}

	rr := tc.postForm(t, "/console/a2a-keys", url.Values{"csrf_token": {csrf}, "name": {"billing"}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("mint = %d, want 303 (%s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if a2aSecretRE.MatchString(loc) {
		t.Fatalf("redirect Location carries the secret: %s", loc)
	}

	page := tc.get(t, loc, cookie)
	secret := a2aSecretRE.FindString(page.Body.String())
	if secret == "" {
		t.Fatalf("reveal card missing the secret:\n%s", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "Copy") {
		t.Errorf("reveal card has no copy button")
	}
	// Second load of the same URL: the secret is gone.
	if again := tc.get(t, loc, cookie); a2aSecretRE.MatchString(again.Body.String()) {
		t.Errorf("secret shown twice")
	}

	keys, err := tc.deps.A2AAuth.List(context.Background(), tc.scope)
	if err != nil || len(keys) != 1 {
		t.Fatalf("List = %v, %v", keys, err)
	}
	id := keys[0].ID

	if rr := tc.postForm(t, "/console/a2a-keys/"+id+"/revoke", url.Values{"csrf_token": {csrf}}, cookie); rr.Code != http.StatusSeeOther {
		t.Fatalf("revoke = %d, want 303", rr.Code)
	}
	list := tc.get(t, "/console/a2a-keys", cookie).Body.String()
	if !strings.Contains(list, "Revoked") {
		t.Errorf("revoked key not marked:\n%s", list)
	}
}

func TestA2AKeys_MintErrorRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	rr := tc.postForm(t, "/console/a2a-keys", url.Values{"csrf_token": {csrf}, "name": {""}}, cookie)
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "createError=") {
		t.Fatalf("empty name: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	rr = tc.postForm(t, "/console/a2a-keys", url.Values{"csrf_token": {csrf}, "name": {"x"}, "subscriber_id": {"nobody"}}, cookie)
	if !strings.Contains(rr.Header().Get("Location"), "createError=") {
		t.Fatalf("unknown subscriber: %s", rr.Header().Get("Location"))
	}
}
