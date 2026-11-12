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
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/phoneverify"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

const policyURL = "https://shop.example/privacy"

type consentFixture struct {
	srv    *httptest.Server
	st     *store.Store
	id     *identity.Service
	pv     *phoneverify.Service
	scope  domain.Scope
	apiKey string
	// other is a second tenant, with its own subscriber "cust-1".
	other    domain.Scope
	otherKey string
	capt     *providers.CaptureSMS
}

func newConsentFixture(t *testing.T) consentFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	id := identity.New(st, bytesRepeat(0x2a, 32))
	aud := audience.New(st)
	capt := providers.NewCaptureSMS()
	clk := &clock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	pv := phoneverify.New(st, fakeSMSResolver{capt}).WithClock(clk.now)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil),
		Inbox: inboxapi.New(st).WithPhoneVerification(pv), Store: st, Phone: pv,
	}))
	t.Cleanup(srv.Close)

	key, scope, err := id.Bootstrap(ctx, "Studs Cycles", "prod")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	otherKey, other, err := id.Bootstrap(ctx, "Elsewhere", "prod")
	if err != nil {
		t.Fatalf("Bootstrap other: %v", err)
	}
	for _, sc := range []domain.Scope{scope, other} {
		if _, err := st.Subscribers().Upsert(ctx, sc, domain.Subscriber{SubscriberID: "cust-1"}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	return consentFixture{
		srv: srv, st: st, id: id, pv: pv, scope: scope, apiKey: "ApiKey " + key,
		other: other, otherKey: "ApiKey " + otherKey, capt: capt,
	}
}

// enable turns the setting on and grants the key the permission.
func (f consentFixture) enable(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := f.pv.SetConsentSettings(ctx, f.scope, true, policyURL); err != nil {
		t.Fatalf("SetConsentSettings: %v", err)
	}
	if err := f.id.SetAttestSMSConsent(ctx, f.scope, true); err != nil {
		t.Fatalf("SetAttestSMSConsent: %v", err)
	}
}

func (f consentFixture) attest(t *testing.T, subscriberID, body string) (int, string) {
	t.Helper()
	return req(t, http.MethodPost, f.srv.URL+"/v1/subscribers/"+subscriberID+"/phone/consent", f.apiKey, body)
}

func (f consentFixture) revoke(t *testing.T, subscriberID string) (int, string) {
	t.Helper()
	return f.revokeWith(t, subscriberID, "")
}

func (f consentFixture) revokeWith(t *testing.T, subscriberID, body string) (int, string) {
	t.Helper()
	return req(t, http.MethodDelete, f.srv.URL+"/v1/subscribers/"+subscriberID+"/phone/consent", f.apiKey, body)
}

// sendSMS runs one sms step for cust-1 and returns the message it recorded.
func (f consentFixture) sendSMS(t *testing.T, run string) domain.Message {
	t.Helper()
	env := runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: f.scope.OrgID, EnvID: f.scope.EnvID},
		TransactionID:  "tx-" + run,
		WorkflowID:     "wf",
		NotificationID: "ntf-" + run,
		SubscriberID:   "cust-1",
		Subscriber:     json.RawMessage(`{"subscriberId":"cust-1"}`),
		Payload:        json.RawMessage(`{}`),
		Step:           runenv.Step{StepID: "text", Channel: "sms", BodyTemplate: "Your bike is ready"},
	}
	input, _ := json.Marshal(env)
	out, err := delivery.NewSMS(f.st, fakeSMSResolver{f.capt}).HandleSMS(context.Background(), engine.TaskPayload{RunID: run, StepID: "text", Input: input})
	if err != nil {
		t.Fatalf("HandleSMS: %v", err)
	}
	var m domain.Message
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	return m
}

// subscriberJSON reads cust-1 through GET /v1/subscribers/{id}.
func (f consentFixture) subscriberJSON(t *testing.T) map[string]any {
	t.Helper()
	sub, err := f.st.Subscribers().GetByExternalID(context.Background(), f.scope, "cust-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	status, body := req(t, http.MethodGet, f.srv.URL+"/v1/subscribers/"+sub.ID, f.apiKey, "")
	if status != http.StatusOK {
		t.Fatalf("GET subscriber: %d %s", status, body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func TestSMSConsent_SettingOffIs403(t *testing.T) {
	f := newConsentFixture(t)
	if err := f.id.SetAttestSMSConsent(context.Background(), f.scope, true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	status, body := f.attest(t, "cust-1", `{"phone":"+15551112222"}`)
	if status != http.StatusForbidden || errorCode(t, body) != "sms_consent_disabled" {
		t.Fatalf("attest with the setting off: %d %s, want 403 sms_consent_disabled", status, body)
	}
}

func TestSMSConsent_KeyWithoutPermissionIs403(t *testing.T) {
	f := newConsentFixture(t)
	if err := f.pv.SetConsentSettings(context.Background(), f.scope, true, policyURL); err != nil {
		t.Fatalf("enable: %v", err)
	}
	status, body := f.attest(t, "cust-1", `{"phone":"+15551112222"}`)
	if status != http.StatusForbidden || errorCode(t, body) != "forbidden" {
		t.Fatalf("attest without the permission: %d %s, want 403 forbidden", status, body)
	}
	if m := f.subscriberJSON(t); m["phoneConsent"] != nil {
		t.Fatalf("refused attest left consent: %v", m)
	}
}

func TestSMSConsent_AttestThenSendRecordsAudit(t *testing.T) {
	f := newConsentFixture(t)
	f.enable(t)
	// No consent yet: skipped.
	if m := f.sendSMS(t, "r0"); m.Status != "skipped" || m.SkipReason != delivery.SkipReasonNoVerifiedPhone {
		t.Fatalf("before attest: %+v", m)
	}

	status, body := f.attest(t, "cust-1", `{"phone":"+1 (555) 111-2222","source":"opted in at the counter","attestedAt":"2025-06-01T10:00:00Z"}`)
	if status != http.StatusCreated {
		t.Fatalf("attest: %d %s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	keys, _ := f.st.ApiKeys().List(context.Background(), f.scope)
	for k, want := range map[string]any{
		"subscriberId": "cust-1", "phone": "+15551112222", "phoneConsent": "operator_attested",
		"apiKeyId": keys[0].Hash, "source": "opted in at the counter", "policyUrl": policyURL,
	} {
		if got[k] != want {
			t.Errorf("response %s = %v, want %v (body %s)", k, got[k], want, body)
		}
	}
	if got["attestedAt"] == "" || got["attestedAt"] == nil || got["id"] == nil || got["createdAt"] == nil {
		t.Errorf("response missing attestedAt/id/createdAt: %s", body)
	}

	m := f.subscriberJSON(t)
	if m["phone"] != "+15551112222" || m["phoneConsent"] != "operator_attested" || m["phoneVerified"] != false {
		t.Fatalf("subscriber = %v, want operator_attested and not verified", m)
	}
	if sent := f.sendSMS(t, "r1"); sent.Status != "sent" {
		t.Fatalf("after attest: %+v, want sent", sent)
	}
	if s := f.capt.Sent(); len(s) != 1 || s[0].To != "+15551112222" {
		t.Fatalf("sent = %+v", s)
	}

	rows, err := f.st.SMSConsents().List(context.Background(), f.scope, "cust-1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit = %+v, %v", rows, err)
	}
	if rows[0].APIKeyID != keys[0].Hash || rows[0].PolicyURL != policyURL || rows[0].RevokedAt != "" {
		t.Fatalf("audit row = %+v, want the key id and policy URL", rows[0])
	}
}

func TestSMSConsent_StopAfterAttestIsOptedOut(t *testing.T) {
	f := newConsentFixture(t)
	f.enable(t)
	if status, body := f.attest(t, "cust-1", `{"phone":"+15551112222"}`); status != http.StatusCreated {
		t.Fatalf("attest: %d %s", status, body)
	}
	if _, err := f.st.SMSOptOuts().OptOut(context.Background(), f.scope, "+15551112222", "int"); err != nil {
		t.Fatalf("OptOut: %v", err)
	}
	if m := f.sendSMS(t, "r1"); m.Status != "skipped" || m.SkipReason != delivery.SkipReasonOptedOut {
		t.Fatalf("after STOP: %+v, want skipped opted_out", m)
	}
}

func TestSMSConsent_PhoneChangeClears(t *testing.T) {
	f := newConsentFixture(t)
	f.enable(t)
	if status, body := f.attest(t, "cust-1", `{"phone":"+15551112222"}`); status != http.StatusCreated {
		t.Fatalf("attest: %d %s", status, body)
	}
	sub, _ := f.st.Subscribers().GetByExternalID(context.Background(), f.scope, "cust-1")
	if status, body := req(t, http.MethodPatch, f.srv.URL+"/v1/subscribers/"+sub.ID, f.apiKey, `{"phone":"+15559990000"}`); status != http.StatusOK {
		t.Fatalf("patch: %d %s", status, body)
	}
	if m := f.subscriberJSON(t); m["phoneConsent"] != nil {
		t.Fatalf("after a phone change: %v, want no consent", m)
	}
	if m := f.sendSMS(t, "r1"); m.Status != "skipped" || m.SkipReason != delivery.SkipReasonNoVerifiedPhone {
		t.Fatalf("after a phone change: %+v, want skipped", m)
	}
	rows, _ := f.st.SMSConsents().List(context.Background(), f.scope, "cust-1")
	if len(rows) != 1 || rows[0].RevokedReason != store.RevokedPhoneChanged {
		t.Fatalf("audit = %+v", rows)
	}
}

func TestSMSConsent_RevokeSkips(t *testing.T) {
	f := newConsentFixture(t)
	f.enable(t)
	if status, body := f.attest(t, "cust-1", `{"phone":"+15551112222"}`); status != http.StatusCreated {
		t.Fatalf("attest: %d %s", status, body)
	}
	if status, body := f.revoke(t, "cust-1"); status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", status, body)
	}
	if m := f.sendSMS(t, "r1"); m.Status != "skipped" || m.SkipReason != delivery.SkipReasonNoVerifiedPhone {
		t.Fatalf("after revoke: %+v, want skipped", m)
	}
	if m := f.subscriberJSON(t); m["phone"] != "+15551112222" || m["phoneConsent"] != nil {
		t.Fatalf("after revoke: %v, want the phone kept without consent", m)
	}
	rows, _ := f.st.SMSConsents().List(context.Background(), f.scope, "cust-1")
	keys, _ := f.st.ApiKeys().List(context.Background(), f.scope)
	if len(rows) != 1 || rows[0].RevokedAt == "" || rows[0].RevokedReason != store.RevokedByAPI || rows[0].RevokedByKeyID != keys[0].Hash {
		t.Fatalf("audit = %+v, want the row kept, revoked by the key", rows)
	}
	if status, body := f.revoke(t, "nobody"); status != http.StatusNotFound {
		t.Fatalf("revoke unknown: %d %s", status, body)
	}
}

func TestSMSConsent_OtherEnvironmentsSubscriberIs404(t *testing.T) {
	f := newConsentFixture(t)
	f.enable(t)
	// cust-1 exists in the other tenant only under that tenant's id space;
	// "only-there" exists nowhere in ours.
	if _, err := f.st.Subscribers().Upsert(context.Background(), f.other, domain.Subscriber{SubscriberID: "only-there"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	status, body := f.attest(t, "only-there", `{"phone":"+15551112222"}`)
	if status != http.StatusNotFound || errorCode(t, body) != "not_found" {
		t.Fatalf("attest another environment's subscriber: %d %s, want 404", status, body)
	}
	if status, _ := f.revoke(t, "only-there"); status != http.StatusNotFound {
		t.Fatalf("revoke another environment's subscriber: %d, want 404", status)
	}
	// Attesting ours leaves the other tenant's same-named subscriber alone.
	if status, body := f.attest(t, "cust-1", `{"phone":"+15551112222"}`); status != http.StatusCreated {
		t.Fatalf("attest: %d %s", status, body)
	}
	theirs, _ := f.st.Subscribers().GetByExternalID(context.Background(), f.other, "cust-1")
	if theirs.PhoneAttestedAt != "" || theirs.Phone != "" {
		t.Fatalf("other tenant's subscriber changed: %+v", theirs)
	}
}

func TestSMSConsent_BadInput(t *testing.T) {
	f := newConsentFixture(t)
	f.enable(t)
	for body, want := range map[string]int{
		`{"phone":"555"}`: http.StatusUnprocessableEntity,
		`{"phone":"+15551112222","attestedAt":"2099-01-01T00:00:00Z"}`: http.StatusBadRequest,
		`{"phone":"+15551112222","attestedAt":"last week"}`:            http.StatusBadRequest,
		`{"phone":"+15551112222","attestedAt":"1999-12-31T23:59:59Z"}`: http.StatusBadRequest,
		`not json`: http.StatusBadRequest,
	} {
		if status, resp := f.attest(t, "cust-1", body); status != want {
			t.Errorf("attest %s: %d %s, want %d", body, status, resp, want)
		}
	}
}

// TestSMSConsent_SelfVerifiedUnchanged: a self-verified phone sends with the
// setting off and no permission, reads as self_verified, and survives a
// revoke.
func TestSMSConsent_SelfVerifiedUnchanged(t *testing.T) {
	f := newConsentFixture(t)
	if err := f.st.Subscribers().SetVerifiedPhone(context.Background(), f.scope, "cust-1", "+15551112222", "2026-11-12T12:00:00Z"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if m := f.subscriberJSON(t); m["phoneConsent"] != "self_verified" || m["phoneVerified"] != true {
		t.Fatalf("subscriber = %v", m)
	}
	if m := f.sendSMS(t, "r1"); m.Status != "sent" {
		t.Fatalf("self-verified: %+v, want sent", m)
	}
	if status, body := f.revoke(t, "cust-1"); status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", status, body)
	}
	if m := f.sendSMS(t, "r2"); m.Status != "sent" {
		t.Fatalf("self-verified after a revoke: %+v, want sent", m)
	}
}

// TestSMSConsent_RevokeRecordsTheReason: DELETE takes an optional
// {"reason"}, stored with the revoking key on the audit row.
func TestSMSConsent_RevokeRecordsTheReason(t *testing.T) {
	f := newConsentFixture(t)
	f.enable(t)
	if status, body := f.attest(t, "cust-1", `{"phone":"+15551112222"}`); status != http.StatusCreated {
		t.Fatalf("attest: %d %s", status, body)
	}
	if status, body := f.revokeWith(t, "cust-1", `{"reason":`); status != http.StatusBadRequest {
		t.Fatalf("revoke with bad JSON: %d %s, want 400", status, body)
	}
	if status, body := f.revokeWith(t, "cust-1", `{"reason":"customer called to opt out"}`); status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", status, body)
	}
	rows, _ := f.st.SMSConsents().List(context.Background(), f.scope, "cust-1")
	keys, _ := f.st.ApiKeys().List(context.Background(), f.scope)
	if len(rows) != 1 || rows[0].RevokedByKeyID != keys[0].Hash || rows[0].RevokedNote != "customer called to opt out" {
		t.Fatalf("audit = %+v, want the key and the reason", rows)
	}
}

// TestSMSConsent_SelfVerifiedDifferentNumberIs409: an attestation may not
// replace a number the subscriber verified themselves; attesting that same
// number succeeds and is recorded.
func TestSMSConsent_SelfVerifiedDifferentNumberIs409(t *testing.T) {
	f := newConsentFixture(t)
	f.enable(t)
	ctx := context.Background()
	if err := f.st.Subscribers().SetVerifiedPhone(ctx, f.scope, "cust-1", "+15551112222", "2026-11-12T12:00:00Z"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	status, body := f.attest(t, "cust-1", `{"phone":"+15559998888"}`)
	if status != http.StatusConflict || errorCode(t, body) != "phone_self_verified" {
		t.Fatalf("attest over a different verified number: %d %s, want 409 phone_self_verified", status, body)
	}
	if m := f.subscriberJSON(t); m["phone"] != "+15551112222" || m["phoneConsent"] != "self_verified" {
		t.Fatalf("refused attest changed the subscriber: %v", m)
	}
	if status, body := f.attest(t, "cust-1", `{"phone":"+15551112222"}`); status != http.StatusCreated {
		t.Fatalf("attest the verified number: %d %s", status, body)
	}
	if m := f.subscriberJSON(t); m["phoneConsent"] != "self_verified" {
		t.Fatalf("same-number attest: %v, want still self_verified", m)
	}
	if rows, _ := f.st.SMSConsents().List(ctx, f.scope, "cust-1"); len(rows) != 1 {
		t.Fatalf("audit = %+v, want the same-number attestation recorded", rows)
	}
}
