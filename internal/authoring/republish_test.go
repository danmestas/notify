package authoring_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
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
