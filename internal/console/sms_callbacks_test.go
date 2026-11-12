package console

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// TestFragment_IntegrationDetail_SMSCallbackURLs: an SMS integration's drawer
// shows the two URLs to paste into Twilio or SignalWire; other providers'
// drawers do not.
func TestFragment_IntegrationDetail_SMSCallbackURLs(t *testing.T) {
	tc := newTestConsole(t)
	sms := tc.seedIntegration(t, domain.Integration{ProviderID: "twilio", Channel: "sms", Active: true, Identifier: "tw-cb", EncCredentials: []byte("fake")})
	mail := tc.seedIntegration(t, domain.Integration{ProviderID: "smtp", Channel: "email", Active: true, Identifier: "smtp-cb", EncCredentials: []byte("fake")})
	cookie, _ := tc.login(t)

	body := tc.getWithQuery(t, "/console/api/fragments/integration-detail", url.Values{"id": {sms.ID}}.Encode(), cookie).Body.String()
	for _, want := range []string{"/v1/inbound/sms/" + sms.ID, "/v1/inbound/sms/" + sms.ID + "/status", "Inbound message URL", "Status callback URL"} {
		if !strings.Contains(body, want) {
			t.Errorf("sms drawer missing %q", want)
		}
	}
	body = tc.getWithQuery(t, "/console/api/fragments/integration-detail", url.Values{"id": {mail.ID}}.Encode(), cookie).Body.String()
	if strings.Contains(body, "/v1/inbound/sms/") {
		t.Error("an email integration's drawer shows SMS callback URLs")
	}
}

// TestIntegrations_Update_HelpText: the edit form sets a SignalWire signing
// key and HELP reply, clears the reply when emptied, and refuses a keyword.
func TestIntegrations_Update_HelpText(t *testing.T) {
	tc := newTestConsole(t)
	in, err := tc.deps.Connectivity.CreateSignalWire(testBackground(), tc.scope, "sw-edit",
		connectivity.SignalWireConfig{ProjectID: "p", Token: "t", SpaceURL: "x.signalwire.com", From: "+15550001111"}, true)
	if err != nil {
		t.Fatalf("CreateSignalWire: %v", err)
	}
	cookie, csrf := tc.login(t)
	save := func(fields url.Values) int {
		t.Helper()
		fields.Set("_method", "PUT")
		fields.Set("identifier", "sw-edit")
		fields.Set("csrf_token", csrf)
		return tc.postForm(t, "/console/integrations/"+in.ID, fields, cookie).Code
	}
	callback := func() connectivity.SMSCallback {
		t.Helper()
		cb, err := tc.deps.Connectivity.SMSCallback(testBackground(), in.ID)
		if err != nil {
			t.Fatalf("SMSCallback: %v", err)
		}
		return cb
	}

	if code := save(url.Values{"signing_key": {"PSK1"}, "help_text": {"Acme help"}}); code != http.StatusSeeOther {
		t.Fatalf("save: status %d", code)
	}
	if cb := callback(); cb.SigningKey != "PSK1" || cb.HelpText != "Acme help" {
		t.Fatalf("after save: %+v", cb)
	}
	if code := save(url.Values{"help_text": {"STOP"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("keyword help_text: status %d, want 422", code)
	}
	if code := save(url.Values{"help_text": {""}}); code != http.StatusSeeOther {
		t.Fatalf("clear: status %d", code)
	}
	if cb := callback(); cb.HelpText != "" || cb.SigningKey != "PSK1" {
		t.Fatalf("after clearing: %+v, want no help text and the key kept", cb)
	}
}

// TestFragment_IntegrationActivity_InboundTexts: texts that arrived through
// an SMS integration appear on its Activity tab, with the sender, the body,
// and whether the matched subscriber's phone is verified.
func TestFragment_IntegrationActivity_InboundTexts(t *testing.T) {
	tc := newTestConsole(t)
	sms := tc.seedIntegration(t, domain.Integration{ProviderID: "twilio", Channel: "sms", Active: true, Identifier: "tw-in", EncCredentials: []byte("fake")})
	if _, _, _, err := tc.st.InboundMessages().RecordSMS(context.Background(), tc.scope, store.InboundMessage{
		IntegrationID: sms.ID, MessageID: "SM1", Sender: "+15551112222", Recipient: "+15559990000",
		Body: "running late, sorry", SubscriberID: "sub-texter",
	}, store.OptNone); err != nil {
		t.Fatalf("RecordSMS: %v", err)
	}
	cookie, _ := tc.login(t)
	rr := tc.getWithQuery(t, "/console/api/fragments/integration-activity", url.Values{"id": {sms.ID}}.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	body := rr.Body.String()
	// html/template writes "+" as "&#43;".
	for _, want := range []string{"Inbound texts", "&#43;15551112222", "running late, sorry", "sub-texter", "unverified"} {
		if !strings.Contains(body, want) {
			t.Errorf("activity missing %q", want)
		}
	}
}
