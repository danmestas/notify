package store

import (
	"context"
	"errors"
	"testing"
	"time"

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

	apply := func(scope domain.Scope, sid, status, class, text string) StatusCallbackResult {
		t.Helper()
		res, err := s.Messages().ApplyStatusCallback(ctx, scope, "int-1", sid, status, class, text)
		if err != nil {
			t.Fatalf("ApplyStatusCallback(%s, %s): %v", sid, status, err)
		}
		return res
	}
	if res := apply(scope, "SM123", "delivered", "", ""); !res.Changed || res.Message.ID != m.ID || res.Message.Status != "delivered" {
		t.Fatalf("sent -> delivered = %+v, want a change", res)
	}
	if _, err := s.Messages().ApplyStatusCallback(ctx, scope, "int-1", "SM123", "sent", "", ""); err == nil {
		t.Fatal("a callback to sent was accepted; only final statuses are")
	}
	for _, late := range []struct{ status, class, text string }{
		{"error", "recipient", "30003"}, {"delivered", "", ""},
	} {
		if res := apply(scope, "SM123", late.status, late.class, late.text); res.Changed || res.Buffered {
			t.Fatalf("delivered -> %s = %+v, want no change", late.status, res)
		}
	}
	got, _ = s.Messages().Get(ctx, scope, m.ID)
	if got.Status != "delivered" || got.ErrorClass != "" {
		t.Fatalf("after late callbacks: %+v, want still delivered", got)
	}

	// A different environment can never move the row; its callback only
	// buffers in its own environment.
	m2, _ := s.Messages().Create(ctx, scope, domain.Message{Channel: "sms", IntegrationID: "int-1", Status: "sent", ProviderMessageID: "SM456"})
	if res := apply(other, "SM456", "error", "recipient", "x"); res.Changed || !res.Buffered {
		t.Fatalf("another environment's callback = %+v, want buffered there", res)
	}
	if res := apply(scope, "SM456", "error", "recipient", "Twilio error 30003"); !res.Changed {
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
	first, created, _, err := s.InboundMessages().RecordSMS(ctx, scope, in, OptNone)
	if err != nil || !created {
		t.Fatalf("first RecordSMS: created=%v err=%v", created, err)
	}
	again, created, _, err := s.InboundMessages().RecordSMS(ctx, scope, in, OptNone)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("retry: created=%v err=%v id=%s want %s", created, err, again.ID, first.ID)
	}
	// The same body from another sender is another message.
	in2 := in
	in2.MessageID, in2.Sender, in2.SubscriberID, in2.PhoneVerified = "SM2", "+15553334444", "", false
	if _, created, _, err := s.InboundMessages().RecordSMS(ctx, scope, in2, OptNone); err != nil || !created {
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
	if changed, err := repo.OptIn(ctx, a, "+15551112222"); err != nil || changed {
		t.Fatalf("START with no opt-out: changed=%v err=%v, want no change", changed, err)
	}
	for i, want := range []bool{true, false} { // a repeated STOP is harmless
		if changed, err := repo.OptOut(ctx, a, "+15551112222", "int-1"); err != nil || changed != want {
			t.Fatalf("OptOut #%d: changed=%v err=%v, want %v", i, changed, err, want)
		}
	}
	if out, _ := repo.IsOptedOut(ctx, a, "+15551112222"); !out {
		t.Fatal("not opted out after STOP")
	}
	if out, _ := repo.IsOptedOut(ctx, b, "+15551112222"); out {
		t.Fatal("another environment sees the opt-out")
	}
	if changed, err := repo.OptIn(ctx, a, "+15551112222"); err != nil || !changed {
		t.Fatalf("OptIn: changed=%v err=%v", changed, err)
	}
	if out, _ := repo.IsOptedOut(ctx, a, "+15551112222"); out {
		t.Fatal("still opted out after START")
	}
}

// TestInboundMessageRepo_RecordSMSWithOptChange: the text and its keyword's
// opt change commit together. A failed opt change records nothing, so the
// provider's retry is a first delivery and applies it; a duplicate applies
// nothing again.
func TestInboundMessageRepo_RecordSMSWithOptChange(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	stop := InboundMessage{IntegrationID: "int-1", MessageID: "SMstop", Sender: "+15551112222", Recipient: "+15559990000", Body: "STOP"}

	writer, _ := s.Pools()
	if _, err := writer.ExecContext(ctx, `CREATE TRIGGER fail_opt_out BEFORE INSERT ON sms_opt_outs
		BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if _, _, _, err := s.InboundMessages().RecordSMS(ctx, scope, stop, OptOut); err == nil {
		t.Fatal("RecordSMS succeeded through a failing opt-out")
	}
	if list, _ := s.InboundMessages().List(ctx, scope, 0, 10); len(list) != 0 {
		t.Fatalf("the text was kept without its opt-out: %+v", list)
	}
	if _, err := writer.ExecContext(ctx, `DROP TRIGGER fail_opt_out`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	_, created, changed, err := s.InboundMessages().RecordSMS(ctx, scope, stop, OptOut)
	if err != nil || !created || !changed {
		t.Fatalf("retry: created=%v changed=%v err=%v", created, changed, err)
	}
	if out, _ := s.SMSOptOuts().IsOptedOut(ctx, scope, "+15551112222"); !out {
		t.Fatal("not opted out after the retry")
	}

	// A later START, then a duplicate of the old STOP: the duplicate changes
	// nothing, so it cannot undo the newer START.
	start := InboundMessage{IntegrationID: "int-1", MessageID: "SMstart", Sender: "+15551112222", Recipient: "+15559990000", Body: "START"}
	if _, _, changed, err := s.InboundMessages().RecordSMS(ctx, scope, start, OptIn); err != nil || !changed {
		t.Fatalf("START: changed=%v err=%v", changed, err)
	}
	if _, created, changed, err := s.InboundMessages().RecordSMS(ctx, scope, stop, OptOut); err != nil || created || changed {
		t.Fatalf("duplicate STOP: created=%v changed=%v err=%v, want neither", created, changed, err)
	}
	if out, _ := s.SMSOptOuts().IsOptedOut(ctx, scope, "+15551112222"); out {
		t.Fatal("a duplicate STOP undid a newer START")
	}
}

// TestMessageRepo_StatusCallbackBeforeTheMessage: a final status for a
// provider id notify has not stored yet is buffered and applied when the
// message is written; one older than the buffer's life is not.
func TestMessageRepo_StatusCallbackBeforeTheMessage(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	res, err := s.Messages().ApplyStatusCallback(ctx, scope, "int-1", "SMearly", "error", "recipient", "twilio: failed (error 30003)")
	if err != nil || !res.Buffered || res.Changed {
		t.Fatalf("early callback = %+v, %v; want buffered", res, err)
	}
	// Another environment's message with the same provider id is untouched.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	om, _ := s.Messages().Create(ctx, other, domain.Message{Channel: "sms", IntegrationID: "int-1", Status: "sent", ProviderMessageID: "SMearly"})
	if om.Status != "sent" {
		t.Fatalf("another environment's message took the buffered status: %+v", om)
	}

	m, err := s.Messages().Create(ctx, scope, domain.Message{Channel: "sms", IntegrationID: "int-1", Status: "sent", ProviderMessageID: "SMearly"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.Status != "error" || m.ErrorClass != "recipient" || m.ErrorText != "twilio: failed (error 30003)" {
		t.Fatalf("created = %+v, want the buffered failure applied", m)
	}
	// Consumed: a second message with the id (another send) starts at sent.
	m2, _ := s.Messages().Create(ctx, scope, domain.Message{Channel: "sms", IntegrationID: "int-1", Status: "sent", ProviderMessageID: "SMearly"})
	if m2.Status != "sent" {
		t.Fatalf("buffered status applied twice: %+v", m2)
	}

	// A found message advances as before.
	if _, err := s.Messages().Create(ctx, scope, domain.Message{Channel: "sms", IntegrationID: "int-1", Status: "sent", ProviderMessageID: "SMlate"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err = s.Messages().ApplyStatusCallback(ctx, scope, "int-1", "SMlate", "delivered", "", "")
	if err != nil || res.Buffered || !res.Changed || res.Message.Status != "delivered" {
		t.Fatalf("callback for a stored message = %+v, %v", res, err)
	}

	// A buffered status past its life is dropped, not applied.
	writer, _ := s.Pools()
	if _, err := s.Messages().ApplyStatusCallback(ctx, scope, "int-1", "SMstale", "delivered", "", ""); err != nil {
		t.Fatalf("buffer: %v", err)
	}
	if _, err := writer.ExecContext(ctx, `UPDATE sms_status_pending SET received_at = '2000-01-01T00:00:00Z'`); err != nil {
		t.Fatalf("age: %v", err)
	}
	stale, _ := s.Messages().Create(ctx, scope, domain.Message{Channel: "sms", IntegrationID: "int-1", Status: "sent", ProviderMessageID: "SMstale"})
	if stale.Status != "sent" {
		t.Fatalf("a stale buffered status was applied: %+v", stale)
	}
}

// TestSMSOptOutRepo_ClaimHelpReply: one HELP reply per number per
// integration per window.
func TestSMSOptOutRepo_ClaimHelpReply(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	a := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.SMSOptOuts()
	claim := func(scope domain.Scope, integration, phone string) bool {
		t.Helper()
		ok, err := repo.ClaimHelpReply(ctx, scope, integration, phone, 10*time.Minute)
		if err != nil {
			t.Fatalf("ClaimHelpReply: %v", err)
		}
		return ok
	}
	if !claim(a, "int-1", "+15551112222") {
		t.Fatal("first HELP refused")
	}
	if claim(a, "int-1", "+15551112222") {
		t.Fatal("second HELP within the window allowed")
	}
	if !claim(a, "int-2", "+15551112222") || !claim(a, "int-1", "+15553334444") || !claim(domain.Scope{OrgID: "o1", EnvID: "e2"}, "int-1", "+15551112222") {
		t.Fatal("another integration, number or environment was limited")
	}
	writer, _ := s.Pools()
	if _, err := writer.ExecContext(ctx, `UPDATE sms_help_replies SET sent_at = '2000-01-01T00:00:00Z'`); err != nil {
		t.Fatalf("age: %v", err)
	}
	if !claim(a, "int-1", "+15551112222") {
		t.Fatal("HELP refused after the window")
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
