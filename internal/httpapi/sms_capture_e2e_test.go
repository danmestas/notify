package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/phoneverify"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/smsloop"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

type capturedEvents struct {
	mu  sync.Mutex
	got []map[string]any
}

func (e *capturedEvents) PublishDelivery(_ string, data []byte) error {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	e.mu.Lock()
	e.got = append(e.got, m)
	e.mu.Unlock()
	return nil
}

func (e *capturedEvents) ofType(typ string) []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []map[string]any
	for _, m := range e.got {
		if m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

type captureFixture struct {
	t      *testing.T
	st     *store.Store
	id     *identity.Service
	conn   *connectivity.Service
	srv    *httptest.Server
	events *capturedEvents
	scope  domain.Scope
	auth   string
	eng    *engine.Engine
	authr  *authoring.Service
}

// newCaptureFixture wires the HTTP surface over a real connectivity service,
// so a capture integration is the only SMS sender. withEngine adds a workflow
// engine whose sms steps send through it.
func newCaptureFixture(t *testing.T, envIdentifier string, withEngine bool) *captureFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mk := bytesRepeat(0x2a, 32)
	id := identity.New(st, mk)
	aud := audience.New(st)
	conn := connectivity.New(st, mk)
	events := &capturedEvents{}
	sms := smsloop.New(st, conn, "https://notify.example.com", slog.New(slog.NewTextHandler(io.Discard, nil))).WithEventPublisher(events)

	f := &captureFixture{t: t, st: st, id: id, conn: conn, events: events}
	var eng *engine.Engine
	if withEngine {
		natsURL := dagtest.StartSidecar(t)
		eng, err = engine.New(natsURL)
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		t.Cleanup(func() { _ = eng.Stop() })
		eng.Handle("notify.send.sms", delivery.NewSMS(st, conn).HandleSMS)
		if err := eng.StartWorkers(ctx); err != nil {
			t.Fatalf("StartWorkers: %v", err)
		}
		f.eng = eng
		f.authr = authoring.New(st, eng)
	}
	var trg *trigger.Service
	if eng != nil {
		trg = trigger.New(aud, st, eng)
	} else {
		trg = trigger.New(aud, st, nil)
	}
	inbox := inboxapi.New(st).WithPhoneVerification(phoneverify.New(st, conn))
	f.srv = httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Store: st,
		Connectivity: conn, SMSLoop: sms,
	}))
	t.Cleanup(f.srv.Close)

	key, scope, err := id.Bootstrap(ctx, "Acme", envIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	f.scope, f.auth = scope, "ApiKey "+key
	return f
}

func (f *captureFixture) createCapture(t *testing.T) (int, string, string) {
	t.Helper()
	status, body := doPost(t, f.srv.URL+"/v1/integrations/sms", f.auth, `{"identifier":"local-capture","provider":"capture","primary":true}`)
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return status, body, out.ID
}

type capturedText struct {
	ID                string `json:"id"`
	ProviderMessageID string `json:"providerMessageId"`
	To                string `json:"to"`
	From              string `json:"from"`
	Body              string `json:"body"`
	SentAt            string `json:"sentAt"`
}

func (f *captureFixture) captured(t *testing.T, auth, integrationID, query string) (int, []capturedText) {
	t.Helper()
	status, body := doGet(t, f.srv.URL+"/v1/integrations/sms/"+integrationID+"/captured"+query, auth)
	var out struct {
		Data []capturedText `json:"data"`
	}
	if status == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode captured: %v: %s", err, body)
		}
	}
	return status, out.Data
}

func (f *captureFixture) simulate(t *testing.T, auth, integrationID, body string) (int, string) {
	t.Helper()
	return doPost(t, f.srv.URL+"/v1/integrations/sms/"+integrationID+"/inbound", auth, body)
}

// TestSMSCapture_WorkflowReachesOnlyAVerifiedPhone: with a capture integration
// as the only sender, a subscriber verifies a phone (the code arrives in
// /captured), an sms workflow step lands there and the message is delivered; a
// subscriber whose phone was only set by the sender gets nothing.
func TestSMSCapture_WorkflowReachesOnlyAVerifiedPhone(t *testing.T) {
	f := newCaptureFixture(t, "dev", true)
	ctx := context.Background()
	status, body, capID := f.createCapture(t)
	if status != http.StatusCreated {
		t.Fatalf("create capture: %d %s", status, body)
	}

	steps := []domain.Step{{StepID: "text", Type: "sms", ControlValues: json.RawMessage(`{"body":"Ready: {{payload.what}}","defaultEnabled":true}`)}}
	if _, err := f.authr.Create(ctx, f.scope, authoring.WorkflowInput{Name: "Ready", TriggerIdentifier: "ready", Steps: steps}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := f.authr.Publish(ctx, f.scope, "ready"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for _, b := range []string{
		`{"subscriberId":"verified","email":"v@acme.io"}`,
		`{"subscriberId":"api-phone","email":"a@acme.io","phone":"+15553334444"}`,
	} {
		if status, resp := doPost(t, f.srv.URL+"/v1/subscribers", f.auth, b); status != http.StatusCreated {
			t.Fatalf("create subscriber: %d %s", status, resp)
		}
	}

	tok, _ := f.id.MintSubscriberSession(f.scope, "verified")
	if status, resp := req(t, http.MethodPost, f.srv.URL+"/v1/me/phone/verification", "Bearer "+tok, `{"phone":"+15551112222"}`); status != http.StatusAccepted {
		t.Fatalf("request code: %d %s", status, resp)
	}
	_, texts := f.captured(t, f.auth, capID, "")
	if len(texts) != 1 {
		t.Fatalf("captured after code request = %+v, want the code text", texts)
	}
	code := sixDigits.FindString(texts[0].Body)
	if code == "" || texts[0].To != "+15551112222" || texts[0].From != connectivity.DefaultCaptureFrom {
		t.Fatalf("code text = %+v", texts[0])
	}
	if status, resp := req(t, http.MethodPost, f.srv.URL+"/v1/me/phone/verification/confirm", "Bearer "+tok, `{"code":"`+code+`"}`); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, resp)
	}

	for _, sid := range []string{"verified", "api-phone"} {
		trig := `{"name":"ready","to":{"subscriberId":"` + sid + `"},"payload":{"what":"bike"},"transactionId":"tx-` + sid + `"}`
		if status, resp := doPost(t, f.srv.URL+"/v1/events/trigger", f.auth, trig); status != http.StatusCreated {
			t.Fatalf("trigger %s: %d %s", sid, status, resp)
		}
	}
	smsMessage := func(sid string) (domain.Message, bool) {
		msgs, err := f.st.Messages().List(ctx, f.scope, sid, "sms")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.WorkflowID != "" {
				return m, true
			}
		}
		return domain.Message{}, false
	}
	deadline := time.Now().Add(30 * time.Second)
	var vm, am domain.Message
	for {
		var okV, okA bool
		vm, okV = smsMessage("verified")
		am, okA = smsMessage("api-phone")
		if okV && okA {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runs did not reach the sms step within 30s")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if vm.Status != "delivered" || vm.ProviderMessageID == "" {
		t.Errorf("verified sms: status %q provider id %q, want delivered with an id", vm.Status, vm.ProviderMessageID)
	}
	if am.Status != "skipped" || am.SkipReason != delivery.SkipReasonNoVerifiedPhone {
		t.Errorf("unverified sms: status %q skip %q, want skipped no_verified_phone", am.Status, am.SkipReason)
	}

	_, texts = f.captured(t, f.auth, capID, "")
	if len(texts) != 2 {
		t.Fatalf("captured = %+v, want the code and one workflow text", texts)
	}
	got := texts[0] // newest first
	if got.Body != "Ready: bike" || got.To != "+15551112222" || got.ProviderMessageID != vm.ProviderMessageID || got.SentAt == "" || got.ID == "" {
		t.Errorf("workflow text = %+v, message provider id %q", got, vm.ProviderMessageID)
	}
	for _, c := range texts {
		if c.To == "+15553334444" {
			t.Errorf("the sender-set phone was captured: %+v", c)
		}
	}
	if _, one := f.captured(t, f.auth, capID, "?limit=1"); len(one) != 1 || one[0].Body != "Ready: bike" {
		t.Errorf("limit=1 = %+v", one)
	}
	if status, _ := f.captured(t, f.auth, capID, "?limit=abc"); status != http.StatusBadRequest {
		t.Errorf("limit=abc: %d, want 400", status)
	}
}

// TestSMSCapture_SimulatedInbound: STOP opts a number out and START back in,
// each telling the tenant; HELP is answered into /captured; free text is kept.
func TestSMSCapture_SimulatedInbound(t *testing.T) {
	f := newCaptureFixture(t, "dev", false)
	ctx := context.Background()
	status, body, capID := f.createCapture(t)
	if status != http.StatusCreated {
		t.Fatalf("create capture: %d %s", status, body)
	}
	if status, resp := doPost(t, f.srv.URL+"/v1/subscribers", f.auth, `{"subscriberId":"cust","email":"c@acme.io","phone":"+15125550123"}`); status != http.StatusCreated {
		t.Fatalf("create subscriber: %d %s", status, resp)
	}
	const phone = "+15125550123"

	post := func(text string) map[string]any {
		t.Helper()
		status, resp := f.simulate(t, f.auth, capID, `{"from":"+1 (512) 555-0123","body":"`+text+`"}`)
		if status != http.StatusOK {
			t.Fatalf("simulate %q: %d %s", text, status, resp)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(resp), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := post("hello there")
	if out["keyword"] != "none" || out["optChanged"] != false || out["inboundMessageId"] == "" {
		t.Errorf("free text = %v", out)
	}
	out = post("STOP")
	if out["keyword"] != "opt_out" || out["optChanged"] != true {
		t.Errorf("STOP = %v", out)
	}
	if opted, err := f.st.SMSOptOuts().IsOptedOut(ctx, f.scope, phone); err != nil || !opted {
		t.Errorf("after STOP opted out = %v, %v", opted, err)
	}
	if evs := f.events.ofType(smsloop.EventOptOut); len(evs) != 1 || evs[0]["subscriberId"] != "cust" {
		t.Errorf("sms.opt_out events = %v", evs)
	}
	out = post("START")
	if out["keyword"] != "opt_in" || out["optChanged"] != true {
		t.Errorf("START = %v", out)
	}
	if opted, err := f.st.SMSOptOuts().IsOptedOut(ctx, f.scope, phone); err != nil || opted {
		t.Errorf("after START opted out = %v, %v", opted, err)
	}
	if evs := f.events.ofType(smsloop.EventOptIn); len(evs) != 1 {
		t.Errorf("sms.opt_in events = %v", evs)
	}
	if evs := f.events.ofType(smsloop.EventInbound); len(evs) != 3 {
		t.Errorf("sms.inbound events = %d, want 3", len(evs))
	}

	if _, texts := f.captured(t, f.auth, capID, ""); len(texts) != 0 {
		t.Fatalf("captured before HELP = %+v", texts)
	}
	out = post("HELP")
	if out["keyword"] != "help" {
		t.Errorf("HELP = %v", out)
	}
	_, texts := f.captured(t, f.auth, capID, "")
	if len(texts) != 1 || texts[0].To != phone || texts[0].From != connectivity.DefaultCaptureFrom || !strings.Contains(texts[0].Body, "STOP") {
		t.Errorf("HELP reply captured = %+v", texts)
	}

	if status, _ := f.simulate(t, f.auth, capID, `{"body":"x"}`); status != http.StatusBadRequest {
		t.Errorf("simulate without from: %d, want 400", status)
	}
}

// TestSMSCapture_RefusedInProduction: an environment flagged production, by
// type or by identifier, gets no capture integration and no simulated text.
func TestSMSCapture_RefusedInProduction(t *testing.T) {
	f := newCaptureFixture(t, "production", false)
	if status, body, _ := f.createCapture(t); status != http.StatusUnprocessableEntity {
		t.Fatalf("create in production by identifier: %d %s, want 422", status, body)
	}
	if status, body := doPost(t, f.srv.URL+"/v1/integrations", f.auth, `{"providerId":"capture","identifier":"c2"}`); status != http.StatusUnprocessableEntity {
		t.Fatalf("unified create in production: %d %s, want 422", status, body)
	}

	if status, body := f.simulate(t, f.auth, "any-id", `{"from":"+15125550123","body":"hi"}`); status != http.StatusUnprocessableEntity {
		t.Fatalf("simulate in production: %d %s, want 422", status, body)
	}
}

// TestSMSCapture_CarrierRouteRefusesCapture: the public signed route answers a
// capture integration id like an unknown one: capture has no signature.
func TestSMSCapture_CarrierRouteRefusesCapture(t *testing.T) {
	f := newCaptureFixture(t, "dev", false)
	_, _, capID := f.createCapture(t)
	form := url.Values{"MessageSid": {"SM1"}, "From": {"+15551112222"}, "To": {"+15550100000"}, "Body": {"STOP"}}
	for _, path := range []string{"/v1/inbound/sms/" + capID, "/v1/inbound/sms/" + capID + "/status"} {
		resp, err := http.Post(f.srv.URL+path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", path, resp.StatusCode)
		}
	}
}

// TestSMSCapture_ScopedAndCaptureOnly: another tenant, and any integration
// that is not a capture one, answer 404 on both routes.
func TestSMSCapture_ScopedAndCaptureOnly(t *testing.T) {
	f := newCaptureFixture(t, "dev", false)
	_, _, capID := f.createCapture(t)
	status, body := doPost(t, f.srv.URL+"/v1/integrations/sms", f.auth,
		`{"identifier":"real","provider":"twilio","accountSid":"AC1","authToken":"tok","from":"+15559990000"}`)
	if status != http.StatusCreated {
		t.Fatalf("create twilio: %d %s", status, body)
	}
	var tw struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &tw)

	otherKey, _, err := f.id.Bootstrap(context.Background(), "Other", "dev")
	if err != nil {
		t.Fatal(err)
	}
	other := "ApiKey " + otherKey

	for name, c := range map[string]struct{ auth, id string }{
		"other tenant": {other, capID},
		"twilio":       {f.auth, tw.ID},
		"unknown":      {f.auth, "nope"},
	} {
		if status, _ := f.captured(t, c.auth, c.id, ""); status != http.StatusNotFound {
			t.Errorf("%s captured: %d, want 404", name, status)
		}
		if status, body := f.simulate(t, c.auth, c.id, `{"from":"+15125550123","body":"STOP"}`); status != http.StatusNotFound {
			t.Errorf("%s simulate: %d %s, want 404", name, status, body)
		}
	}
	if status, _ := f.captured(t, f.auth, capID, ""); status != http.StatusOK {
		t.Errorf("own capture: %d, want 200", status)
	}
}

// TestSMSCapture_VerifyAndDelete: a capture integration verifies with no
// network, and deleting it removes its texts.
func TestSMSCapture_VerifyAndDelete(t *testing.T) {
	f := newCaptureFixture(t, "dev", false)
	ctx := context.Background()
	_, _, capID := f.createCapture(t)
	res, err := f.conn.VerifyByID(ctx, f.scope, capID)
	if err != nil || !res.OK {
		t.Fatalf("VerifyByID = %+v, %v, want ok", res, err)
	}
	in, err := f.st.Integrations().GetByID(ctx, f.scope, capID)
	if err != nil {
		t.Fatal(err)
	}
	prov, err := f.conn.SMSProviderFromIntegration(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Send(ctx, providers.SMSMessage{To: "+15551112222", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, texts := f.captured(t, f.auth, capID, ""); len(texts) != 1 {
		t.Fatalf("captured = %+v", texts)
	}
	if status, body := req(t, http.MethodDelete, f.srv.URL+"/v1/integrations/"+capID, f.auth, ""); status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", status, body)
	}
	rows, err := f.st.SMSCaptured().List(ctx, f.scope, capID, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows after delete = %+v, %v", rows, err)
	}
}
