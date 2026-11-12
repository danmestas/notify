package connectivity

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/signalwire"
	"github.com/danmestas/notify/internal/providers/twilio"
	"github.com/danmestas/notify/internal/store"
)

// TestSMSCallback: a callback names its integration by id alone; the
// resolution carries the integration's scope, the key that signs its
// callbacks (Twilio's auth token, SignalWire's signing key), its help text and
// a sender for the reply.
func TestSMSCallback(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	a := domain.Scope{OrgID: "o1", EnvID: "e1"}
	b := domain.Scope{OrgID: "o2", EnvID: "e2"}

	tw, err := svc.CreateTwilio(ctx, a, "tw", TwilioConfig{AccountSID: "AC1", AuthToken: "tok", From: "+15550001111", HelpText: "Acme: reply STOP"}, true)
	if err != nil {
		t.Fatalf("CreateTwilio: %v", err)
	}
	sw, err := svc.CreateSignalWire(ctx, b, "sw", SignalWireConfig{ProjectID: "p", Token: "t", SpaceURL: "x.signalwire.com", From: "+15550002222", SigningKey: "PSK1"}, true)
	if err != nil {
		t.Fatalf("CreateSignalWire: %v", err)
	}

	cb, err := svc.SMSCallback(ctx, tw.ID)
	if err != nil {
		t.Fatalf("SMSCallback(twilio): %v", err)
	}
	if cb.Scope != a || cb.IntegrationID != tw.ID || cb.ProviderID != "twilio" || cb.SigningKey != "tok" || cb.HelpText != "Acme: reply STOP" || cb.From != "+15550001111" {
		t.Fatalf("twilio callback = %+v", cb)
	}
	if _, ok := cb.Sender.(*twilio.Client); !ok {
		t.Fatalf("sender = %T", cb.Sender)
	}

	cb, err = svc.SMSCallback(ctx, sw.ID)
	if err != nil {
		t.Fatalf("SMSCallback(signalwire): %v", err)
	}
	if cb.Scope != b || cb.ProviderID != "signalwire" || cb.SigningKey != "PSK1" || cb.HelpText != "" {
		t.Fatalf("signalwire callback = %+v", cb)
	}
	if _, ok := cb.Sender.(*signalwire.Client); !ok {
		t.Fatalf("sender = %T", cb.Sender)
	}

	if _, err := svc.SMSCallback(ctx, "no-such-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}
	mail, err := svc.CreateResend(ctx, a, "mail", ResendConfig{APIKey: "k", From: "a@x.test"}, true)
	if err != nil {
		t.Fatalf("CreateResend: %v", err)
	}
	if _, err := svc.SMSCallback(ctx, mail.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-sms integration err = %v, want ErrNotFound", err)
	}

	// A deactivated integration answers no callback.
	if err := svc.Store().Integrations().SetActive(ctx, a, tw.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if _, err := svc.SMSCallback(ctx, tw.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("inactive integration err = %v, want ErrNotFound", err)
	}
}

// TestHelpText_KeywordRejected: a HELP reply that is itself a keyword would
// answer a text with a command, so it is refused on create and on update.
func TestHelpText_KeywordRejected(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	if _, err := svc.CreateTwilio(ctx, scope, "tw", TwilioConfig{AccountSID: "AC1", AuthToken: "tok", From: "+1555", HelpText: " Stop "}, true); !errors.Is(err, ErrHelpTextIsKeyword) {
		t.Fatalf("CreateTwilio err = %v, want ErrHelpTextIsKeyword", err)
	}
	if _, err := svc.CreateSignalWire(ctx, scope, "sw", SignalWireConfig{ProjectID: "p", Token: "t", SpaceURL: "x", From: "+1", HelpText: "help"}, false); !errors.Is(err, ErrHelpTextIsKeyword) {
		t.Fatalf("CreateSignalWire err = %v, want ErrHelpTextIsKeyword", err)
	}
	in, err := svc.CreateTwilio(ctx, scope, "tw", TwilioConfig{AccountSID: "AC1", AuthToken: "tok", From: "+1555"}, true)
	if err != nil {
		t.Fatalf("CreateTwilio: %v", err)
	}
	if _, err := svc.Update(ctx, scope, in.ID, UpdateInput{Identifier: "tw", Credentials: map[string]string{"help_text": "yes"}}); !errors.Is(err, ErrHelpTextIsKeyword) {
		t.Fatalf("Update err = %v, want ErrHelpTextIsKeyword", err)
	}
}

// TestHelpText_Cleared: an empty help_text clears it back to the default; an
// absent or masked one keeps it.
func TestHelpText_Cleared(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	in, err := svc.CreateTwilio(ctx, scope, "tw", TwilioConfig{AccountSID: "AC1", AuthToken: "tok", From: "+1555", HelpText: "Acme help"}, true)
	if err != nil {
		t.Fatalf("CreateTwilio: %v", err)
	}
	help := func() string {
		t.Helper()
		cb, err := svc.SMSCallback(ctx, in.ID)
		if err != nil {
			t.Fatalf("SMSCallback: %v", err)
		}
		return cb.HelpText
	}
	for _, keep := range []map[string]string{{"from": "+1555"}, {"help_text": CredentialMaskSentinel}} {
		if _, err := svc.Update(ctx, scope, in.ID, UpdateInput{Identifier: "tw", Credentials: keep}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got := help(); got != "Acme help" {
			t.Fatalf("after %v help = %q, want it kept", keep, got)
		}
	}
	if _, err := svc.Update(ctx, scope, in.ID, UpdateInput{Identifier: "tw", Credentials: map[string]string{"help_text": ""}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := help(); got != "" {
		t.Fatalf("after clearing help = %q, want empty (the default)", got)
	}
	// An empty value still keeps every other credential.
	if cb, _ := svc.SMSCallback(ctx, in.ID); cb.SigningKey != "tok" {
		t.Fatalf("clearing help_text lost the auth token: %+v", cb)
	}
}
