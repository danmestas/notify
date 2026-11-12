package smsloop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/providers/twilio"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

const (
	publicURL = "https://notify.example.com/base"
	ourNumber = "+15559990000"
	token     = "twilio-auth-token"
	swKey     = "PSK-signing-key"
)

var (
	envA = domain.Scope{OrgID: "o1", EnvID: "envA"}
	envB = domain.Scope{OrgID: "o2", EnvID: "envB"}
)

// fakeCallbacks stands in for connectivity: each integration id resolves to
// its scope, provider and signing key, with a capturing sender.
type fakeCallbacks map[string]connectivity.SMSCallback

func (f fakeCallbacks) SMSCallback(_ context.Context, id string) (connectivity.SMSCallback, error) {
	cb, ok := f[id]
	if !ok {
		return connectivity.SMSCallback{}, store.ErrNotFound
	}
	return cb, nil
}

type events struct {
	mu  sync.Mutex
	got []map[string]any
}

func (e *events) PublishDelivery(envID string, data []byte) error {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if m["EnvID"] != envID {
		return errors.New("event published under another environment")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, m)
	return nil
}

func (e *events) ofType(typ string) []map[string]any {
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

// flakySender fails every send while fail is set, else captures it.
type flakySender struct {
	*providers.CaptureSMS
	fail atomic.Bool
}

func (f *flakySender) Send(ctx context.Context, msg providers.SMSMessage) (providers.Result, error) {
	if f.fail.Load() {
		return providers.Result{}, errors.New("provider down")
	}
	return f.CaptureSMS.Send(ctx, msg)
}

type rig struct {
	t      *testing.T
	flaky  *flakySender
	st     *store.Store
	prefs  *preferences.Service
	ev     *events
	sender *providers.CaptureSMS
	srv    *httptest.Server
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &rig{t: t, st: st, prefs: preferences.New(st), ev: &events{}, sender: providers.NewCaptureSMS()}
	r.flaky = &flakySender{CaptureSMS: r.sender}
	cbs := fakeCallbacks{
		"int-tw": {Scope: envA, IntegrationID: "int-tw", ProviderID: "twilio", SigningKey: token, From: ourNumber, Sender: r.flaky},
		"int-sw": {
			Scope: envB, IntegrationID: "int-sw", ProviderID: "signalwire", SigningKey: swKey, From: ourNumber,
			HelpText: "Acme alerts. Reply STOP to stop.", Sender: r.flaky,
		},
		"int-nokey": {Scope: envB, IntegrationID: "int-nokey", ProviderID: "signalwire", From: ourNumber, Sender: r.flaky},
	}
	svc := New(st, cbs, publicURL, slog.New(slog.NewTextHandler(io.Discard, nil))).WithEventPublisher(r.ev)
	router := chi.NewRouter()
	svc.Routes(router)
	r.srv = httptest.NewServer(router)
	t.Cleanup(r.srv.Close)
	return r
}

// post sends form to path, signed as provider would sign it for the public
// URL (unless sig overrides it), and returns the status.
func (r *rig) post(path string, form url.Values, header, sig string) int {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if header != "" {
		req.Header.Set(header, sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("POST %s: %v", path, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func (r *rig) twilio(path string, form url.Values) int {
	r.t.Helper()
	return r.post(path, form, twilio.SignatureHeader, twilio.Signature(token, publicURL+path, form))
}

func text(sid, from, body string) url.Values {
	return url.Values{"MessageSid": {sid}, "From": {from}, "To": {ourNumber}, "Body": {body}, "AccountSid": {"AC1"}}
}

func (r *rig) subscriber(scope domain.Scope, id, phone string, verified bool) {
	r.t.Helper()
	ctx := context.Background()
	if _, err := r.st.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: id, Phone: phone}); err != nil {
		r.t.Fatalf("upsert: %v", err)
	}
	if verified {
		if err := r.st.Subscribers().SetVerifiedPhone(ctx, scope, id, phone, "2026-11-01T00:00:00Z"); err != nil {
			r.t.Fatalf("verify: %v", err)
		}
	}
}

func TestInbound_SignatureIsRequired(t *testing.T) {
	r := newRig(t)
	path := "/v1/inbound/sms/int-tw"
	form := text("SM1", "+15551112222", "hello")
	good := twilio.Signature(token, publicURL+path, form)
	for name, c := range map[string]struct{ header, sig string }{
		"missing":             {"", ""},
		"wrong token":         {twilio.SignatureHeader, twilio.Signature("other", publicURL+path, form)},
		"signed for the host": {twilio.SignatureHeader, twilio.Signature(token, r.srv.URL+path, form)},
	} {
		if got := r.post(path, form, c.header, c.sig); got != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, got)
		}
	}
	if got := r.post("/v1/inbound/sms/no-such", form, twilio.SignatureHeader, good); got != http.StatusForbidden {
		t.Errorf("unknown integration: status %d, want 403", got)
	}
	if got := r.post(path, form, twilio.SignatureHeader, good); got != http.StatusOK {
		t.Fatalf("correct signature: status %d, want 200", got)
	}
	if n := len(r.ev.ofType(EventInbound)); n != 1 {
		t.Fatalf("%d inbound events, want 1 (only the signed request)", n)
	}
}

func TestInbound_SignalWire(t *testing.T) {
	r := newRig(t)
	path := "/v1/inbound/sms/int-sw"
	form := text("SW1", "+15551112222", "hello")
	raw := form.Encode()
	if got := r.post(path, form, "X-SignalWire-Signature", hexSig(swKey, publicURL+path, raw)); got != http.StatusOK {
		t.Fatalf("signed: status %d, want 200", got)
	}
	if got := r.post(path, form, "X-SignalWire-Signature", hexSig("wrong", publicURL+path, raw)); got != http.StatusForbidden {
		t.Fatalf("wrong key: status %d, want 403", got)
	}
	// An integration with no signing key cannot be verified, so nothing is
	// accepted for it.
	nokey := "/v1/inbound/sms/int-nokey"
	if got := r.post(nokey, form, "X-SignalWire-Signature", hexSig("", publicURL+nokey, raw)); got != http.StatusForbidden {
		t.Fatalf("no signing key: status %d, want 403", got)
	}
}

func TestInbound_KnownAndUnknownSender(t *testing.T) {
	r := newRig(t)
	r.subscriber(envA, "ada", "+15551112222", true)
	r.subscriber(envA, "bob", "+15553334444", false)
	path := "/v1/inbound/sms/int-tw"

	for _, c := range []struct {
		sid, from, wantSub string
		verified           bool
	}{
		{"SM1", "+1 (555) 111-2222", "ada", true},
		{"SM2", "+15553334444", "bob", false},
		{"SM3", "+15550000000", "", false},
	} {
		if got := r.twilio(path, text(c.sid, c.from, "hi "+c.sid)); got != http.StatusOK {
			t.Fatalf("%s: status %d", c.sid, got)
		}
	}
	evs := r.ev.ofType(EventInbound)
	if len(evs) != 3 {
		t.Fatalf("%d inbound events, want 3", len(evs))
	}
	want := []struct {
		sub, from string
		verified  bool
	}{{"ada", "+15551112222", true}, {"bob", "+15553334444", false}, {"", "+15550000000", false}}
	for i, w := range want {
		e := evs[i]
		if sub, _ := e["subscriberId"].(string); sub != w.sub {
			t.Errorf("event %d subscriberId = %q, want %q", i, sub, w.sub)
		}
		if e["from"] != w.from || e["to"] != ourNumber || e["body"] == "" || e["integrationId"] != "int-tw" {
			t.Errorf("event %d = %v", i, e)
		}
		if v, _ := e["phoneVerified"].(bool); v != w.verified {
			t.Errorf("event %d phoneVerified = %v, want %v", i, v, w.verified)
		}
	}
	if _, ok := evs[2]["subscriberId"]; ok {
		t.Error("an unknown sender's event carries a subscriberId key")
	}

	msgs, err := r.st.InboundMessages().List(context.Background(), envA, 0, 50)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("stored %d inbound messages, %v; want 3", len(msgs), err)
	}
	if m := msgs[0]; m.Channel != "sms" || m.Sender != "+15551112222" || m.SubscriberID != "ada" || !m.PhoneVerified || m.Body != "hi SM1" {
		t.Fatalf("stored = %+v", m)
	}

	// A provider retry of the same message adds nothing.
	if got := r.twilio(path, text("SM1", "+15551112222", "hi SM1")); got != http.StatusOK {
		t.Fatalf("retry: status %d", got)
	}
	if n := len(r.ev.ofType(EventInbound)); n != 3 {
		t.Fatalf("retry emitted again: %d events", n)
	}
}

// smsStep runs one sms step for subscriber ada in envA through the real
// delivery handler with the real preference service, the workflow turning
// sms on by default, and returns the message it recorded.
func (r *rig) smsStep(capt *providers.CaptureSMS, defaultOn bool) domain.Message {
	r.t.Helper()
	env := runenv.InAppEnvelope{
		Scope:        runenv.Scope{OrgID: envA.OrgID, EnvID: envA.EnvID},
		WorkflowID:   "wf1",
		SubscriberID: "ada",
		Step:         runenv.Step{StepID: "S1", Channel: "sms", BodyTemplate: "hello", DefaultEnabled: defaultOn},
	}
	input, _ := json.Marshal(env)
	h := delivery.NewSMS(r.st, resolver{capt}).WithPreferences(r.prefs)
	out, err := h.HandleSMS(context.Background(), engine.TaskPayload{RunID: "run", StepID: "S1", Input: input})
	if err != nil {
		r.t.Fatalf("HandleSMS: %v", err)
	}
	var m domain.Message
	_ = json.Unmarshal(out, &m)
	return m
}

type resolver struct{ p providers.SMSProvider }

func (s resolver) ResolveSMS(context.Context, domain.Scope) (providers.SMSProvider, string, string, error) {
	return s.p, "twilio", "int-tw", nil
}

func (s resolver) ResolveIntegration(context.Context, domain.Scope, string, string) (any, string, string, error) {
	return s.p, "twilio", "int-tw", nil
}

func TestKeywords_StopThenStart(t *testing.T) {
	r := newRig(t)
	r.subscriber(envA, "ada", "+15551112222", true)
	path := "/v1/inbound/sms/int-tw"
	capt := providers.NewCaptureSMS()

	if m := r.smsStep(capt, true); m.Status != "sent" {
		t.Fatalf("before STOP: %+v, want sent", m)
	}
	if got := r.twilio(path, text("SM1", "+15551112222", "  stop ")); got != http.StatusOK {
		t.Fatalf("STOP: status %d", got)
	}
	out := r.ev.ofType(EventOptOut)
	if len(out) != 1 || out[0]["subscriberId"] != "ada" || out[0]["from"] != "+15551112222" || out[0]["keyword"] != "STOP" {
		t.Fatalf("opt-out events = %v", out)
	}
	if m := r.smsStep(capt, true); m.Status != "skipped" || m.SkipReason != delivery.SkipReasonOptedOut {
		t.Fatalf("after STOP: %+v, want skipped opted_out", m)
	}
	// A workflow whose sms is off by default is skipped for that, not the
	// opt-out, once START lifts it; meanwhile the opt-out wins.
	if m := r.smsStep(capt, false); m.SkipReason != delivery.SkipReasonOptedOut {
		t.Fatalf("default-off workflow after STOP: %+v, want opted_out", m)
	}

	if got := r.twilio(path, text("SM2", "+15551112222", "Start")); got != http.StatusOK {
		t.Fatalf("START: status %d", got)
	}
	if in := r.ev.ofType(EventOptIn); len(in) != 1 || in[0]["subscriberId"] != "ada" || in[0]["keyword"] != "START" {
		t.Fatalf("opt-in events = %v", in)
	}
	if m := r.smsStep(capt, true); m.Status != "sent" {
		t.Fatalf("after START: %+v, want sent", m)
	}
	// START restores exactly the earlier state: it does not turn sms on for
	// a workflow that has it off by default.
	if m := r.smsStep(capt, false); m.Status != "skipped" || m.SkipReason != delivery.SkipReasonPreference {
		t.Fatalf("default-off workflow after START: %+v, want skipped by preference", m)
	}
	if len(capt.Sent()) != 2 {
		t.Fatalf("sent %d, want 2 (before STOP and after START)", len(capt.Sent()))
	}
	if rows, _ := r.prefs.GetForSubscriber(context.Background(), envA, "ada"); len(rows) != 0 {
		t.Fatalf("keywords wrote preferences: %+v", rows)
	}
}

// TestKeywords_StartWithoutStopChangesNothing: a YES that answers some other
// question, from a number that never opted out, is just a text.
func TestKeywords_StartWithoutStopChangesNothing(t *testing.T) {
	r := newRig(t)
	r.subscriber(envA, "ada", "+15551112222", true)
	capt := providers.NewCaptureSMS()
	if got := r.twilio("/v1/inbound/sms/int-tw", text("Y1", "+15551112222", "YES")); got != http.StatusOK {
		t.Fatalf("YES: status %d", got)
	}
	if n := len(r.ev.ofType(EventOptIn)); n != 0 {
		t.Fatalf("%d opt-in events, want none", n)
	}
	if n := len(r.ev.ofType(EventInbound)); n != 1 {
		t.Fatalf("%d inbound events, want the text itself", n)
	}
	if rows, _ := r.prefs.GetForSubscriber(context.Background(), envA, "ada"); len(rows) != 0 {
		t.Fatalf("YES wrote preferences: %+v", rows)
	}
	if m := r.smsStep(capt, false); m.SkipReason != delivery.SkipReasonPreference {
		t.Fatalf("default-off workflow after YES: %+v, want still skipped by preference", m)
	}
}

// TestKeywords_OwnPreferenceSurvivesStopStart: a subscriber who turned sms
// off themselves stays off through STOP and START.
func TestKeywords_OwnPreferenceSurvivesStopStart(t *testing.T) {
	r := newRig(t)
	r.subscriber(envA, "ada", "+15551112222", true)
	capt := providers.NewCaptureSMS()
	if err := r.prefs.SetGlobal(context.Background(), envA, "ada", map[string]bool{"sms": false}); err != nil {
		t.Fatalf("SetGlobal: %v", err)
	}
	r.twilio("/v1/inbound/sms/int-tw", text("S1", "+15551112222", "STOP"))
	r.twilio("/v1/inbound/sms/int-tw", text("S2", "+15551112222", "START"))
	if on, _ := r.prefs.ChannelEnabled(context.Background(), envA, "ada", "wf1", "sms", false, true); on {
		t.Fatal("the subscriber's own sms=false did not survive STOP and START")
	}
	if m := r.smsStep(capt, true); m.SkipReason != delivery.SkipReasonPreference {
		t.Fatalf("after STOP and START: %+v, want skipped by preference", m)
	}
}

// TestKeywords_FailedOptOutIsAppliedOnRetry: if recording the opt-out fails,
// the text is not kept either, so the provider's retry applies it.
func TestKeywords_FailedOptOutIsAppliedOnRetry(t *testing.T) {
	r := newRig(t)
	r.subscriber(envA, "ada", "+15551112222", true)
	writer, _ := r.st.Pools()
	ctx := context.Background()
	if _, err := writer.ExecContext(ctx, `CREATE TRIGGER fail_opt_out BEFORE INSERT ON sms_opt_outs
		BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	stop := text("SMstop", "+15551112222", "STOP")
	if got := r.twilio("/v1/inbound/sms/int-tw", stop); got != http.StatusInternalServerError {
		t.Fatalf("STOP with a failing opt-out: status %d, want 500 so the provider retries", got)
	}
	if n := len(r.ev.ofType(EventInbound)) + len(r.ev.ofType(EventOptOut)); n != 0 {
		t.Fatalf("%d events for a STOP that was not applied", n)
	}
	if _, err := writer.ExecContext(ctx, `DROP TRIGGER fail_opt_out`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if got := r.twilio("/v1/inbound/sms/int-tw", stop); got != http.StatusOK {
		t.Fatalf("retry: status %d", got)
	}
	if out, _ := r.st.SMSOptOuts().IsOptedOut(ctx, envA, "+15551112222"); !out {
		t.Fatal("the retried STOP was not applied")
	}
	if n := len(r.ev.ofType(EventOptOut)); n != 1 {
		t.Fatalf("%d opt-out events, want 1", n)
	}
}

func TestKeywords_Synonyms(t *testing.T) {
	r := newRig(t)
	path := "/v1/inbound/sms/int-tw"
	for i, kw := range []string{"STOPALL", "unsubscribe", "Cancel", "END", "quit"} {
		r.twilio(path, text("OUT"+kw, "+1555000000"+string(rune('0'+i)), kw))
	}
	if n := len(r.ev.ofType(EventOptOut)); n != 5 {
		t.Fatalf("%d opt-outs, want 5", n)
	}
	for i, kw := range []string{"yes", "UNSTOP"} {
		r.twilio(path, text("IN"+kw, "+1555000000"+string(rune('0'+i)), kw))
	}
	if n := len(r.ev.ofType(EventOptIn)); n != 2 {
		t.Fatalf("%d opt-ins, want 2", n)
	}
	// A keyword inside a sentence is just a text.
	r.twilio(path, text("SENT", "+15550000009", "please stop by later"))
	if n := len(r.ev.ofType(EventOptOut)); n != 5 {
		t.Fatalf("a sentence opted out: %d opt-outs", n)
	}
	// Unknown senders' opt-outs carry no subscriber but are still told.
	if _, ok := r.ev.ofType(EventOptOut)[0]["subscriberId"]; ok {
		t.Fatal("an unknown sender's opt-out carries a subscriberId")
	}
}

func TestKeywords_HelpReplies(t *testing.T) {
	r := newRig(t)
	// An unverified sender still gets the reply: it answers the sender.
	r.subscriber(envA, "bob", "+15553334444", false)
	if got := r.twilio("/v1/inbound/sms/int-tw", text("H1", "+15553334444", "help")); got != http.StatusOK {
		t.Fatalf("HELP: status %d", got)
	}
	sent := r.sender.Sent()
	if len(sent) != 1 || sent[0].To != "+15553334444" || sent[0].From != ourNumber || !strings.Contains(sent[0].Body, ourNumber) {
		t.Fatalf("default HELP reply = %+v", sent)
	}
	// The reply is recorded as an sms message carrying the provider's id.
	msgs, _ := r.st.Messages().List(context.Background(), envA, "bob", "sms")
	if len(msgs) != 1 || msgs[0].Status != "sent" || msgs[0].ProviderMessageID == "" || msgs[0].IntegrationID != "int-tw" {
		t.Fatalf("recorded reply = %+v", msgs)
	}

	form := text("H2", "+15551112222", "INFO")
	path := "/v1/inbound/sms/int-sw"
	if got := r.post(path, form, "X-SignalWire-Signature", hexSig(swKey, publicURL+path, form.Encode())); got != http.StatusOK {
		t.Fatalf("INFO: status %d", got)
	}
	if sent := r.sender.Sent(); len(sent) != 2 || sent[1].Body != "Acme alerts. Reply STOP to stop." {
		t.Fatalf("configured HELP reply = %+v", sent)
	}
	// The reply asks for its own delivery status, like any send.
	if cb := r.sender.Sent()[0].StatusCallback; cb != publicURL+"/v1/inbound/sms/int-tw/status" {
		t.Fatalf("HELP reply StatusCallback = %q", cb)
	}
}

// TestKeywords_HelpIsRateLimited: one HELP reply per number per integration
// per 10 minutes, however often the number asks.
func TestKeywords_HelpIsRateLimited(t *testing.T) {
	r := newRig(t)
	for i, sid := range []string{"H1", "H2", "H3"} {
		if got := r.twilio("/v1/inbound/sms/int-tw", text(sid, "+15553334444", "HELP")); got != http.StatusOK {
			t.Fatalf("HELP #%d: status %d", i, got)
		}
	}
	r.twilio("/v1/inbound/sms/int-tw", text("H4", "+15551112222", "HELP")) // another number
	sent := r.sender.Sent()
	if len(sent) != 2 || sent[0].To != "+15553334444" || sent[1].To != "+15551112222" {
		t.Fatalf("replies = %+v, want one per number", sent)
	}
	if n := len(r.ev.ofType(EventInbound)); n != 4 {
		t.Fatalf("%d inbound events, want every text delivered", n)
	}
}

// TestStatus_BeforeTheMessage: a failed callback that beats the send's
// message row is kept and applied when the row is written.
func TestStatus_BeforeTheMessage(t *testing.T) {
	r := newRig(t)
	if got := r.twilio("/v1/inbound/sms/int-tw/status", status("SMearly", "failed", "30003")); got != http.StatusNoContent {
		t.Fatalf("early callback: status %d", got)
	}
	m := r.sentMessage(envA, "int-tw", "SMearly")
	if m.Status != "error" || m.ErrorClass != string(providers.ClassRecipient) || !strings.Contains(m.ErrorText, "30003") {
		t.Fatalf("message written after its callback = %+v, want the failure applied", m)
	}
}

// TestStatus_BufferedStatusSendsWebhookEvent: a status applied at message
// create reaches the tenant as the same sms.status event a live one does.
func TestStatus_BufferedStatusSendsWebhookEvent(t *testing.T) {
	r := newRig(t)
	r.twilio("/v1/inbound/sms/int-tw/status", status("SMearly2", "failed", "30003"))
	if n := len(r.ev.ofType(EventStatus)); n != 0 {
		t.Fatalf("%d status events before the message exists", n)
	}
	m := r.sentMessage(envA, "int-tw", "SMearly2")
	evs := r.ev.ofType(EventStatus)
	if len(evs) != 1 {
		t.Fatalf("%d status events after create, want 1", len(evs))
	}
	e := evs[0]
	if e["messageId"] != m.ID || e["status"] != "error" || e["errorClass"] != string(providers.ClassRecipient) ||
		e["providerMessageId"] != "SMearly2" || e["integrationId"] != "int-tw" || e["subscriberId"] != "ada" ||
		e["OrgID"] != envA.OrgID || e["EnvID"] != envA.EnvID {
		t.Fatalf("event = %+v", e)
	}
	// A message with no buffered status sends none.
	r.sentMessage(envA, "int-tw", "SMplain")
	if n := len(r.ev.ofType(EventStatus)); n != 1 {
		t.Fatalf("%d status events, want still 1", n)
	}
}

// TestKeywords_FailedHelpSendReleasesTheSlot: a HELP whose send failed does
// not hold the 10-minute slot, so the next HELP is answered.
func TestKeywords_FailedHelpSendReleasesTheSlot(t *testing.T) {
	r := newRig(t)
	r.flaky.fail.Store(true)
	if got := r.twilio("/v1/inbound/sms/int-tw", text("HF1", "+15553334444", "HELP")); got != http.StatusOK {
		t.Fatalf("HELP: status %d", got)
	}
	if n := len(r.sender.Sent()); n != 0 {
		t.Fatalf("%d sends while the provider was down", n)
	}
	r.flaky.fail.Store(false)
	r.twilio("/v1/inbound/sms/int-tw", text("HF2", "+15553334444", "HELP"))
	if n := len(r.sender.Sent()); n != 1 {
		t.Fatalf("%d replies after recovery, want 1: a failed send used up the slot", n)
	}
	r.twilio("/v1/inbound/sms/int-tw", text("HF3", "+15553334444", "HELP"))
	if n := len(r.sender.Sent()); n != 1 {
		t.Fatalf("%d replies, want the successful one to still hold the slot", n)
	}
}

// TestInbound_OversizedBody: a body past the limit is refused with 413.
func TestInbound_OversizedBody(t *testing.T) {
	r := newRig(t)
	form := text("BIG", "+15551112222", strings.Repeat("x", 70<<10))
	if got := r.twilio("/v1/inbound/sms/int-tw", form); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", got)
	}
}

// sentMessage records a sent sms message in scope through integration, as
// the delivery handler would.
func (r *rig) sentMessage(scope domain.Scope, integration, sid string) domain.Message {
	r.t.Helper()
	m, err := r.st.Messages().Create(context.Background(), scope, domain.Message{
		Channel: "sms", ProviderID: "twilio", IntegrationID: integration, SubscriberID: "ada",
		Status: "sent", ProviderMessageID: sid,
	})
	if err != nil {
		r.t.Fatalf("create: %v", err)
	}
	return m
}

func status(sid, st, code string) url.Values {
	v := url.Values{"MessageSid": {sid}, "MessageStatus": {st}, "AccountSid": {"AC1"}, "To": {"+15551112222"}, "From": {ourNumber}}
	if code != "" {
		v.Set("ErrorCode", code)
	}
	return v
}

func (r *rig) message(scope domain.Scope, id string) domain.Message {
	r.t.Helper()
	m, err := r.st.Messages().Get(context.Background(), scope, id)
	if err != nil {
		r.t.Fatalf("get: %v", err)
	}
	return m
}

func TestStatus_DeliveredAndFailed(t *testing.T) {
	r := newRig(t)
	path := "/v1/inbound/sms/int-tw/status"
	ok := r.sentMessage(envA, "int-tw", "SMok")
	bad := r.sentMessage(envA, "int-tw", "SMbad")

	for _, f := range []url.Values{status("SMok", "queued", ""), status("SMok", "sent", "")} {
		if got := r.twilio(path, f); got != http.StatusNoContent {
			t.Fatalf("status %d, want 204", got)
		}
	}
	if m := r.message(envA, ok.ID); m.Status != "sent" {
		t.Fatalf("after queued/sent: %q, want sent", m.Status)
	}
	r.twilio(path, status("SMok", "delivered", ""))
	if m := r.message(envA, ok.ID); m.Status != "delivered" {
		t.Fatalf("after delivered: %q", m.Status)
	}
	r.twilio(path, status("SMbad", "undelivered", "30003"))
	if m := r.message(envA, bad.ID); m.Status != "error" || m.ErrorClass != string(providers.ClassRecipient) || !strings.Contains(m.ErrorText, "30003") {
		t.Fatalf("after undelivered: %+v", m)
	}
	evs := r.ev.ofType(EventStatus)
	if len(evs) != 2 || evs[0]["status"] != "delivered" || evs[1]["status"] != "error" || evs[1]["errorClass"] != "recipient" {
		t.Fatalf("status events = %v", evs)
	}
	if evs[0]["messageId"] != ok.ID || evs[0]["subscriberId"] != "ada" {
		t.Fatalf("status event = %v", evs[0])
	}

	// A bad signature is refused like any other callback.
	if got := r.post(path, status("SMok", "failed", "30008"), twilio.SignatureHeader, "nope"); got != http.StatusForbidden {
		t.Fatalf("unsigned status: %d, want 403", got)
	}
}

func TestStatus_DuplicateAndOutOfOrder(t *testing.T) {
	r := newRig(t)
	path := "/v1/inbound/sms/int-tw/status"
	m := r.sentMessage(envA, "int-tw", "SM1")

	r.twilio(path, status("SM1", "delivered", ""))
	r.twilio(path, status("SM1", "delivered", "")) // duplicate
	r.twilio(path, status("SM1", "sent", ""))      // late
	r.twilio(path, status("SM1", "failed", "30008"))
	if got := r.message(envA, m.ID); got.Status != "delivered" || got.ErrorClass != "" {
		t.Fatalf("after duplicate and late callbacks: %+v, want delivered", got)
	}
	if n := len(r.ev.ofType(EventStatus)); n != 1 {
		t.Fatalf("%d status events, want 1 (only the real change)", n)
	}
	// An unknown message id is acknowledged and changes nothing.
	if got := r.twilio(path, status("SMnope", "delivered", "")); got != http.StatusNoContent {
		t.Fatalf("unknown sid: %d, want 204", got)
	}
}

// healthObserver records the failures delivery health hears.
type healthObserver struct {
	mu      sync.Mutex
	failed  []string
	created int
}

func (h *healthObserver) MessageCreated(context.Context, domain.Scope, domain.Message) {
	h.mu.Lock()
	h.created++
	h.mu.Unlock()
}

func (h *healthObserver) AttemptFailed(_ context.Context, _ domain.Scope, integrationID, class, _ string) {
	h.mu.Lock()
	h.failed = append(h.failed, integrationID+":"+class)
	h.mu.Unlock()
}

func TestStatus_FeedsHealthWithRealFailuresOnly(t *testing.T) {
	r := newRig(t)
	obs := &healthObserver{}
	r.st.SetDeliveryObserver(obs)
	path := "/v1/inbound/sms/int-tw/status"
	r.sentMessage(envA, "int-tw", "SMa")
	r.sentMessage(envA, "int-tw", "SMb")

	r.twilio(path, status("SMa", "delivered", ""))
	r.twilio(path, status("SMb", "failed", "30007"))
	r.twilio(path, status("SMb", "failed", "30007")) // repeat: not a second failure
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.failed) != 1 || obs.failed[0] != "int-tw:rejected_message" {
		t.Fatalf("health failures = %v, want one rejected_message", obs.failed)
	}
}

// TestTenancy: a callback for env B's integration cannot touch env A's
// messages or subscribers, even naming the same provider message id and phone.
func TestTenancy(t *testing.T) {
	r := newRig(t)
	r.subscriber(envA, "ada", "+15551112222", true)
	m := r.sentMessage(envA, "int-tw", "SHARED")

	status := url.Values{"MessageSid": {"SHARED"}, "MessageStatus": {"failed"}, "ErrorCode": {"30003"}}
	path := "/v1/inbound/sms/int-sw/status"
	if got := r.post(path, status, "X-SignalWire-Signature", hexSig(swKey, publicURL+path, status.Encode())); got != http.StatusNoContent {
		t.Fatalf("env B status: %d", got)
	}
	if got := r.message(envA, m.ID); got.Status != "sent" {
		t.Fatalf("env B's callback moved env A's message to %q", got.Status)
	}

	stop := text("B1", "+15551112222", "STOP")
	in := "/v1/inbound/sms/int-sw"
	if got := r.post(in, stop, "X-SignalWire-Signature", hexSig(swKey, publicURL+in, stop.Encode())); got != http.StatusOK {
		t.Fatalf("env B STOP: %d", got)
	}
	if out, _ := r.st.SMSOptOuts().IsOptedOut(context.Background(), envA, "+15551112222"); out {
		t.Fatal("env B's STOP opted the number out in env A")
	}
	if out, _ := r.st.SMSOptOuts().IsOptedOut(context.Background(), envB, "+15551112222"); !out {
		t.Fatal("env B's STOP was not recorded in env B")
	}
	for _, e := range r.ev.ofType(EventOptOut) {
		if e["EnvID"] != envB.EnvID {
			t.Fatalf("opt-out event in %v", e["EnvID"])
		}
		if _, ok := e["subscriberId"]; ok {
			t.Fatal("env B's event named env A's subscriber")
		}
	}
	if a, _ := r.st.InboundMessages().List(context.Background(), envA, 0, 50); len(a) != 0 {
		t.Fatalf("env B's text landed in env A: %+v", a)
	}
}

func TestNoPublicURL_Refuses(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st, fakeCallbacks{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := chi.NewRouter()
	svc.Routes(router)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/inbound/sms/int-tw", strings.NewReader("Body=x")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 without a public URL", rec.Code)
	}
}
