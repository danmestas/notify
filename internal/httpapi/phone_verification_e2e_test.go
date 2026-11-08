package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/phoneverify"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// fakeSMSResolver hands every SMS send (verification codes and sms steps) to
// one recording provider. It stands in for connectivity.Service: no real
// Twilio is ever reached.
type fakeSMSResolver struct{ capt *providers.CaptureSMS }

func (r fakeSMSResolver) ResolveSMS(context.Context, domain.Scope) (providers.SMSProvider, string, string, error) {
	return r.capt, "twilio", "", nil
}

func (r fakeSMSResolver) ResolveIntegration(ctx context.Context, scope domain.Scope, _, _ string) (any, string, string, error) {
	return r.ResolveSMS(ctx, scope)
}

var sixDigits = regexp.MustCompile(`\b\d{6}\b`)

// lastCode is the code in the newest sms sent to phone.
func lastCode(t *testing.T, capt *providers.CaptureSMS, phone string) string {
	t.Helper()
	sent := capt.Sent()
	for i := len(sent) - 1; i >= 0; i-- {
		if sent[i].To == phone {
			if c := sixDigits.FindString(sent[i].Body); c != "" {
				return c
			}
		}
	}
	t.Fatalf("no code sent to %s (sent: %+v)", phone, sent)
	return ""
}

// clock is a settable time source shared with phoneverify.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type phoneFixture struct {
	srv      *httptest.Server
	st       *store.Store
	scope    domain.Scope
	apiKey   string
	aliceTok string
	bobTok   string
	capt     *providers.CaptureSMS
	clock    *clock
}

func newPhoneFixture(t *testing.T) phoneFixture {
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
	clk := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	pv := phoneverify.New(st, fakeSMSResolver{capt}).WithClock(clk.now)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil),
		Inbox: inboxapi.New(st).WithPhoneVerification(pv), Store: st,
	}))
	t.Cleanup(srv.Close)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, sub := range []string{"alice", "bob"} {
		if _, err := st.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: sub}); err != nil {
			t.Fatalf("upsert %s: %v", sub, err)
		}
	}
	alice, _ := id.MintSubscriberSession(scope, "alice")
	bob, _ := id.MintSubscriberSession(scope, "bob")
	return phoneFixture{
		srv: srv, st: st, scope: scope, apiKey: "ApiKey " + key,
		aliceTok: "Bearer " + alice, bobTok: "Bearer " + bob, capt: capt, clock: clk,
	}
}

func (f phoneFixture) request(t *testing.T, auth, phone string) (int, string) {
	t.Helper()
	return req(t, http.MethodPost, f.srv.URL+"/v1/me/phone/verification", auth, `{"phone":"`+phone+`"}`)
}

func (f phoneFixture) confirm(t *testing.T, auth, code string) (int, string) {
	t.Helper()
	return req(t, http.MethodPost, f.srv.URL+"/v1/me/phone/verification/confirm", auth, `{"code":"`+code+`"}`)
}

func (f phoneFixture) sub(t *testing.T, id string) domain.Subscriber {
	t.Helper()
	s, err := f.st.Subscribers().GetByExternalID(context.Background(), f.scope, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return s
}

// errorCode decodes the {"error":{"code":...}} envelope.
func errorCode(t *testing.T, body string) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, body)
	}
	return env.Error.Code
}

func TestPhoneVerification_HappyPath(t *testing.T) {
	f := newPhoneFixture(t)

	status, body := f.request(t, f.aliceTok, "+1 (555) 123-4567")
	if status != http.StatusAccepted {
		t.Fatalf("request: status=%d body=%s", status, body)
	}
	var pending map[string]any
	if err := json.Unmarshal([]byte(body), &pending); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pending["phone"] != "+15551234567" || pending["expiresAt"] != "2026-11-08T12:10:00Z" {
		t.Errorf("request body = %s, want phone +15551234567 expiring at 12:10", body)
	}

	status, body = f.confirm(t, f.aliceTok, lastCode(t, f.capt, "+15551234567"))
	if status != http.StatusOK {
		t.Fatalf("confirm: status=%d body=%s", status, body)
	}
	var verified map[string]any
	if err := json.Unmarshal([]byte(body), &verified); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if verified["phone"] != "+15551234567" || verified["verifiedAt"] != "2026-11-08T12:00:00Z" {
		t.Errorf("confirm body = %s", body)
	}
	if s := f.sub(t, "alice"); s.Phone != "+15551234567" || s.PhoneVerifiedAt == "" {
		t.Fatalf("alice = %+v, want the number verified", s)
	}

	// The sender's view shows it verified, read-only.
	status, body = req(t, http.MethodGet, f.srv.URL+"/v1/subscribers/"+f.sub(t, "alice").ID, f.apiKey, "")
	if status != http.StatusOK {
		t.Fatalf("GET subscriber: %d %s", status, body)
	}
	var dto map[string]any
	_ = json.Unmarshal([]byte(body), &dto)
	if dto["phoneVerified"] != true {
		t.Errorf("subscriber DTO = %s, want phoneVerified true", body)
	}
}

func TestPhoneVerification_WrongCode(t *testing.T) {
	f := newPhoneFixture(t)
	if status, body := f.request(t, f.aliceTok, "+15551234567"); status != http.StatusAccepted {
		t.Fatalf("request: %d %s", status, body)
	}
	code := lastCode(t, f.capt, "+15551234567")
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	status, body := f.confirm(t, f.aliceTok, wrong)
	if status != http.StatusUnprocessableEntity || errorCode(t, body) != "wrong_code" {
		t.Fatalf("wrong code: status=%d body=%s, want 422 wrong_code", status, body)
	}
	if s := f.sub(t, "alice"); s.PhoneVerifiedAt != "" {
		t.Fatalf("wrong code verified the phone: %+v", s)
	}
}

func TestPhoneVerification_ExpiredCode(t *testing.T) {
	f := newPhoneFixture(t)
	if status, body := f.request(t, f.aliceTok, "+15551234567"); status != http.StatusAccepted {
		t.Fatalf("request: %d %s", status, body)
	}
	code := lastCode(t, f.capt, "+15551234567")
	f.clock.advance(phoneverify.CodeTTL)
	status, body := f.confirm(t, f.aliceTok, code)
	if status != http.StatusGone || errorCode(t, body) != "code_expired" {
		t.Fatalf("expired code: status=%d body=%s, want 410 code_expired", status, body)
	}
	if s := f.sub(t, "alice"); s.PhoneVerifiedAt != "" {
		t.Fatalf("expired code verified the phone: %+v", s)
	}
}

func TestPhoneVerification_AttemptLimit(t *testing.T) {
	f := newPhoneFixture(t)
	if status, body := f.request(t, f.aliceTok, "+15551234567"); status != http.StatusAccepted {
		t.Fatalf("request: %d %s", status, body)
	}
	code := lastCode(t, f.capt, "+15551234567")
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < phoneverify.MaxAttempts; i++ {
		if status, body := f.confirm(t, f.aliceTok, wrong); status != http.StatusUnprocessableEntity {
			t.Fatalf("attempt %d: status=%d body=%s", i+1, status, body)
		}
	}
	status, body := f.confirm(t, f.aliceTok, code)
	if status != http.StatusTooManyRequests || errorCode(t, body) != "too_many_attempts" {
		t.Fatalf("right code past the limit: status=%d body=%s, want 429 too_many_attempts", status, body)
	}
	if s := f.sub(t, "alice"); s.PhoneVerifiedAt != "" {
		t.Fatalf("phone verified past the attempt limit: %+v", s)
	}
}

func TestPhoneVerification_SendRateLimit(t *testing.T) {
	f := newPhoneFixture(t)
	for i := 0; i < phoneverify.MaxSends; i++ {
		if status, body := f.request(t, f.aliceTok, "+15551234567"); status != http.StatusAccepted {
			t.Fatalf("send %d: %d %s", i+1, status, body)
		}
	}
	status, body := f.request(t, f.aliceTok, "+15551234567")
	if status != http.StatusTooManyRequests || errorCode(t, body) != "rate_limited" {
		t.Fatalf("over the limit: status=%d body=%s, want 429 rate_limited", status, body)
	}
	// Per number too: bob cannot flood alice's number either.
	if status, body := f.request(t, f.bobTok, "+15551234567"); status != http.StatusTooManyRequests {
		t.Fatalf("bob to the same number: status=%d body=%s, want 429", status, body)
	}
	if got := len(f.capt.Sent()); got != phoneverify.MaxSends {
		t.Errorf("codes sent = %d, want %d", got, phoneverify.MaxSends)
	}
}

func TestPhoneVerification_InvalidNumber(t *testing.T) {
	f := newPhoneFixture(t)
	status, body := f.request(t, f.aliceTok, "555-1234")
	if status != http.StatusUnprocessableEntity || errorCode(t, body) != "invalid_phone" {
		t.Fatalf("invalid number: status=%d body=%s, want 422 invalid_phone", status, body)
	}
}

// TestPhoneVerification_SessionCannotVerifyAnotherSubscriber: the session
// decides whose phone it is. Bob holding alice's code verifies nothing, and a
// subscriberId in the body is ignored.
func TestPhoneVerification_SessionCannotVerifyAnotherSubscriber(t *testing.T) {
	f := newPhoneFixture(t)
	status, body := req(t, http.MethodPost, f.srv.URL+"/v1/me/phone/verification", f.bobTok,
		`{"phone":"+15551234567","subscriberId":"alice"}`)
	if status != http.StatusAccepted {
		t.Fatalf("bob request: %d %s", status, body)
	}
	if _, err := f.st.PhoneVerifications().Latest(context.Background(), f.scope, "alice"); err == nil {
		t.Fatal("bob's request created a code for alice")
	}

	if status, body := f.request(t, f.aliceTok, "+15559990000"); status != http.StatusAccepted {
		t.Fatalf("alice request: %d %s", status, body)
	}
	aliceCode := lastCode(t, f.capt, "+15559990000")
	status, body = req(t, http.MethodPost, f.srv.URL+"/v1/me/phone/verification/confirm", f.bobTok,
		`{"code":"`+aliceCode+`","subscriberId":"alice"}`)
	if status == http.StatusOK {
		t.Fatalf("bob confirmed with alice's code: %s", body)
	}
	if s := f.sub(t, "alice"); s.PhoneVerifiedAt != "" || s.Phone != "" {
		t.Fatalf("alice changed by bob: %+v", s)
	}
	if s := f.sub(t, "bob"); s.Phone == "+15559990000" {
		t.Fatalf("bob took alice's number: %+v", s)
	}
}

// TestPhoneVerification_RequiresASubscriberSession: the sender's API key can
// never verify a phone, so the routes refuse it.
func TestPhoneVerification_RequiresASubscriberSession(t *testing.T) {
	f := newPhoneFixture(t)
	status, body := req(t, http.MethodPost, f.srv.URL+"/v1/me/phone/verification", f.apiKey, `{"phone":"+15551234567"}`)
	if status != http.StatusForbidden {
		t.Fatalf("api key request: status=%d body=%s, want 403", status, body)
	}
	status, body = req(t, http.MethodPost, f.srv.URL+"/v1/me/phone/verification/confirm", f.apiKey, `{"code":"123456"}`)
	if status != http.StatusForbidden {
		t.Fatalf("api key confirm: status=%d body=%s, want 403", status, body)
	}
	if got := len(f.capt.Sent()); got != 0 {
		t.Errorf("codes sent = %d, want 0", got)
	}
	if status, _ := req(t, http.MethodPost, f.srv.URL+"/v1/me/phone/verification", "", `{"phone":"+15551234567"}`); status != http.StatusUnauthorized {
		t.Errorf("no credential: status=%d, want 401", status)
	}
}

// TestPhoneVerification_APIWritesNeverVerify: the sender API still stores a
// phone (existing clients keep working) but never a verified one, and writing
// a different number clears a verification the subscriber made.
func TestPhoneVerification_APIWritesNeverVerify(t *testing.T) {
	f := newPhoneFixture(t)

	status, body := req(t, http.MethodPost, f.srv.URL+"/v1/subscribers", f.apiKey,
		`{"subscriberId":"carol","phone":"+15552223333","phoneVerified":true,"phoneVerifiedAt":"2026-01-01T00:00:00Z"}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	carol := f.sub(t, "carol")
	if carol.Phone != "+15552223333" || carol.PhoneVerifiedAt != "" {
		t.Fatalf("API create: %+v, want the phone stored unverified", carol)
	}

	// alice verifies, then the sender patches the same number: still verified.
	if status, body := f.request(t, f.aliceTok, "+15551234567"); status != http.StatusAccepted {
		t.Fatalf("request: %d %s", status, body)
	}
	if status, body := f.confirm(t, f.aliceTok, lastCode(t, f.capt, "+15551234567")); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, body)
	}
	aliceID := f.sub(t, "alice").ID
	if status, body := doPatch(t, f.srv.URL+"/v1/subscribers/"+aliceID, f.apiKey, `{"phone":"+15551234567","firstName":"Alice"}`); status != http.StatusOK {
		t.Fatalf("patch same: %d %s", status, body)
	}
	if s := f.sub(t, "alice"); s.PhoneVerifiedAt == "" {
		t.Fatalf("patching the same number cleared verification: %+v", s)
	}

	// A different number through the API is stored, unverified.
	if status, body := doPatch(t, f.srv.URL+"/v1/subscribers/"+aliceID, f.apiKey, `{"phone":"+15558887777"}`); status != http.StatusOK {
		t.Fatalf("patch new: %d %s", status, body)
	}
	if s := f.sub(t, "alice"); s.Phone != "+15558887777" || s.PhoneVerifiedAt != "" {
		t.Fatalf("after API change: %+v, want the new number unverified", s)
	}
	// So does re-creating through POST /v1/subscribers with another number.
	if status, body := req(t, http.MethodPost, f.srv.URL+"/v1/subscribers", f.apiKey, `{"subscriberId":"alice","phone":"+15551234567"}`); status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("re-post: %d %s", status, body)
	}
	if s := f.sub(t, "alice"); s.PhoneVerifiedAt != "" {
		t.Fatalf("API upsert produced a verified number: %+v", s)
	}
}
