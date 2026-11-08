package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/phoneverify"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// capturePushResolver hands push sends to a recording provider as "ntfy", so
// the handler addresses each subscriber by their ntfy topic.
type capturePushResolver struct{ p *providers.CapturePush }

func (r capturePushResolver) ResolvePush(context.Context, domain.Scope) (providers.PushProvider, string, string, error) {
	return r.p, "ntfy", "", nil
}

func (r capturePushResolver) ResolveIntegration(ctx context.Context, scope domain.Scope, _, _ string) (any, string, string, error) {
	return r.ResolvePush(ctx, scope)
}

// TestSMSStep_SignOffReachesOnlyAVerifiedPhone is #130's acceptance: a
// sign-off workflow in_app -> push -> email -> sms over a real engine, store
// and HTTP surface, with recording providers in place of ntfy, mail and
// Twilio. The subscriber who verified a phone (through their own session) gets
// all four; one with no phone, and one whose phone only the sender set, get the
// first three and a skipped sms, and no step errors.
func TestSMSStep_SignOffReachesOnlyAVerifiedPhone(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	pushCapt := providers.NewCapturePush()
	mailCapt := providers.NewCapture()
	smsCapt := providers.NewCaptureSMS()
	sms := fakeSMSResolver{smsCapt}
	prefs := preferences.New(st)

	eng.Handle("notify.send.in_app", delivery.New(st).WithPreferences(prefs).HandleInApp)
	eng.Handle("notify.send.push", delivery.NewPush(st, capturePushResolver{pushCapt}).WithPreferences(prefs).HandlePush)
	eng.Handle("notify.send.email", delivery.NewEmail(st, capResolver{c: mailCapt, from: "noreply@acme.io"}).WithPreferences(prefs).HandleEmail)
	eng.Handle("notify.send.sms", delivery.NewSMS(st, sms).WithPreferences(prefs).HandleSMS)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	id := identity.New(st, bytesRepeat(0x2a, 32))
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st).WithPhoneVerification(phoneverify.New(st, sms))
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Store: st}))
	t.Cleanup(srv.Close)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	apiKey := "ApiKey " + key

	// A sign-off: its sms step is on by default.
	steps := []domain.Step{
		{StepID: "inbox", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Sign-off needed","body":"{{payload.what}}"}`)},
		{StepID: "push", Type: "push", ControlValues: json.RawMessage(`{"subject":"Sign-off needed","body":"{{payload.what}}"}`)},
		{StepID: "mail", Type: "email", ControlValues: json.RawMessage(`{"subject":"Sign-off needed","body":"{{payload.what}}"}`)},
		{StepID: "text", Type: "sms", ControlValues: json.RawMessage(`{"body":"Sign-off needed: {{payload.link}}","defaultEnabled":true}`)},
	}
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{Name: "Sign-off", TriggerIdentifier: "quarry-signoff", Steps: steps}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "quarry-signoff"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	subs := map[string]string{
		"verified":  `{"subscriberId":"verified","email":"v@acme.io"}`,
		"no-phone":  `{"subscriberId":"no-phone","email":"n@acme.io"}`,
		"api-phone": `{"subscriberId":"api-phone","email":"a@acme.io","phone":"+15553334444"}`,
	}
	for sid, body := range subs {
		if status, resp := doPost(t, srv.URL+"/v1/subscribers", apiKey, body); status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", sid, status, resp)
		}
		cred := `{"providerId":"ntfy","channel":"push","credentials":{"topic":"topic-` + sid + `"}}`
		if status, resp := doPut(t, srv.URL+"/v1/subscribers/"+sid+"/credentials", apiKey, cred); status != http.StatusNoContent {
			t.Fatalf("credentials %s: %d %s", sid, status, resp)
		}
	}

	// "verified" verifies their own phone through a subscriber session.
	tok, _ := id.MintSubscriberSession(scope, "verified")
	if status, resp := req(t, http.MethodPost, srv.URL+"/v1/me/phone/verification", "Bearer "+tok, `{"phone":"+15551112222"}`); status != http.StatusAccepted {
		t.Fatalf("request code: %d %s", status, resp)
	}
	code := lastCode(t, smsCapt, "+15551112222")
	if status, resp := req(t, http.MethodPost, srv.URL+"/v1/me/phone/verification/confirm", "Bearer "+tok, `{"code":"`+code+`"}`); status != http.StatusOK {
		t.Fatalf("confirm code: %d %s", status, resp)
	}
	codesSent := len(smsCapt.Sent())

	for sid := range subs {
		body := `{"name":"quarry-signoff","to":{"subscriberId":"` + sid + `"},"payload":{"what":"Approve deploy","link":"https://q.example/s/1"},"transactionId":"tx-` + sid + `"}`
		if status, resp := doPost(t, srv.URL+"/v1/events/trigger", apiKey, body); status != http.StatusCreated {
			t.Fatalf("trigger %s: %d %s", sid, status, resp)
		}
	}

	// The sms step is last, so once each subscriber has an sms message their
	// run has finished.
	byChannel := func(sid string) map[string]domain.Message {
		out := map[string]domain.Message{}
		for _, ch := range []string{"in_app", "push", "email", "sms"} {
			msgs, err := st.Messages().List(ctx, scope, sid, ch)
			if err != nil {
				t.Fatalf("list %s/%s: %v", sid, ch, err)
			}
			for _, m := range msgs {
				if m.WorkflowID != "" { // not the verification code's record
					out[ch] = m
				}
			}
		}
		return out
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		done := true
		for sid := range subs {
			if _, ok := byChannel(sid)["sms"]; !ok {
				done = false
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			for sid := range subs {
				t.Logf("%s: %+v", sid, byChannel(sid))
			}
			t.Fatal("runs did not reach the sms step within 30s")
		}
		time.Sleep(200 * time.Millisecond)
	}

	for sid := range subs {
		got := byChannel(sid)
		for _, ch := range []string{"in_app", "push", "email"} {
			if m := got[ch]; m.Status != "sent" {
				t.Errorf("%s %s: status %q (%s), want sent", sid, ch, m.Status, m.ErrorText)
			}
		}
		m := got["sms"]
		if sid == "verified" {
			if m.Status != "sent" {
				t.Errorf("verified sms: status %q skip=%q err=%q, want sent", m.Status, m.SkipReason, m.ErrorText)
			}
			continue
		}
		if m.Status != "skipped" || m.SkipReason != delivery.SkipReasonNoVerifiedPhone || m.ErrorText != "" {
			t.Errorf("%s sms = status %q skip %q err %q, want skipped (no_verified_phone) with no error", sid, m.Status, m.SkipReason, m.ErrorText)
		}
	}

	// Exactly one step sms went out, to the verified phone, carrying the link.
	steps2 := smsCapt.Sent()[codesSent:]
	if len(steps2) != 1 || steps2[0].To != "+15551112222" || steps2[0].Body != "Sign-off needed: https://q.example/s/1" {
		t.Fatalf("step sms sent = %+v, want one to +15551112222 with the link", steps2)
	}
	if got := len(pushCapt.Sent()); got != 3 {
		t.Errorf("push sent = %d, want 3", got)
	}
	if got := len(mailCapt.Sent()); got != 3 {
		t.Errorf("email sent = %d, want 3", got)
	}
}
