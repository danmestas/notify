package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/names"
)

// fakeProgramTriggers answers api.p.notify.triggers.* the way dagnats main
// (8b2fc4b6, dagnats#958) does and records each request body. The dagnats this
// repo pins predates the endpoint, so a real sidecar cannot serve it; the
// no-responders case below does use the real sidecar.
func fakeProgramTriggers(t *testing.T, nc *nats.Conn, subject, reply string) chan []byte {
	t.Helper()
	got := make(chan []byte, 4)
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		got <- append([]byte(nil), m.Data...)
		_ = m.Respond([]byte(reply))
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return got
}

func newTriggerEngine(t *testing.T) (*engine.Engine, *nats.Conn) {
	t.Helper()
	return connectTriggerEngine(t, dagtest.StartSidecar(t))
}

// newFakeTriggerEngine connects to a bare NATS server, so fakeProgramTriggers
// is the only responder. On a real dagnats sidecar the engine also answers
// the triggers API, and whichever reply lands first wins.
func newFakeTriggerEngine(t *testing.T) (*engine.Engine, *nats.Conn) {
	t.Helper()
	return connectTriggerEngine(t, dagtest.StartBareNATS(t))
}

func connectTriggerEngine(t *testing.T, url string) (*engine.Engine, *nats.Conn) {
	t.Helper()
	nc, err := nats.Connect(url, engine.InboxOption())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return engine.FromConn(nc), nc
}

func TestUpsertTrigger_SendsCronTriggerDef(t *testing.T) {
	e, nc := newFakeTriggerEngine(t)
	got := fakeProgramTriggers(t, nc, "api.p.notify.triggers.create", `{"status":"created","id":"notify.health-verify-daily"}`)

	err := e.UpsertTrigger(context.Background(), engine.CronTrigger{
		ID:         names.HealthVerifyTrigger,
		WorkflowID: names.HealthVerifyAllWorkflow,
		Expression: "17 6 * * *",
		Timezone:   "UTC",
	})
	if err != nil {
		t.Fatalf("UpsertTrigger: %v", err)
	}
	var def struct {
		ID         string `json:"id"`
		WorkflowID string `json:"workflow_id"`
		Enabled    bool   `json:"enabled"`
		Cron       *struct {
			Expression string `json:"expression"`
			Timezone   string `json:"timezone"`
			Backfill   bool   `json:"backfill"`
		} `json:"cron"`
		Subject, Webhook, HTTP, External, RunTerminal, Debounce json.RawMessage
	}
	select {
	case body := <-got:
		if err := json.Unmarshal(body, &def); err != nil {
			t.Fatalf("body %s: %v", body, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no trigger request seen")
	}
	if !strings.HasPrefix(def.ID, "notify.") || !strings.HasPrefix(def.WorkflowID, "notify.") {
		t.Errorf("ids not under notify.: %q, %q", def.ID, def.WorkflowID)
	}
	if def.ID != "notify.health-verify-daily" || def.WorkflowID != "notify.health-verify-all" {
		t.Errorf("id/workflow = %q/%q", def.ID, def.WorkflowID)
	}
	if !def.Enabled || def.Cron == nil || def.Cron.Expression != "17 6 * * *" || def.Cron.Timezone != "UTC" || def.Cron.Backfill {
		t.Errorf("cron def = %+v enabled=%v", def.Cron, def.Enabled)
	}
}

func TestUpsertTrigger_RefusedIsAnErrorButNotUnsupported(t *testing.T) {
	e, nc := newFakeTriggerEngine(t)
	fakeProgramTriggers(t, nc, "api.p.notify.triggers.create", `{"error":"a program may register cron triggers only"}`)

	err := e.UpsertTrigger(context.Background(), engine.CronTrigger{ID: "notify.x", WorkflowID: "notify.y", Expression: "* * * * *"})
	if err == nil || errors.Is(err, engine.ErrTriggersUnsupported) {
		t.Fatalf("err = %v, want a refusal that is not ErrTriggersUnsupported", err)
	}
}

// TestUpsertTrigger_RealDagnatsCreatesAndDeletes runs against the real
// pinned dagnats (v0.0.55 or later serves program triggers, dagnats#957).
func TestUpsertTrigger_RealDagnatsCreatesAndDeletes(t *testing.T) {
	e, _ := newTriggerEngine(t)
	tr := engine.CronTrigger{ID: "notify.x", WorkflowID: "notify.y", Expression: "17 6 * * *", Timezone: "UTC"}
	if err := e.UpsertTrigger(context.Background(), tr); err != nil {
		t.Fatalf("UpsertTrigger: %v", err)
	}
	if err := e.UpsertTrigger(context.Background(), tr); err != nil {
		t.Fatalf("UpsertTrigger again (upsert): %v", err)
	}
	if err := e.DeleteTrigger(context.Background(), "notify.x"); err != nil {
		t.Fatalf("DeleteTrigger: %v", err)
	}
}

// TestUpsertTrigger_NoRespondersIsUnsupported runs against a bare NATS
// server, which is what a dagnats without program triggers looks like.
func TestUpsertTrigger_NoRespondersIsUnsupported(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartBareNATS(t), engine.InboxOption())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	e := engine.FromConn(nc)
	err = e.UpsertTrigger(context.Background(), engine.CronTrigger{ID: "notify.x", WorkflowID: "notify.y", Expression: "* * * * *"})
	if !errors.Is(err, engine.ErrTriggersUnsupported) {
		t.Fatalf("err = %v, want ErrTriggersUnsupported", err)
	}
	if err := e.DeleteTrigger(context.Background(), "notify.x"); !errors.Is(err, engine.ErrTriggersUnsupported) {
		t.Fatalf("delete err = %v, want ErrTriggersUnsupported", err)
	}
}

func TestDeleteTrigger_SendsRawIDAndToleratesMissing(t *testing.T) {
	e, nc := newFakeTriggerEngine(t)
	got := fakeProgramTriggers(t, nc, "api.p.notify.triggers.delete", `{"error":"trigger not found"}`)

	if err := e.DeleteTrigger(context.Background(), names.HealthVerifyTrigger); err != nil {
		t.Fatalf("DeleteTrigger of a missing trigger: %v", err)
	}
	select {
	case body := <-got:
		if string(body) != "notify.health-verify-daily" {
			t.Errorf("delete body = %q, want the raw trigger id", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delete request seen")
	}
}
