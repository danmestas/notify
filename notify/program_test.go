package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/danmestas/dagnats/program"

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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng := p.notify().eng
	wf := names.Prefix + "program-test"
	if err := eng.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  wf,
		Steps: []engine.StepDef{{ID: "send", Task: names.SendInApp}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	runID, err := eng.StartRun(ctx, wf, []byte(`{}`))
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// The handler fails the step (no such run in the store), which is enough:
	// only a claimed task reaches a terminal state.
	for {
		snap, err := eng.GetRun(ctx, runID)
		if err == nil && snap.IsTerminal() {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("run %s never finished: the %s task was not handled (last %+v, %v)",
				runID, names.SendInApp, snap, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	first := p.grant
	stopProgram(t, running)

	again := &Program{Config: cfg}
	stopProgram(t, runProgram(t, again, opts))
	if !reflect.DeepEqual(again.grant, first) {
		t.Fatalf("restart with the same seed got grant %+v, want %+v", again.grant, first)
	}
}

// otherManifest is a program registered as "notify" with a manifest that is
// not notify's, so registering the real one with its key is refused.
type otherManifest struct{}

func (otherManifest) Manifest() program.Manifest {
	m := Manifest()
	m.Version = "other"
	return m
}
func (otherManifest) Start(context.Context, program.Host) error { return nil }
func (otherManifest) Stop(context.Context) error                { return nil }

// TestProgram_RefusalBeforeStart: a refused registration reaches the caller as
// an *ErrRefused, and notify built nothing (its database was never opened).
func TestProgram_RefusalBeforeStart(t *testing.T) {
	opts := program.Options{URL: dagtest.StartSidecar(t), SeedPath: filepath.Join(t.TempDir(), "notify.seed")}
	stopProgram(t, runProgram(t, otherManifest{}, opts))

	cfg := programConfig(t)
	_, err := program.Run(context.Background(), &Program{Config: cfg}, opts)
	var refused *program.ErrRefused
	if !errors.As(err, &refused) || refused.Code != program.RefusedManifestChanged {
		t.Fatalf("program.Run = %v, want *ErrRefused %s", err, program.RefusedManifestChanged)
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
