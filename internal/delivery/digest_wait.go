package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
)

// DigestWait is the engine handler for a scheduled (cron) digest window. Unlike
// a regular digest — which the engine times itself via a static "sleep" StepDef
// — a scheduled window's wait duration depends on wall-clock time, so it cannot
// be baked into the DAG. The digest.wait task instead resolves the step's cron
// expression at dispatch time and sleeps until the next tick; the downstream
// channel step then self-sources and drains the accumulated events.
type DigestWait struct {
	store *store.Store
}

// NewDigestWait constructs a DigestWait backed by s.
func NewDigestWait(s *store.Store) *DigestWait { return &DigestWait{store: s} }

// HandleDigestWait is registered via engine.Handle("notify.digest.wait", ...). It
// loads the scheduled-digest step's cron expression (by run id + step id),
// computes the next cron tick, and parks the task until it (waitUntil). A
// parked digest holds no worker slot, survives a restart, and resumes at the
// tick it parked for.
func (d *DigestWait) HandleDigestWait(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	// A resumed digest fires at the tick it parked for. Recomputing
	// "the next tick after now" at that tick would wait a whole period more.
	if !t.ResumeAt.IsZero() {
		return waitUntil(t.ResumeAt)
	}
	cronExpr, err := stepCron(ctx, d.store, t.RunID, t.StepID)
	if err != nil {
		return nil, err
	}
	schedule, err := steptype.ParseCron(cronExpr)
	if err != nil {
		return nil, fmt.Errorf("digest.wait: invalid cron %q: %w", cronExpr, err)
	}
	return waitUntil(schedule.Next(time.Now()))
}
