package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestMessageRepo_ProviderMessageID_FindAndAdvance: a message keeps the id the
// provider gave it at send, a status callback finds it by that id within its
// integration, and its status only moves forward from sent.
func TestMessageRepo_ProviderMessageID_FindAndAdvance(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	m, err := s.Messages().Create(ctx, scope, domain.Message{
		Channel: "sms", ProviderID: "twilio", IntegrationID: "int-1", Status: "sent", ProviderMessageID: "SM123",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.Messages().GetByProviderMessageID(ctx, scope, "int-1", "SM123")
	if err != nil || got.ID != m.ID || got.ProviderMessageID != "SM123" {
		t.Fatalf("GetByProviderMessageID = %+v, %v; want message %s", got, err, m.ID)
	}
	if _, err := s.Messages().GetByProviderMessageID(ctx, scope, "int-2", "SM123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another integration's lookup err = %v, want ErrNotFound", err)
	}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	if _, err := s.Messages().GetByProviderMessageID(ctx, other, "int-1", "SM123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another environment's lookup err = %v, want ErrNotFound", err)
	}

	changed, err := s.Messages().AdvanceStatus(ctx, scope, m.ID, "delivered", "", "")
	if err != nil || !changed {
		t.Fatalf("sent -> delivered: changed=%v err=%v, want a change", changed, err)
	}
	if _, err := s.Messages().AdvanceStatus(ctx, scope, m.ID, "sent", "", ""); err == nil {
		t.Fatal("advancing to sent was accepted; only final statuses are")
	}
	for _, late := range []struct{ status, class, text string }{
		{"error", "recipient", "30003"}, {"delivered", "", ""},
	} {
		changed, err = s.Messages().AdvanceStatus(ctx, scope, m.ID, late.status, late.class, late.text)
		if err != nil || changed {
			t.Fatalf("delivered -> %s: changed=%v err=%v, want no change", late.status, changed, err)
		}
	}
	got, _ = s.Messages().Get(ctx, scope, m.ID)
	if got.Status != "delivered" || got.ErrorClass != "" {
		t.Fatalf("after late callbacks: %+v, want still delivered", got)
	}

	// A different environment can never move the row.
	m2, _ := s.Messages().Create(ctx, scope, domain.Message{Channel: "sms", IntegrationID: "int-1", Status: "sent", ProviderMessageID: "SM456"})
	if changed, _ := s.Messages().AdvanceStatus(ctx, other, m2.ID, "error", "recipient", "x"); changed {
		t.Fatal("another environment advanced the message")
	}
	if changed, _ := s.Messages().AdvanceStatus(ctx, scope, m2.ID, "error", "recipient", "Twilio error 30003"); !changed {
		t.Fatal("sent -> error made no change")
	}
	got, _ = s.Messages().Get(ctx, scope, m2.ID)
	if got.Status != "error" || got.ErrorClass != "recipient" || got.ErrorText != "Twilio error 30003" {
		t.Fatalf("after failure: %+v", got)
	}
}

// TestInboundMessageRepo_RecordSMS: an inbound text is kept once per provider
// message id, listed beside inbound mail with its channel and body, and a
// provider retry adds nothing.
func TestInboundMessageRepo_RecordSMS(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	in := InboundMessage{
		Channel: "sms", IntegrationID: "int-1", MessageID: "SM1", Sender: "+15551112222",
		Recipient: "+15559990000", Body: "hello", SubscriberID: "sub-1", PhoneVerified: true,
	}
	first, created, err := s.InboundMessages().RecordSMS(ctx, scope, in)
	if err != nil || !created {
		t.Fatalf("first RecordSMS: created=%v err=%v", created, err)
	}
	again, created, err := s.InboundMessages().RecordSMS(ctx, scope, in)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("retry: created=%v err=%v id=%s want %s", created, err, again.ID, first.ID)
	}
	// The same body from another sender is another message.
	in2 := in
	in2.MessageID, in2.Sender, in2.SubscriberID, in2.PhoneVerified = "SM2", "+15553334444", "", false
	if _, created, err := s.InboundMessages().RecordSMS(ctx, scope, in2); err != nil || !created {
		t.Fatalf("second sender: created=%v err=%v", created, err)
	}
	// Mail still dedupes by content and recipient, untouched by sms rows.
	if _, err := s.InboundMessages().Record(ctx, scope, InboundMessage{SHA256: "abc", Size: 3, Recipient: "a@x.test"}); err != nil {
		t.Fatalf("record mail: %v", err)
	}
	if _, err := s.InboundMessages().Record(ctx, scope, InboundMessage{SHA256: "abc", Size: 3, Recipient: "a@x.test"}); err != nil {
		t.Fatalf("record mail retry: %v", err)
	}

	list, err := s.InboundMessages().List(ctx, scope, 0, 50)
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %d rows, %v; want 3", len(list), err)
	}
	if list[0].Channel != "sms" || list[0].Body != "hello" || list[0].SubscriberID != "sub-1" || !list[0].PhoneVerified {
		t.Fatalf("sms row = %+v", list[0])
	}
	if list[2].Channel != "email" {
		t.Fatalf("mail row channel = %q, want email", list[2].Channel)
	}
	if other, _ := s.InboundMessages().List(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"}, 0, 50); len(other) != 0 {
		t.Fatalf("another environment sees %d rows", len(other))
	}

	texts, err := s.InboundMessages().ListSMSByIntegration(ctx, scope, "int-1", 10)
	if err != nil || len(texts) != 2 || texts[0].MessageID != "SM2" || texts[1].MessageID != "SM1" {
		t.Fatalf("ListSMSByIntegration = %+v, %v; want SM2 then SM1", texts, err)
	}
	if none, _ := s.InboundMessages().ListSMSByIntegration(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"}, "int-1", 10); len(none) != 0 {
		t.Fatalf("another environment lists %d texts", len(none))
	}
}

// TestSMSOptOutRepo: STOP records the number as opted out in its environment
// only; START clears it.
func TestSMSOptOutRepo(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	a := domain.Scope{OrgID: "o1", EnvID: "e1"}
	b := domain.Scope{OrgID: "o1", EnvID: "e2"}
	repo := s.SMSOptOuts()
	if out, err := repo.IsOptedOut(ctx, a, "+15551112222"); err != nil || out {
		t.Fatalf("fresh: out=%v err=%v", out, err)
	}
	for range 2 { // a repeated STOP is harmless
		if err := repo.OptOut(ctx, a, "+15551112222", "int-1"); err != nil {
			t.Fatalf("OptOut: %v", err)
		}
	}
	if out, _ := repo.IsOptedOut(ctx, a, "+15551112222"); !out {
		t.Fatal("not opted out after STOP")
	}
	if out, _ := repo.IsOptedOut(ctx, b, "+15551112222"); out {
		t.Fatal("another environment sees the opt-out")
	}
	if err := repo.OptIn(ctx, a, "+15551112222"); err != nil {
		t.Fatalf("OptIn: %v", err)
	}
	if out, _ := repo.IsOptedOut(ctx, a, "+15551112222"); out {
		t.Fatal("still opted out after START")
	}
}

// TestSubscriberRepo_ListByPhone: subscribers with a phone, verified ones
// first, live ones only, in the one environment.
func TestSubscriberRepo_ListByPhone(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.Subscribers()
	for _, id := range []string{"unverified", "verified", "gone"} {
		if _, err := repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: id, Phone: "+1 555 111 2222"}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	if err := repo.SetVerifiedPhone(ctx, scope, "verified", "+15551112222", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	gone, _ := repo.GetByExternalID(ctx, scope, "gone")
	if err := repo.SoftDelete(ctx, scope, gone.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Upsert(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"}, domain.Subscriber{SubscriberID: "elsewhere", Phone: "+15551112222"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	subs, err := repo.ListByPhone(ctx, scope, "+15551112222")
	if err != nil {
		t.Fatalf("ListByPhone: %v", err)
	}
	if len(subs) != 2 || subs[0].SubscriberID != "verified" || subs[1].SubscriberID != "unverified" {
		t.Fatalf("subs = %+v, want verified then unverified", subs)
	}
}
