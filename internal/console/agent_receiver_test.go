package console

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var receiverSecretRE = regexp.MustCompile(`a2arx_[0-9a-f]{64}`)

func TestAgentReceiverKeys_MintRevealOnceAndRevoke(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	seedBasicAgent(t, tc, "bot", "Bot")
	overview := "/console/agents/bot/tabs/overview"

	page := tc.get(t, overview, cookie).Body.String()
	if !strings.Contains(page, "Answered by") || !strings.Contains(page, "mesh runtime") {
		t.Fatalf("overview lacks the answered-by row:\n%s", page)
	}

	rr := tc.postForm(t, "/console/agents/bot/receiver-keys", url.Values{"csrf_token": {csrf}, "name": {"laptop"}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("mint = %d, want 303", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if receiverSecretRE.MatchString(loc) {
		t.Fatalf("redirect Location carries the secret: %s", loc)
	}
	secret := receiverSecretRE.FindString(tc.get(t, loc, cookie).Body.String())
	if secret == "" {
		t.Fatal("reveal card missing the secret")
	}
	if receiverSecretRE.MatchString(tc.get(t, loc, cookie).Body.String()) {
		t.Error("secret shown twice")
	}

	keys, err := tc.deps.Receiver.List(testBackground(), tc.scope, "bot")
	if err != nil || len(keys) != 1 {
		t.Fatalf("List = %v, %v", keys, err)
	}
	if rr := tc.postForm(t, "/console/agents/bot/receiver-keys/"+keys[0].ID+"/revoke", url.Values{"csrf_token": {csrf}}, cookie); rr.Code != http.StatusSeeOther {
		t.Fatalf("revoke = %d, want 303", rr.Code)
	}
	if !strings.Contains(tc.get(t, overview, cookie).Body.String(), "revoked") {
		t.Error("revoked key not marked")
	}
}
