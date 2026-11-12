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

// TestSMSConsent_PageShowsTheSettingOff: the API keys page shows the SMS
// consent controls, off by default.
func TestSMSConsent_PageShowsTheSettingOff(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	body := tc.get(t, "/console/settings/api-keys", cookie).Body.String()
	for _, want := range []string{"SMS consent", `name="policy_url"`, `id="sms-consent-enabled"`, `id="attest-sms-consent"`} {
		if !strings.Contains(body, want) {
			t.Errorf("api-keys page missing %q", want)
		}
	}
	if strings.Contains(body, `id="sms-consent-enabled" name="enabled" value="on" checked`) {
		t.Error("setting rendered on by default")
	}
}

// TestSMSConsent_EnableNeedsAPolicyURL: enabling without an https policy URL
// is refused and stored nothing; with one it is stored and shown.
func TestSMSConsent_EnableNeedsAPolicyURL(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	ctx := context.Background()

	rr := tc.postForm(t, "/console/settings/api-keys/sms-consent",
		url.Values{"csrf_token": {csrf}, "enabled": {"on"}, "policy_url": {"http://shop.example/privacy"}}, cookie)
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "smsConsentError=1") {
		t.Fatalf("enable with http URL: %d %q, want 303 with an error", rr.Code, rr.Header().Get("Location"))
	}
	if s, _ := tc.st.SMSConsents().Settings(ctx, tc.scope); s.Enabled {
		t.Fatalf("refused enable stored %+v", s)
	}
	if body := tc.get(t, "/console/settings/api-keys?smsConsentError=1", cookie).Body.String(); !strings.Contains(body, "https privacy policy URL") {
		t.Error("page does not explain the refusal")
	}

	rr = tc.postForm(t, "/console/settings/api-keys/sms-consent",
		url.Values{"csrf_token": {csrf}, "enabled": {"on"}, "policy_url": {"https://shop.example/privacy"}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("enable: %d", rr.Code)
	}
	if s, _ := tc.st.SMSConsents().Settings(ctx, tc.scope); !s.Enabled || s.PolicyURL != "https://shop.example/privacy" {
		t.Fatalf("settings = %+v", s)
	}
	body := tc.get(t, "/console/settings/api-keys", cookie).Body.String()
	if !strings.Contains(body, `value="https://shop.example/privacy"`) || !strings.Contains(body, `name="enabled" value="on" checked`) {
		t.Error("page does not show the stored setting")
	}

	// Unchecked turns it off and keeps the URL.
	tc.postForm(t, "/console/settings/api-keys/sms-consent",
		url.Values{"csrf_token": {csrf}, "policy_url": {"https://shop.example/privacy"}}, cookie)
	if s, _ := tc.st.SMSConsents().Settings(ctx, tc.scope); s.Enabled || s.PolicyURL == "" {
		t.Fatalf("after disable = %+v", s)
	}
}

// TestSMSConsent_KeyPermission: the console grants and withdraws the key's
// permission.
func TestSMSConsent_KeyPermission(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	ctx := context.Background()
	rr := tc.postForm(t, "/console/settings/api-keys/attest-permission",
		url.Values{"csrf_token": {csrf}, "allow": {"on"}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("grant: %d", rr.Code)
	}
	if ok, _ := tc.c.deps.Identity.AttestSMSConsentAllowed(ctx, tc.scope); !ok {
		t.Fatal("permission not granted")
	}
	if body := tc.get(t, "/console/settings/api-keys", cookie).Body.String(); !strings.Contains(body, `id="attest-sms-consent" name="allow" value="on" checked`) {
		t.Error("page does not show the granted permission")
	}
	tc.postForm(t, "/console/settings/api-keys/attest-permission", url.Values{"csrf_token": {csrf}}, cookie)
	if ok, _ := tc.c.deps.Identity.AttestSMSConsentAllowed(ctx, tc.scope); ok {
		t.Fatal("permission not withdrawn")
	}
}

func TestSMSConsent_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)
	for _, path := range []string{"/console/settings/api-keys/sms-consent", "/console/settings/api-keys/attest-permission"} {
		rr := tc.postForm(t, path, url.Values{"csrf_token": {csrf}, "enabled": {"on"}, "allow": {"on"}, "policy_url": {"https://x.example/p"}}, cookie)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s read-only: %d, want 403", path, rr.Code)
		}
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
