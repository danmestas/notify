package phoneverify_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/phoneverify"
	"github.com/danmestas/notify/internal/providers"
)

func (f fixture) ensure(t *testing.T, id string) {
	t.Helper()
	if _, err := f.st.Subscribers().Upsert(context.Background(), f.scope, domain.Subscriber{SubscriberID: id}); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

func (f fixture) wrongFor(t *testing.T) (right, wrong string) {
	t.Helper()
	right = codeIn(t, f.sms.last(t).Body)
	if right == "000000" {
		return right, "111111"
	}
	return right, "000000"
}

// TestRequest_EnvironmentDailyCap: an environment sends at most
// MaxSendsPerEnvDay codes a day, however the sends are spread.
func TestRequest_EnvironmentDailyCap(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := 0; i < phoneverify.MaxSendsPerEnvDay; i++ {
		id := fmt.Sprintf("s%03d", i)
		f.ensure(t, id)
		if _, err := f.svc.Request(ctx, f.scope, id, fmt.Sprintf("+1555000%04d", i)); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	f.ensure(t, "late")
	_, err := f.svc.Request(ctx, f.scope, "late", "+15559999999")
	if !errors.Is(err, phoneverify.ErrEnvironmentDailyLimit) {
		t.Fatalf("over the environment cap: err = %v, want ErrEnvironmentDailyLimit", err)
	}
	// A day later it opens again.
	*f.now = f.now.Add(24*time.Hour + time.Second)
	if _, err := f.svc.Request(ctx, f.scope, "late", "+15559999999"); err != nil {
		t.Fatalf("next day: %v", err)
	}
}

// TestRequest_NumberDailySendCap: a number gets at most MaxSendsPerPhoneDay
// codes a day, even spread past the hourly window.
func TestRequest_NumberDailySendCap(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sent := 0
	for sent < phoneverify.MaxSendsPerPhoneDay {
		for i := 0; i < phoneverify.MaxSends && sent < phoneverify.MaxSendsPerPhoneDay; i++ {
			if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); err != nil {
				t.Fatalf("send %d: %v", sent+1, err)
			}
			sent++
		}
		*f.now = f.now.Add(phoneverify.SendWindow + time.Second)
	}
	_, err := f.svc.Request(ctx, f.scope, "bob", "+15551234567")
	if !errors.Is(err, phoneverify.ErrDailyLimit) {
		t.Fatalf("over the number's daily cap: err = %v, want ErrDailyLimit", err)
	}
	// Other numbers are unaffected.
	if _, err := f.svc.Request(ctx, f.scope, "bob", "+15557654321"); err != nil {
		t.Fatalf("another number: %v", err)
	}
}

// TestConfirm_NumberDailyFailureCap: wrong guesses against one number are
// bounded per day, not only per code, so requesting fresh codes does not buy
// unlimited guesses.
func TestConfirm_NumberDailyFailureCap(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	failures := 0
	for failures < phoneverify.MaxFailedConfirmsPerPhoneDay {
		if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); err != nil {
			t.Fatalf("request: %v", err)
		}
		_, wrong := f.wrongFor(t)
		for i := 0; i < phoneverify.MaxAttempts && failures < phoneverify.MaxFailedConfirmsPerPhoneDay; i++ {
			if _, err := f.svc.Confirm(ctx, f.scope, "alice", wrong); !errors.Is(err, phoneverify.ErrWrongCode) {
				t.Fatalf("failure %d: err = %v, want ErrWrongCode", failures+1, err)
			}
			failures++
		}
	}
	// A fresh code, the right one, is still refused today.
	if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); err != nil {
		t.Fatalf("request: %v", err)
	}
	right, _ := f.wrongFor(t)
	if _, err := f.svc.Confirm(ctx, f.scope, "alice", right); !errors.Is(err, phoneverify.ErrDailyLimit) {
		t.Fatalf("confirm past the failure cap: err = %v, want ErrDailyLimit", err)
	}
	if s := f.subscriber(t, "alice"); s.PhoneVerifiedAt != "" {
		t.Fatalf("verified past the failure cap: %+v", s)
	}
	// The cap is the number's: bob guessing at it is refused too.
	if _, err := f.svc.Request(ctx, f.scope, "bob", "+15551234567"); err != nil {
		t.Fatalf("bob request: %v", err)
	}
	right, _ = f.wrongFor(t)
	if _, err := f.svc.Confirm(ctx, f.scope, "bob", right); !errors.Is(err, phoneverify.ErrDailyLimit) {
		t.Fatalf("bob confirm: err = %v, want ErrDailyLimit", err)
	}
}

// TestConfirm_WritesTheNumberTheCodeWentTo: a tenant change to the phone
// between Request and Confirm does not win. The subscriber proved they hold
// the number the code was sent to, so that number ends up verified.
func TestConfirm_WritesTheNumberTheCodeWentTo(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Request(ctx, f.scope, "alice", "+15551234567"); err != nil {
		t.Fatalf("request: %v", err)
	}
	right, _ := f.wrongFor(t)
	if _, err := f.st.Subscribers().Upsert(ctx, f.scope, domain.Subscriber{SubscriberID: "alice", Phone: "+15550000000"}); err != nil {
		t.Fatalf("tenant patch: %v", err)
	}
	if _, err := f.svc.Confirm(ctx, f.scope, "alice", right); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if s := f.subscriber(t, "alice"); s.Phone != "+15551234567" || s.PhoneVerifiedAt == "" {
		t.Fatalf("alice = %+v, want the confirmed number, verified", s)
	}
}

// TestRequest_ProviderErrorNeverCarriesTheNumber: a provider error that
// echoes the recipient is redacted before it is stored or returned, and still
// classified from the original.
func TestRequest_ProviderErrorNeverCarriesTheNumber(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.sms.err = errors.New("twilio: status 401: The 'To' number +15551234567 (15551234567) is not allowed")
	_, err := f.svc.Request(ctx, f.scope, "alice", "+1 555 123 4567")
	if !errors.Is(err, phoneverify.ErrSendFailed) {
		t.Fatalf("err = %v, want ErrSendFailed", err)
	}
	if strings.Contains(err.Error(), "5551234567") {
		t.Errorf("returned error carries the number: %v", err)
	}
	msgs, _ := f.st.Messages().ListByIntegrationID(ctx, f.scope, "integ-1", 10)
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	if strings.Contains(msgs[0].ErrorText, "5551234567") {
		t.Errorf("stored ErrorText carries the number: %q", msgs[0].ErrorText)
	}
	if msgs[0].ErrorClass != string(providers.ClassAuth) {
		t.Errorf("ErrorClass = %q, want auth", msgs[0].ErrorClass)
	}
}
