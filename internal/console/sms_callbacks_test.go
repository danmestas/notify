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

// TestFragment_IntegrationActivity_InboundTexts: texts that arrived through
// an SMS integration appear on its Activity tab, with the sender, the body,
// and whether the matched subscriber's phone is verified.
func TestFragment_IntegrationActivity_InboundTexts(t *testing.T) {
	tc := newTestConsole(t)
	sms := tc.seedIntegration(t, domain.Integration{ProviderID: "twilio", Channel: "sms", Active: true, Identifier: "tw-in", EncCredentials: []byte("fake")})
	if _, _, err := tc.st.InboundMessages().RecordSMS(context.Background(), tc.scope, store.InboundMessage{
		IntegrationID: sms.ID, MessageID: "SM1", Sender: "+15551112222", Recipient: "+15559990000",
		Body: "running late, sorry", SubscriberID: "sub-texter",
	}); err != nil {
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
