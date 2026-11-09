package phoneverify_test

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/phoneverify"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
)

// fakeSMS records every message and can be told to fail.
type fakeSMS struct {
	mu   sync.Mutex
	sent []providers.SMSMessage
	err  error
}

func (f *fakeSMS) Send(_ context.Context, m providers.SMSMessage) (providers.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return providers.Result{}, f.err
	}
	f.sent = append(f.sent, m)
	return providers.Result{ProviderMessageID: "SM1"}, nil
}

func (f *fakeSMS) last(t *testing.T) providers.SMSMessage {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("no sms sent")
	}
	return f.sent[len(f.sent)-1]
}

type resolver struct{ p providers.SMSProvider }

func (r resolver) ResolveSMS(context.Context, domain.Scope) (providers.SMSProvider, string, string, error) {
	return r.p, "twilio", "integ-1", nil
}

var codeRE = regexp.MustCompile(`\b\d{6}\b`)

func codeIn(t *testing.T, body string) string {
	t.Helper()
	c := codeRE.FindString(body)
	if c == "" {
		t.Fatalf("no 6-digit code in %q", body)
	}
	return c
}

type fixture struct {
	st    *store.Store
	svc   *phoneverify.Service
	sms   *fakeSMS
	scope domain.Scope
	now   *time.Time
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	for _, id := range []string{"alice", "bob"} {
		if _, err := st.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: id}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	sms := &fakeSMS{}
	svc := phoneverify.New(st, resolver{sms}).WithClock(func() time.Time { return now })
	return fixture{st: st, svc: svc, sms: sms, scope: scope, now: &now}
}

func (f fixture) subscriber(t *testing.T, id string) domain.Subscriber {
	t.Helper()
	sub, err := f.st.Subscribers().GetByExternalID(context.Background(), f.scope, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return sub
}

func TestRequestConfirm_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	pending, err := f.svc.Request(ctx, f.scope, "alice", "+1 555 123 4567")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if pending.Phone != "+15551234567" {
		t.Errorf("pending phone = %q, want E.164", pending.Phone)
	}
	if want := f.now.Add(phoneverify.CodeTTL); !pending.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", pending.ExpiresAt, want)
	}
	msg := f.sms.last(t)
	if msg.To != "+15551234567" {
		t.Errorf("sms to %q, want the normalised number", msg.To)
	}
	code := codeIn(t, msg.Body)

	// Requesting alone verifies nothing.
	if sub := f.subscriber(t, "alice"); sub.PhoneVerifiedAt != "" {
		t.Fatalf("verified before confirm: %+v", sub)
	}

	verified, err := f.svc.Confirm(ctx, f.scope, "alice", code)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if verified.Phone != "+15551234567" {
		t.Errorf("verified phone = %q", verified.Phone)
	}
	sub := f.subscriber(t, "alice")
	if sub.Phone != "+15551234567" || sub.PhoneVerifiedAt == "" {
		t.Fatalf("subscriber after confirm: phone=%q verifiedAt=%q", sub.Phone, sub.PhoneVerifiedAt)
	}

	// The code is single use.
	if _, err := f.svc.Confirm(ctx, f.scope, "alice", code); !errors.Is(err, phoneverify.ErrNoPendingCode) {
		t.Errorf("second Confirm err = %v, want ErrNoPendingCode", err)
	}
}

func TestRequest_StoresOnlyAHashAndRecordsTheSendWithoutTheCode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	code := codeIn(t, f.sms.last(t).Body)

	row, err := f.st.PhoneVerifications().Latest(ctx, f.scope, "alice")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if row.CodeHash == "" || strings.Contains(row.CodeHash, code) {
		t.Fatalf("code hash %q is empty or carries the code", row.CodeHash)
	}

	// The send is a delivery like any other: one sms message on the
	// integration, so delivery health counts it. It never carries the code.
	msgs, err := f.st.Messages().ListByIntegrationID(ctx, f.scope, "integ-1", 10)
	if err != nil {
		t.Fatalf("ListByIntegration: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages on the integration = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Channel != "sms" || m.Status != "sent" || m.SubscriberID != "alice" || m.ProviderID != "twilio" {
		t.Errorf("message = %+v", m)
	}
	if strings.Contains(m.Content, code) || strings.Contains(m.Subject, code) {
		t.Errorf("recorded message leaks the code: %+v", m)
	}
}

func TestConfirm_WrongCode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	code := codeIn(t, f.sms.last(t).Body)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if _, err := f.svc.Confirm(ctx, f.scope, "alice", wrong); !errors.Is(err, phoneverify.ErrWrongCode) {
		t.Fatalf("Confirm(wrong) err = %v, want ErrWrongCode", err)
	}
	if sub := f.subscriber(t, "alice"); sub.PhoneVerifiedAt != "" || sub.Phone != "" {
		t.Fatalf("wrong code changed the subscriber: %+v", sub)
	}
	// The right code still works after a miss.
	if _, err := f.svc.Confirm(ctx, f.scope, "alice", code); err != nil {
		t.Fatalf("Confirm(right) after a miss: %v", err)
	}
}

func TestConfirm_ExpiredCode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	code := codeIn(t, f.sms.last(t).Body)
	*f.now = f.now.Add(phoneverify.CodeTTL + time.Second)
	if _, err := f.svc.Confirm(ctx, f.scope, "alice", code); !errors.Is(err, phoneverify.ErrCodeExpired) {
		t.Fatalf("Confirm(expired) err = %v, want ErrCodeExpired", err)
	}
	if sub := f.subscriber(t, "alice"); sub.PhoneVerifiedAt != "" {
		t.Fatalf("expired code verified the phone: %+v", sub)
	}
}

func TestConfirm_AttemptLimit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	code := codeIn(t, f.sms.last(t).Body)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < phoneverify.MaxAttempts; i++ {
		if _, err := f.svc.Confirm(ctx, f.scope, "alice", wrong); !errors.Is(err, phoneverify.ErrWrongCode) {
			t.Fatalf("attempt %d err = %v, want ErrWrongCode", i+1, err)
		}
	}
	// Even the right code is refused once the attempts are spent.
	if _, err := f.svc.Confirm(ctx, f.scope, "alice", code); !errors.Is(err, phoneverify.ErrTooManyAttempts) {
		t.Fatalf("Confirm after limit err = %v, want ErrTooManyAttempts", err)
	}
	if sub := f.subscriber(t, "alice"); sub.PhoneVerifiedAt != "" {
		t.Fatalf("phone verified past the attempt limit: %+v", sub)
	}
}

func TestRequest_RateLimitedPerSubscriberAndPerNumber(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := 0; i < phoneverify.MaxSends; i++ {
		if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	if _, err := f.svc.Request(ctx, f.scope, "alice", "+15559990000"); !errors.Is(err, phoneverify.ErrRateLimited) {
		t.Fatalf("subscriber over limit err = %v, want ErrRateLimited", err)
	}
	// Another subscriber cannot use the same number to get past the limit.
	if _, err := f.svc.Request(ctx, f.scope, "bob", "+15551234567"); !errors.Is(err, phoneverify.ErrRateLimited) {
		t.Fatalf("number over limit err = %v, want ErrRateLimited", err)
	}
	if got := len(f.sms.sent); got != phoneverify.MaxSends {
		t.Errorf("sms sent = %d, want %d", got, phoneverify.MaxSends)
	}
	// The window passes and sending works again.
	*f.now = f.now.Add(phoneverify.SendWindow + time.Second)
	if _, err := f.svc.Request(ctx, f.scope, "bob", "+15551234567"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestConfirm_OnlyTheRequestingSubscriber(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	code := codeIn(t, f.sms.last(t).Body)
	if _, err := f.svc.Confirm(ctx, f.scope, "bob", code); !errors.Is(err, phoneverify.ErrNoPendingCode) {
		t.Fatalf("bob confirming alice's code err = %v, want ErrNoPendingCode", err)
	}
	if sub := f.subscriber(t, "bob"); sub.Phone != "" || sub.PhoneVerifiedAt != "" {
		t.Fatalf("bob gained a phone: %+v", sub)
	}
}

func TestRequest_RejectsInvalidNumberAndUnknownSubscriber(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Request(ctx, f.scope, "alice", "555-1234"); !errors.Is(err, phoneverify.ErrInvalidPhone) {
		t.Errorf("invalid number err = %v, want ErrInvalidPhone", err)
	}
	if _, err := f.svc.Request(ctx, f.scope, "ghost", "+15551234567"); !errors.Is(err, phoneverify.ErrUnknownSubscriber) {
		t.Errorf("unknown subscriber err = %v, want ErrUnknownSubscriber", err)
	}
	if len(f.sms.sent) != 0 {
		t.Errorf("sms sent = %d, want 0", len(f.sms.sent))
	}
}

func TestRequest_SendFailureIsReportedAndRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.sms.err = errors.New("twilio: status 401: authenticate")
	if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); !errors.Is(err, phoneverify.ErrSendFailed) {
		t.Fatalf("err = %v, want ErrSendFailed", err)
	}
	msgs, err := f.st.Messages().ListByIntegrationID(ctx, f.scope, "integ-1", 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages = %v, %v; want one", msgs, err)
	}
	if msgs[0].Status != "error" || msgs[0].ErrorClass != string(providers.ClassAuth) {
		t.Errorf("failed send recorded as %+v, want an auth-class error", msgs[0])
	}
}
