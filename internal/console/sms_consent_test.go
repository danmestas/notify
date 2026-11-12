package console

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

const consentPath = "/console/integrations/sms-consent"

// bothValues reads the environment setting and the key permission.
func bothValues(t *testing.T, tc *testConsole) (settingOn, keyMayAttest bool, url string) {
	t.Helper()
	ctx := context.Background()
	s, err := tc.st.SMSConsents().Settings(ctx, tc.scope)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	ok, err := tc.c.deps.Identity.AttestSMSConsentAllowed(ctx, tc.scope)
	if err != nil {
		t.Fatalf("permission: %v", err)
	}
	return s.Enabled, ok, s.PolicyURL
}

// TestSMSConsent_CardShowsOffByDefault: the Integrations page carries the
// one card, off, and the API keys page points to it.
func TestSMSConsent_CardShowsOffByDefault(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	body := tc.get(t, "/console/integrations", cookie).Body.String()
	for _, want := range []string{
		"Text customers who opted in at your shop",
		"Link to your privacy policy",
		`name="policy_url"`, `id="sms-consent-on"`,
		`id="sms-consent-status">Off<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("integrations page missing %q", want)
		}
	}
	if strings.Contains(body, `id="sms-consent-on" name="enabled" value="on" checked`) {
		t.Error("switch rendered on by default")
	}
	keys := tc.get(t, "/console/settings/api-keys", cookie).Body.String()
	if !strings.Contains(keys, "/console/integrations#sms-consent") {
		t.Error("api-keys page does not link to the switch")
	}
	if strings.Contains(keys, `name="policy_url"`) || strings.Contains(keys, "attest-sms-consent") {
		t.Error("api-keys page still carries the old controls")
	}
}

// TestSMSConsent_CardWordsAreJargonFree: a shop owner never sees these words
// in the card.
func TestSMSConsent_CardWordsAreJargonFree(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	tc.postForm(t, consentPath, url.Values{"csrf_token": {csrf}, "enabled": {"on"}, "policy_url": {"https://shop.example/privacy"}}, cookie)
	for _, path := range []string{"/console/integrations", "/console/integrations?smsConsentError=1"} {
		body := tc.get(t, path, cookie).Body.String()
		i := strings.Index(body, `id="sms-consent"`)
		if i < 0 {
			t.Fatalf("%s: card not found", path)
		}
		j := strings.Index(body[i:], `integration-toolbar`)
		if j < 0 {
			t.Fatalf("%s: card end not found", path)
		}
		card := strings.ToLower(body[i : i+j])
		for _, bad := range []string{"attest", "api key", "permission", "operator", "tcpa", "environment"} {
			if strings.Contains(card, bad) {
				t.Errorf("%s: card contains jargon %q", path, bad)
			}
		}
	}
}

// TestSMSConsent_OnSetsBothOffClearsBoth: turning the switch on sets the
// setting (with the link) and the key permission; off clears both.
func TestSMSConsent_OnSetsBothOffClearsBoth(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, consentPath,
		url.Values{"csrf_token": {csrf}, "enabled": {"on"}, "policy_url": {" https://shop.example/privacy "}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("on: %d", rr.Code)
	}
	on, key, link := bothValues(t, tc)
	if !on || !key || link != "https://shop.example/privacy" {
		t.Fatalf("after on: setting=%v key=%v url=%q", on, key, link)
	}
	body := tc.get(t, "/console/integrations?smsConsentSaved=1", cookie).Body.String()
	if !strings.Contains(body, "On, using your privacy policy at https://shop.example/privacy") {
		t.Error("status line does not say on")
	}

	rr = tc.postForm(t, consentPath, url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("off: %d", rr.Code)
	}
	on, key, _ = bothValues(t, tc)
	if on || key {
		t.Fatalf("after off: setting=%v key=%v", on, key)
	}
	if body := tc.get(t, "/console/integrations", cookie).Body.String(); !strings.Contains(body, `id="sms-consent-status">Off<`) {
		t.Error("status line does not say off")
	}
}

// TestSMSConsent_BadLinkChangesNothing: a missing or non-https link gets the
// plain error and stores nothing.
func TestSMSConsent_BadLinkChangesNothing(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	for _, bad := range []string{"", "http://shop.example/privacy", "shop.example/privacy"} {
		rr := tc.postForm(t, consentPath,
			url.Values{"csrf_token": {csrf}, "enabled": {"on"}, "policy_url": {bad}}, cookie)
		if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "smsConsentError=1") {
			t.Fatalf("%q: %d %q, want 303 with an error", bad, rr.Code, rr.Header().Get("Location"))
		}
		if on, key, link := bothValues(t, tc); on || key || link != "" {
			t.Fatalf("%q: stored setting=%v key=%v url=%q", bad, on, key, link)
		}
	}
	body := tc.get(t, "/console/integrations?smsConsentError=1", cookie).Body.String()
	if !strings.Contains(body, "Add a link to your privacy policy that starts with https://") {
		t.Error("page does not give the plain error")
	}
}

func TestSMSConsent_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)
	rr := tc.postForm(t, consentPath, url.Values{"csrf_token": {csrf}, "enabled": {"on"}, "policy_url": {"https://x.example/p"}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only: %d, want 403", rr.Code)
	}
	if on, key, _ := bothValues(t, tc); on || key {
		t.Error("read-only refusal still changed something")
	}
}

// TestSMSConsent_NeedsCSRF: a POST without the session's token is refused and
// changes nothing.
func TestSMSConsent_NeedsCSRF(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.postForm(t, consentPath, url.Values{"enabled": {"on"}, "policy_url": {"https://shop.example/privacy"}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("no token: %d, want 403", rr.Code)
	}
	if on, key, _ := bothValues(t, tc); on || key {
		t.Error("tokenless POST changed something")
	}
}

// TestSMSConsent_SubscriberShowsTheConsentKind: the subscriber page says how
// the phone is consented.
func TestSMSConsent_SubscriberShowsTheConsentKind(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	ctx := context.Background()
	sub, err := tc.st.Subscribers().Upsert(ctx, tc.scope, domain.Subscriber{SubscriberID: "cust-1"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if body := tc.get(t, "/console/subscribers/"+sub.ID, cookie).Body.String(); strings.Contains(body, "Operator-attested") {
		t.Error("consent shown with no phone")
	}
	if _, err := tc.st.SMSConsents().Attest(ctx, tc.scope, store.SMSAttestation{SubscriberID: "cust-1", Phone: "+15551112222", APIKeyID: "k"}); err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if body := tc.get(t, "/console/subscribers/"+sub.ID, cookie).Body.String(); !strings.Contains(body, "Operator-attested") {
		t.Error("subscriber page does not show operator-attested consent")
	}
	if err := tc.st.Subscribers().SetVerifiedPhone(ctx, tc.scope, "cust-1", "+15551112222", "2026-11-12T12:00:00Z"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if body := tc.get(t, "/console/subscribers/"+sub.ID, cookie).Body.String(); !strings.Contains(body, "Self-verified") {
		t.Error("subscriber page does not show self-verified consent")
	}
}
