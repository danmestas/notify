package authoring_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
)

// slowRegisterer stands in for a dagnats whose registrations each take a
// network round trip, recording how many were in flight at once.
type slowRegisterer struct {
	delay    time.Duration
	inFlight atomic.Int32
	peak     atomic.Int32
	mu       sync.Mutex
	names    map[string]bool
}

func (r *slowRegisterer) RegisterWorkflow(_ context.Context, def engine.WorkflowDef) error {
	n := r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(r.delay)
	r.mu.Lock()
	r.names[def.Name] = true
	r.mu.Unlock()
	return nil
}

// TestRepublishAll_RegistersConcurrently: on a multi-node dagnats each
// registration costs replication round trips (about 1.6 s on the mesh), so a
// serial boot of a few dozen workflows outlasts a deploy's health window.
// RepublishAll overlaps them, a bounded number at a time.
func TestRepublishAll_RegistersConcurrently(t *testing.T) {
	ctx := context.Background()
	st := newVersioningTestStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	const workflows = 20
	setup := authoring.New(st, &fakeRegisterer{})
	for i := range workflows {
		id := fmt.Sprintf("wf-%d", i)
		if _, err := setup.Create(ctx, scope, authoring.WorkflowInput{
			Name:              id,
			TriggerIdentifier: id,
			Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	reg := &slowRegisterer{delay: 50 * time.Millisecond, names: map[string]bool{}}
	n, err := authoring.New(st, reg).RepublishAll(ctx)
	if err != nil {
		t.Fatalf("RepublishAll: %v", err)
	}
	if n != workflows || len(reg.names) != workflows {
		t.Fatalf("registered %d (%d distinct), want %d", n, len(reg.names), workflows)
	}
	if p := reg.peak.Load(); p < 2 || p > 8 {
		t.Fatalf("peak in-flight registrations = %d, want between 2 and 8", p)
	}
}

// flakyRegisterer fails each workflow's first `failFirst` attempts with err
// (unless permanent names it), counting attempts per workflow.
type flakyRegisterer struct {
	err       error
	failFirst int
	permanent map[string]error
	mu        sync.Mutex
	attempts  map[string]int
	ok        map[string]bool
}

func newFlaky(err error, failFirst int) *flakyRegisterer {
	return &flakyRegisterer{
		err: err, failFirst: failFirst, permanent: map[string]error{},
		attempts: map[string]int{}, ok: map[string]bool{},
	}
}

func (r *flakyRegisterer) RegisterWorkflow(_ context.Context, def engine.WorkflowDef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts[def.Name]++
	for suffix, err := range r.permanent {
		if strings.HasSuffix(def.Name, suffix) {
			return err
		}
	}
	if r.attempts[def.Name] <= r.failFirst {
		return r.err
	}
	r.ok[def.Name] = true
	return nil
}

func seedWorkflows(t *testing.T, n int) (*store.Store, []string) {
	t.Helper()
	ctx := context.Background()
	st := newVersioningTestStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	setup := authoring.New(st, &fakeRegisterer{})
	var names []string
	for i := range n {
		id := fmt.Sprintf("wf-%d", i)
		if _, err := setup.Create(ctx, scope, authoring.WorkflowInput{
			Name: id, TriggerIdentifier: id,
			Steps: []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
		names = append(names, id)
	}
	return st, names
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestRepublishAll_RetriesTransient(t *testing.T) {
	defer authoring.SetRepublishBackoff(time.Millisecond)()
	st, _ := seedWorkflows(t, 5)
	reg := newFlaky(errors.New("engine: kv workflow_defs/x: read: context deadline exceeded"), 1)
	n, err := authoring.New(st, reg).RepublishAll(context.Background())
	if err != nil || n != 5 || len(reg.ok) != 5 {
		t.Fatalf("n=%d ok=%d err=%v, want all 5 registered", n, len(reg.ok), err)
	}
	for name, a := range reg.attempts {
		if a != 2 {
			t.Fatalf("%s attempts = %d, want 2", name, a)
		}
	}
}

func TestRepublishAll_GivesUpAfterAttemptsAndSummarises(t *testing.T) {
	defer authoring.SetRepublishBackoff(time.Millisecond)()
	logs := captureLog(t)
	st, _ := seedWorkflows(t, 2)
	reg := newFlaky(context.DeadlineExceeded, 1000)
	n, _ := authoring.New(st, reg).RepublishAll(context.Background())
	if n != 0 {
		t.Fatalf("registered %d, want 0", n)
	}
	for name, a := range reg.attempts {
		if a != 3 {
			t.Fatalf("%s attempts = %d, want 3", name, a)
		}
	}
	out := logs.String()
	if !strings.Contains(out, "failed_count=2") || !strings.Contains(out, "wf-0") || !strings.Contains(out, "wf-1") {
		t.Fatalf("summary line missing failures:\n%s", out)
	}
}

func TestRepublishAll_PermanentErrorNotRetried(t *testing.T) {
	defer authoring.SetRepublishBackoff(time.Millisecond)()
	logs := captureLog(t)
	st, _ := seedWorkflows(t, 3)
	reg := newFlaky(nil, 0)
	reg.permanent["wf-1"] = errors.New("engine: invalid workflow definition")
	n, _ := authoring.New(st, reg).RepublishAll(context.Background())
	if n != 2 || reg.attemptsFor("wf-1") != 1 {
		t.Fatalf("n=%d attempts(wf-1)=%d, want 2 registered and 1 attempt", n, reg.attemptsFor("wf-1"))
	}
	if out := logs.String(); !strings.Contains(out, "failed_count=1") || !strings.Contains(out, "wf-1") {
		t.Fatalf("summary line missing permanent failure:\n%s", out)
	}
}

func TestRepublishAll_CancelStopsRetrying(t *testing.T) {
	defer authoring.SetRepublishBackoff(time.Hour)()
	st, _ := seedWorkflows(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	reg := newFlaky(context.DeadlineExceeded, 1000)
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan struct{})
	go func() {
		_, _ = authoring.New(st, reg).RepublishAll(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RepublishAll did not stop after ctx cancel")
	}
}

func (r *flakyRegisterer) attemptsFor(suffix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, a := range r.attempts {
		if strings.HasSuffix(name, suffix) {
			return a
		}
	}
	return 0
}
