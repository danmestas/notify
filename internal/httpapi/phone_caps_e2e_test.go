package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/phoneverify"
)

// errorMessage decodes the error envelope's message.
func errorMessage(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `"message":"`)
	if i < 0 {
		t.Fatalf("no message in %s", body)
	}
	rest := body[i+len(`"message":"`):]
	return rest[:strings.Index(rest, `"`)]
}

func TestPhoneVerification_NumberDailyCapIs429(t *testing.T) {
	f := newPhoneFixture(t)
	for sent := 0; sent < phoneverify.MaxSendsPerPhoneDay; {
		for i := 0; i < phoneverify.MaxSends && sent < phoneverify.MaxSendsPerPhoneDay; i++ {
			if status, body := f.request(t, f.aliceTok, "+15551234567"); status != http.StatusAccepted {
				t.Fatalf("send %d: %d %s", sent+1, status, body)
			}
			sent++
		}
		f.clock.advance(phoneverify.SendWindow + time.Second)
	}
	status, body := f.request(t, f.bobTok, "+15551234567")
	if status != http.StatusTooManyRequests || errorCode(t, body) != "daily_limit" {
		t.Fatalf("status=%d body=%s, want 429 daily_limit", status, body)
	}
	if msg := errorMessage(t, body); strings.Contains(msg, "phoneverify") || !strings.Contains(msg, "tomorrow") {
		t.Errorf("message %q is not plain", msg)
	}
}

func TestPhoneVerification_FailedConfirmDailyCapIs429(t *testing.T) {
	f := newPhoneFixture(t)
	for failures := 0; failures < phoneverify.MaxFailedConfirmsPerPhoneDay; {
		if status, body := f.request(t, f.aliceTok, "+15551234567"); status != http.StatusAccepted {
			t.Fatalf("request: %d %s", status, body)
		}
		wrong := "000000"
		if lastCode(t, f.capt, "+15551234567") == wrong {
			wrong = "111111"
		}
		for i := 0; i < phoneverify.MaxAttempts && failures < phoneverify.MaxFailedConfirmsPerPhoneDay; i++ {
			if status, body := f.confirm(t, f.aliceTok, wrong); status != http.StatusUnprocessableEntity {
				t.Fatalf("failure %d: %d %s", failures+1, status, body)
			}
			failures++
		}
	}
	if status, body := f.request(t, f.aliceTok, "+15551234567"); status != http.StatusAccepted {
		t.Fatalf("request: %d %s", status, body)
	}
	status, body := f.confirm(t, f.aliceTok, lastCode(t, f.capt, "+15551234567"))
	if status != http.StatusTooManyRequests || errorCode(t, body) != "daily_limit" {
		t.Fatalf("right code past the cap: status=%d body=%s, want 429 daily_limit", status, body)
	}
}

func TestPhoneVerification_EnvironmentDailyCapIs429(t *testing.T) {
	f := newPhoneFixture(t)
	ctx := context.Background()
	id := identity.New(f.st, bytesRepeat(0x2a, 32))
	session := func(sub string) string {
		if _, err := f.st.Subscribers().Upsert(ctx, f.scope, domain.Subscriber{SubscriberID: sub}); err != nil {
			t.Fatalf("upsert %s: %v", sub, err)
		}
		tok, _ := id.MintSubscriberSession(f.scope, sub)
		return "Bearer " + tok
	}
	for i := 0; i < phoneverify.MaxSendsPerEnvDay; i++ {
		if status, body := f.request(t, session(fmt.Sprintf("s%03d", i)), fmt.Sprintf("+1555000%04d", i)); status != http.StatusAccepted {
			t.Fatalf("send %d: %d %s", i+1, status, body)
		}
	}
	status, body := f.request(t, session("late"), "+15559999999")
	if status != http.StatusTooManyRequests || errorCode(t, body) != "environment_daily_limit" {
		t.Fatalf("status=%d body=%s, want 429 environment_daily_limit", status, body)
	}
}
