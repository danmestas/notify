package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/dagnats/program"
	"github.com/danmestas/dagnats/worker"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/names"
)

func programConfig(t *testing.T) Config {
	t.Helper()
	return Config{DBPath: filepath.Join(t.TempDir(), "notify.db"), MasterKey: make([]byte, 32)}
}

func runProgram(t *testing.T, prog program.Program, opts program.Options) *program.Running {
	t.Helper()
	running, err := program.Run(context.Background(), prog, opts)
	if err != nil {
		t.Fatalf("program.Run: %v", err)
	}
	return running
}

func stopProgram(t *testing.T, running *program.Running) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := running.Stop(ctx); err != nil {
		t.Fatalf("Running.Stop: %v", err)
	}
	select {
	case err := <-running.Done():
		if err != nil {
			t.Fatalf("Done after Stop = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Done did not fire after Stop")
	}
}

// TestProgram_RunOnOpenAuth runs notify the way cmd/notify does, against an
// in-process dagnats with no client login: registration succeeds, a task
// dispatched to a notify.send.* type is claimed by the worker program.Attach
// runs (the engine runs none in program mode), /readyz is ok, Stop drains,
// and a restart with the same seed gets the same Grant.
func TestProgram_RunOnOpenAuth(t *testing.T) {
	opts := program.Options{URL: dagtest.StartSidecar(t), SeedPath: filepath.Join(t.TempDir(), "notify.seed")}
	cfg := programConfig(t)
	p := &Program{Config: cfg}
	running := runProgram(t, p, opts)

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /readyz = %d %s, want 200", rec.Code, rec.Body)
	}
	for _, s := range Manifest().Streams {
		if p.grant.StreamMaxBytes[s.Name] <= 0 {
			t.Errorf("grant has no size for stream %s: %+v", s.Name, p.grant)
		}
	}

	// The handler fails the step (no such run in the store), which is enough:
	// only a claimed task reaches a terminal state.
	runOneStep(t, p.notify().eng, names.SendInApp)

	first := p.grant
	stopProgram(t, running)

	again := &Program{Config: cfg}
	stopProgram(t, runProgram(t, again, opts))
	if !reflect.DeepEqual(again.grant, first) {
		t.Fatalf("restart with the same seed got grant %+v, want %+v", again.grant, first)
	}
}

// runOneStep runs a one-step workflow of taskType and waits for it to reach
// a terminal state, which only a claimed task does.
func runOneStep(t *testing.T, eng *engine.Engine, taskType string) engine.RunSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wf := names.Prefix + "program-test-" + strings.ReplaceAll(names.Legacy(taskType), ".", "-")
	if err := eng.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  wf,
		Steps: []engine.StepDef{{ID: "step", Task: taskType}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	runID, err := eng.StartRun(ctx, wf, []byte(`{}`))
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	for {
		snap, err := eng.GetRun(ctx, runID)
		if err == nil && snap.IsTerminal() {
			return snap
		}
		if ctx.Err() != nil {
			t.Fatalf("run %s never finished: the %s task was not handled (last %+v, %v)",
				runID, taskType, snap, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// upgraded is notify as a later release would register it: another version
// and one more task type, which it handles.
type upgraded struct{ *Program }

const upgradedTaskType = names.Prefix + "test.upgraded"

func (u upgraded) Manifest() program.Manifest {
	m := u.Program.Manifest()
	m.Version = "upgraded"
	m.TaskTypes = append(m.TaskTypes, upgradedTaskType)
	return m
}

func (u upgraded) Start(ctx context.Context, h program.Host) error {
	h.Handle(upgradedTaskType, func(tc worker.TaskContext) error {
		return tc.Complete(json.RawMessage(`"upgraded"`))
	})
	return u.Program.Start(ctx, h)
}

// TestProgram_UpgradeReregisters: a release with a changed manifest (a new
// version and an added task type) re-registers under the same seed, and the
// added task type is handled.
func TestProgram_UpgradeReregisters(t *testing.T) {
	opts := program.Options{URL: dagtest.StartSidecar(t), SeedPath: filepath.Join(t.TempDir(), "notify.seed")}
	cfg := programConfig(t)
	stopProgram(t, runProgram(t, &Program{Config: cfg}, opts))

	next := upgraded{&Program{Config: cfg}}
	running := runProgram(t, next, opts)
	defer stopProgram(t, running)
	if snap := runOneStep(t, next.notify().eng, upgradedTaskType); !snap.Succeeded() {
		t.Fatalf("run of the added task type = %+v, want completed", snap)
	}
}

// otherKey is a program registered as "notify" under another key.
type otherKey struct{}

func (otherKey) Manifest() program.Manifest                { return Manifest() }
func (otherKey) Start(context.Context, program.Host) error { return nil }
func (otherKey) Stop(context.Context) error                { return nil }

// TestProgram_RefusalBeforeStart: a refused registration reaches the caller as
// an *ErrRefused, and notify built nothing (its database was never opened).
func TestProgram_RefusalBeforeStart(t *testing.T) {
	url := dagtest.StartSidecar(t)
	stopProgram(t, runProgram(t, otherKey{},
		program.Options{URL: url, SeedPath: filepath.Join(t.TempDir(), "other.seed")}))

	cfg := programConfig(t)
	opts := program.Options{URL: url, SeedPath: filepath.Join(t.TempDir(), "notify.seed")}
	running, err := program.Run(context.Background(), &Program{Config: cfg}, opts)
	if err == nil {
		stopProgram(t, running)
	}
	var refused *program.ErrRefused
	if !errors.As(err, &refused) || refused.Code != program.RefusedNameTaken {
		t.Fatalf("program.Run = %v, want *ErrRefused %s", err, program.RefusedNameTaken)
	}
	if _, err := os.Stat(cfg.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused notify opened its database (stat err %v)", err)
	}
}

// TestProgram_HandlerBeforeStart answers 503 rather than panicking when the
// host serves Handler before Start.
func TestProgram_HandlerBeforeStart(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Program{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz before Start = %d, want 503", rec.Code)
	}
}
