package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// TestResolveDynamicDuration_ISO8601_Future proves a future RFC3339 timestamp
// at the dynamicKey path resolves to the remaining duration until that instant.
func TestResolveDynamicDuration_ISO8601_Future(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{"sendAt":"2026-06-29T13:00:00Z"}`)
	got, err := resolveDynamicDuration(payload, "payload.sendAt", steptype.DynamicKindInstant, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != time.Hour {
		t.Fatalf("duration = %s, want 1h", got)
	}
}

// TestResolveDynamicDuration_ISO8601_Past proves a past timestamp clamps to
// zero (no negative sleep, no error).
func TestResolveDynamicDuration_ISO8601_Past(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{"sendAt":"2026-06-29T11:00:00Z"}`)
	got, err := resolveDynamicDuration(payload, "payload.sendAt", steptype.DynamicKindInstant, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != 0 {
		t.Fatalf("duration = %s, want 0", got)
	}
}

// TestResolveDynamicDuration_AmountUnit proves an {amount,unit} object (nested
// via a dot-path) resolves to amount*unit, honouring the shared unit table.
func TestResolveDynamicDuration_AmountUnit(t *testing.T) {
	now := time.Now()
	payload := json.RawMessage(`{"wait":{"amount":2,"unit":"hours"}}`)
	got, err := resolveDynamicDuration(payload, "payload.wait", steptype.DynamicKindRelative, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != 2*time.Hour {
		t.Fatalf("duration = %s, want 2h", got)
	}
}

// TestResolveDynamicDuration_Errors proves bad inputs (missing key, non-object
// intermediate, unparseable value, bad path root) all error.
func TestResolveDynamicDuration_Errors(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		payload string
		key     string
		kind    string
	}{
		{"missing key", `{"other":"x"}`, "payload.sendAt", steptype.DynamicKindInstant},
		{"bad root", `{"sendAt":"2026-06-29T13:00:00Z"}`, "sendAt", steptype.DynamicKindInstant},
		{"not a timestamp", `{"sendAt":"tomorrow"}`, "payload.sendAt", steptype.DynamicKindInstant},
		{"zero amount", `{"wait":{"amount":0,"unit":"hours"}}`, "payload.wait", steptype.DynamicKindRelative},
		{"empty key", `{}`, "", steptype.DynamicKindInstant},
		// New under #258: the declared kind and the actual value must agree.
		// A mismatch is bad data, not a cue to reinterpret.
		{"declared instant, got object", `{"sendAt":{"amount":2,"unit":"hours"}}`, "payload.sendAt", steptype.DynamicKindInstant},
		{"declared relative, got string", `{"wait":"2026-06-29T13:00:00Z"}`, "payload.wait", steptype.DynamicKindRelative},
		{"no declared kind", `{"sendAt":"2026-06-29T13:00:00Z"}`, "payload.sendAt", ""},
		{"unknown kind", `{"sendAt":"2026-06-29T13:00:00Z"}`, "payload.sendAt", "sometime"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveDynamicDuration(json.RawMessage(tc.payload), tc.key, tc.kind, now); err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}

// TestWaitUntil_PastDeadlineReturnsImmediately proves a deadline that has
// arrived fires now: (nil, nil), nothing parked.
func TestWaitUntil_PastDeadlineReturnsImmediately(t *testing.T) {
	out, err := waitUntil(time.Now().Add(-time.Second))
	if err != nil || out != nil {
		t.Fatalf("waitUntil(past) = (%v, %v), want (nil, nil)", out, err)
	}
}

// TestWaitUntil_FutureDeadlineParks proves a pending wait never blocks its
// worker: it returns at once with the engine's park request for that exact
// deadline, so dagnats holds the task (durably, across restarts) instead of a
// goroutine sleeping out the wait.
func TestWaitUntil_FutureDeadlineParks(t *testing.T) {
	fire := time.Now().Add(time.Hour)
	begin := time.Now()
	out, err := waitUntil(fire)
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("waitUntil blocked for %v; it must park, not sleep", took)
	}
	var we *engine.WaitError
	if !errors.As(err, &we) || !we.At.Equal(fire) {
		t.Fatalf("err = %v (%T), want *engine.WaitError at %v", err, err, fire)
	}
	if out != nil {
		t.Fatalf("out = %s, want nil", out)
	}
}

// seedDelayRun creates a workflow with a single delay step (controlValues cv)
// and a notification/run over payload, returning the run id and scope so the
// caller can dispatch HandleScheduled/HandleDynamic against it.
func seedDelayRun(t *testing.T, s *store.Store, runID, stepID string, cv json.RawMessage, payload json.RawMessage) domain.Scope {
	t.Helper()
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	steps, err := json.Marshal([]map[string]any{
		{"stepId": stepID, "type": "delay", "controlValues": cv},
	})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "delay-" + runID,
		TriggerIdentifier: "delay-" + runID,
		Active:            true,
		Steps:             steps,
	})
	if err != nil {
		t.Fatalf("Workflows.Create: %v", err)
	}
	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID:            "ntf_" + runID,
		WorkflowID:    wf.ID,
		SubscriberID:  "user_1",
		TransactionID: "txn_" + runID,
		RunID:         runID,
		Payload:       payload,
	}); err != nil {
		t.Fatalf("Notifications.Create: %v", err)
	}
	return scope
}

// dispatchParked calls HandleDynamic/HandleScheduled the way the engine first
// dispatches a delay: its lookups and deadline resolution run, and it returns
// the park request rather than sleeping out the (possibly month-long) wait.
// It returns the deadline the handler parked until.
func dispatchParked(t *testing.T, fn func(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error), payload engine.TaskPayload) time.Time {
	t.Helper()
	_, err := fn(context.Background(), payload)
	var we *engine.WaitError
	if !errors.As(err, &we) {
		t.Fatalf("dispatch: err = %v (%T), want *engine.WaitError", err, err)
	}
	return we.At
}

// TestHandleDynamic_Relative_RequeueResumesOriginalDeadline is the core
// #273 regression: a relative dynamic delay's first dispatch persists a
// fire-at deadline; a requeue (simulated here by dispatching again, as a
// resume or a restart does) must resume toward that SAME
// deadline rather than resolving a new one an hour later.
func TestHandleDynamic_Relative_RequeueResumesOriginalDeadline(t *testing.T) {
	s := newStore(t)
	d := NewDelayWait(s)

	cv := json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.wait","dynamicKind":"relative"}`)
	payload := json.RawMessage(`{"wait":{"amount":1,"unit":"hours"}}`)
	seedDelayRun(t, s, "run_requeue", "S1", cv, payload)

	dispatchParked(t, d.HandleDynamic, engine.TaskPayload{RunID: "run_requeue", StepID: "S1"})
	first, err := s.DelayDeadlines().Get(context.Background(), "run_requeue", "S1")
	if err != nil {
		t.Fatalf("Get after first dispatch: %v", err)
	}

	// Requeue: dispatch again. If the bug were present, this would resolve a
	// brand new "now + 1h" deadline roughly an hour after the first.
	dispatchParked(t, d.HandleDynamic, engine.TaskPayload{RunID: "run_requeue", StepID: "S1"})
	second, err := s.DelayDeadlines().Get(context.Background(), "run_requeue", "S1")
	if err != nil {
		t.Fatalf("Get after requeue: %v", err)
	}

	if !second.Equal(first) {
		t.Fatalf("requeue changed the deadline: first=%v second=%v", first, second)
	}
}

// TestHandleDynamic_Relative_RepeatedRequeuesDoNotExtendWait proves the
// acceptance criterion literally: many requeues in a row never push the
// persisted deadline later than the first resolution.
func TestHandleDynamic_Relative_RepeatedRequeuesDoNotExtendWait(t *testing.T) {
	s := newStore(t)
	d := NewDelayWait(s)

	cv := json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.wait","dynamicKind":"relative"}`)
	payload := json.RawMessage(`{"wait":{"amount":2,"unit":"hours"}}`)
	seedDelayRun(t, s, "run_repeat", "S1", cv, payload)

	var deadlines []time.Time
	for i := 0; i < 5; i++ {
		dispatchParked(t, d.HandleDynamic, engine.TaskPayload{RunID: "run_repeat", StepID: "S1"})
		got, err := s.DelayDeadlines().Get(context.Background(), "run_repeat", "S1")
		if err != nil {
			t.Fatalf("Get after dispatch %d: %v", i, err)
		}
		deadlines = append(deadlines, got)
	}
	for i := 1; i < len(deadlines); i++ {
		if !deadlines[i].Equal(deadlines[0]) {
			t.Fatalf("deadline drifted across requeues: dispatch 0 = %v, dispatch %d = %v", deadlines[0], i, deadlines[i])
		}
	}
}

// TestHandleDynamic_Relative_SurvivesPastStepOutputsRetentionWindow covers a
// delay longer than step_outputs' 7-day retention window, proving the
// deadline is not subject to that purge: the deadline row is backdated past 7
// days and a subsequent requeue still resumes toward the original deadline.
func TestHandleDynamic_Relative_SurvivesPastStepOutputsRetentionWindow(t *testing.T) {
	s := newStore(t)
	d := NewDelayWait(s)

	cv := json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.wait","dynamicKind":"relative"}`)
	payload := json.RawMessage(`{"wait":{"amount":30,"unit":"days"}}`)
	seedDelayRun(t, s, "run_long", "S1", cv, payload)

	dispatchParked(t, d.HandleDynamic, engine.TaskPayload{RunID: "run_long", StepID: "S1"})
	before, err := s.DelayDeadlines().Get(context.Background(), "run_long", "S1")
	if err != nil {
		t.Fatalf("Get after first dispatch: %v", err)
	}

	// Backdate the row's created_at to 8 days ago -- past step_outputs' 7-day
	// retention window -- directly against the writer pool.
	writer, _ := s.Pools()
	backdated := timestamp.Format(time.Now().Add(-8 * 24 * time.Hour))
	if _, err := writer.ExecContext(
		context.Background(),
		`UPDATE delay_deadlines SET created_at = ? WHERE run_id = ? AND step_id = ?`,
		backdated, "run_long", "S1",
	); err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}

	dispatchParked(t, d.HandleDynamic, engine.TaskPayload{RunID: "run_long", StepID: "S1"})
	after, err := s.DelayDeadlines().Get(context.Background(), "run_long", "S1")
	if err != nil {
		t.Fatalf("Get after requeue past 7 days: %v", err)
	}
	if !after.Equal(before) {
		t.Fatalf("deadline changed after surviving past the 7-day step_outputs window: before=%v after=%v", before, after)
	}
}

// TestHandleDynamic_Instant_WritesNoDeadlineRow proves the absolute
// dynamic-delay form is unaffected: it never reads or writes a deadline row,
// resolving straight from the payload's RFC3339 timestamp on every dispatch.
func TestHandleDynamic_Instant_WritesNoDeadlineRow(t *testing.T) {
	s := newStore(t)
	d := NewDelayWait(s)

	cv := json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.sendAt","dynamicKind":"instant"}`)
	payload := json.RawMessage(`{"sendAt":"2099-01-01T00:00:00Z"}`)
	seedDelayRun(t, s, "run_instant", "S1", cv, payload)

	dispatchParked(t, d.HandleDynamic, engine.TaskPayload{RunID: "run_instant", StepID: "S1"})

	if _, err := s.DelayDeadlines().Get(context.Background(), "run_instant", "S1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("instant dynamic delay wrote a deadline row: err = %v, want ErrNotFound", err)
	}
}

// TestHandleScheduled_WritesNoDeadlineRow proves the scheduled (calendar)
// delay form is unaffected: it recomputes its next occurrence from the
// calendar on every dispatch and never touches delay_deadlines.
func TestHandleScheduled_WritesNoDeadlineRow(t *testing.T) {
	s := newStore(t)
	d := NewDelayWait(s)

	cv := json.RawMessage(`{"type":"scheduled","atTime":"09:00"}`)
	seedDelayRun(t, s, "run_scheduled", "S1", cv, json.RawMessage(`{}`))

	dispatchParked(t, d.HandleScheduled, engine.TaskPayload{RunID: "run_scheduled", StepID: "S1"})

	if _, err := s.DelayDeadlines().Get(context.Background(), "run_scheduled", "S1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("scheduled delay wrote a deadline row: err = %v, want ErrNotFound", err)
	}
}

// TestHandleScheduled_ResumesAtTheParkedFireTime proves a scheduled delay
// keeps the fire time it parked for: resumed at it, it fires; resumed early, it
// parks toward the same time. Recomputing "the next occurrence after now" on
// resume would skip to the following occurrence and never fire.
func TestHandleScheduled_ResumesAtTheParkedFireTime(t *testing.T) {
	s := newStore(t)
	d := NewDelayWait(s)
	cv := json.RawMessage(`{"type":"scheduled","atTime":"09:00"}`)
	seedDelayRun(t, s, "run_sched_resume", "S1", cv, json.RawMessage(`{}`))

	fire := dispatchParked(t, d.HandleScheduled, engine.TaskPayload{RunID: "run_sched_resume", StepID: "S1"})
	if !fire.After(time.Now()) {
		t.Fatalf("parked until %v, want a future occurrence", fire)
	}
	early := dispatchParked(t, d.HandleScheduled, engine.TaskPayload{RunID: "run_sched_resume", StepID: "S1", ResumeAt: fire})
	if !early.Equal(fire) {
		t.Fatalf("early resume parked until %v, want the same %v", early, fire)
	}
	out, err := d.HandleScheduled(context.Background(), engine.TaskPayload{
		RunID: "run_sched_resume", StepID: "S1", ResumeAt: time.Now().Add(-time.Millisecond),
	})
	if err != nil || out != nil {
		t.Fatalf("resume at the fire time = (%s, %v), want it to fire with (nil, nil)", out, err)
	}
}

// TestHandleDynamic_Relative_ClearsDeadlineOnCompletion proves the row is
// cleared on normal completion so the common path does not accumulate rows.
// Uses a sub-second wait so the test actually completes the delay for real.
func TestHandleDynamic_Relative_ClearsDeadlineOnCompletion(t *testing.T) {
	s := newStore(t)
	d := NewDelayWait(s)

	cv := json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.wait","dynamicKind":"relative"}`)
	payload := json.RawMessage(`{"wait":{"amount":1,"unit":"seconds"}}`)
	seedDelayRun(t, s, "run_complete", "S1", cv, payload)

	ctx := context.Background()
	at := dispatchParked(t, d.HandleDynamic, engine.TaskPayload{RunID: "run_complete", StepID: "S1"})
	time.Sleep(time.Until(at))
	if _, err := d.HandleDynamic(ctx, engine.TaskPayload{RunID: "run_complete", StepID: "S1", ResumeAt: at}); err != nil {
		t.Fatalf("HandleDynamic (resumed): %v", err)
	}

	if _, err := s.DelayDeadlines().Get(ctx, "run_complete", "S1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deadline row not cleared on completion: err = %v, want ErrNotFound", err)
	}
}
