package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/providers"
)

// TestDigest_EveryChannelStepSeesTheWholeBatch is the #105 regression: a digest
// followed by in_app, email and sms steps in ONE run must render every channel
// from the same five events. Each step used to drain the window, so only the
// first saw them.
func TestDigest_EveryChannelStepSeesTheWholeBatch(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	bus, err := eventbus.Connect(cctx, dagtest.StartSidecar(t))
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { bus.Close() })

	const key = "env_1:dg_user:digested"
	for i := range 5 {
		if err := bus.DigestAppend(key, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatalf("DigestAppend: %v", err)
		}
	}

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	body := "{{digest.count}} items"
	steps := json.RawMessage(`[
	 {"stepId":"dg","type":"digest","controlValues":{"amount":1,"unit":"seconds"}},
	 {"stepId":"S1","type":"in_app","controlValues":{"subject":"D","body":"` + body + `"}},
	 {"stepId":"S2","type":"email","controlValues":{"subject":"D","body":"` + body + `"}},
	 {"stepId":"S3","type":"sms","controlValues":{"subject":"D","body":"` + body + `"}}]`)
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name: "Digested", TriggerIdentifier: "digested", Active: true, Steps: steps,
	})
	if err != nil {
		t.Fatalf("Workflows.Create: %v", err)
	}
	if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{
		SubscriberID: "dg_user", Email: "dg@x.io", Phone: "+15550001111",
	}); err != nil {
		t.Fatalf("Subscribers.Upsert: %v", err)
	}
	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID: "ntf_dg", WorkflowID: wf.ID, SubscriberID: "dg_user", TransactionID: "txn_dg",
		RunID: "r_dg", DigestKey: key, Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Notifications.Create: %v", err)
	}

	inApp := New(s).WithDigester(bus)
	email := NewEmail(s, stubResolver{provider: providers.NewCapture(), from: "noreply@acme.io"}).WithDigester(bus)
	sms := NewSMS(s, stubSMSResolver{p: providers.NewCaptureSMS(), id: "twilio"}).WithDigester(bus)

	run := func(name, step string, h func(context.Context, engine.TaskPayload) (json.RawMessage, error)) {
		t.Helper()
		if _, err := h(ctx, engine.TaskPayload{RunID: "r_dg", StepID: step}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	assertAll := func(channel string, wantMsgs int) {
		t.Helper()
		msgs, err := s.Messages().List(ctx, scope, "dg_user", channel)
		if err != nil {
			t.Fatalf("List %s: %v", channel, err)
		}
		if len(msgs) != wantMsgs {
			t.Fatalf("%s: %d messages, want %d", channel, len(msgs), wantMsgs)
		}
		for _, m := range msgs {
			if m.Content != "5 items" {
				t.Fatalf("%s content = %q, want %q", channel, m.Content, "5 items")
			}
		}
	}

	run("in_app", "S1", inApp.HandleInApp)
	run("email", "S2", email.HandleEmail)
	run("sms", "S3", sms.HandleSMS)
	assertAll("in_app", 1)
	assertAll("email", 1)
	assertAll("sms", 1)

	// A retried email step re-reads the same snapshot instead of an empty list.
	run("email retry", "S2", email.HandleEmail)
	msgs, err := s.Messages().List(ctx, scope, "dg_user", "email")
	if err != nil || len(msgs) == 0 {
		t.Fatalf("List email after retry: %d msgs, err=%v", len(msgs), err)
	}
	for _, m := range msgs {
		if m.Content != "5 items" {
			t.Fatalf("retried email content = %q, want %q", m.Content, "5 items")
		}
	}
}
