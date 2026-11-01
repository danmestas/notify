package engine

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/dagnats/worker"
)

// recorder stands in for program.Host.Handle.
type recorder struct {
	types []string
	opts  map[string]int
}

func (r *recorder) handle(taskType string, fn worker.HandlerFunc, opts ...worker.HandlerOption) {
	if fn == nil {
		panic("recorder: nil handler")
	}
	r.types = append(r.types, taskType)
	r.opts[taskType] = len(opts)
}

// TestWithRegistrar_RegistersThroughItAndRunsNoWorker pins program mode: every
// handler, simple and agent-loop, goes to the registrar with its worker
// options, and the engine builds no worker of its own.
func TestWithRegistrar_RegistersThroughItAndRunsNoWorker(t *testing.T) {
	rec := &recorder{opts: map[string]int{}}
	e := FromConn(nil, WithRegistrar(rec.handle))
	noop := func(context.Context, TaskPayload) (json.RawMessage, error) { return nil, nil }
	e.Handle("notify.a", noop)
	e.Handle("notify.b", noop, WithMaxRunTime(time.Hour))
	e.HandleAgentLoop("notify.loop", func(context.Context, *AgentLoopContext) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	if err := e.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}
	slices.Sort(rec.types)
	if want := []string{"notify.a", "notify.b", "notify.loop"}; !slices.Equal(rec.types, want) {
		t.Fatalf("registered %v, want %v", rec.types, want)
	}
	// Concurrency and ack wait for every type, plus max run time for notify.b.
	if rec.opts["notify.a"] != 2 || rec.opts["notify.b"] != 3 {
		t.Errorf("worker options = %v, want notify.a:2 notify.b:3", rec.opts)
	}
	if e.w != nil {
		t.Fatal("engine built a worker of its own in registrar mode")
	}

	// Cancelling Start's ctx ends handler contexts, as Stop does, so a waiting
	// handler returns before the host's worker drains.
	cancel()
	select {
	case <-e.life.Done():
	case <-time.After(time.Second):
		t.Fatal("handler lifetime did not end with Start's ctx")
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
