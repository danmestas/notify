package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// StepOutputRepo persists per-step action outputs (currently http_request
// responses) keyed by (run_id, step_id). It is the minimum-viable persistence
// that makes an action step's result readable by downstream channel steps as
// both template data ({{steps.<stepId>...}}) and filter-condition sources.
type StepOutputRepo interface {
	// Save upserts the output blob for (runID, stepID) within scope. A repeat
	// dispatch of the same step (engine retry) overwrites the prior row rather
	// than failing the primary-key constraint.
	Save(ctx context.Context, scope domain.Scope, runID, stepID string, output json.RawMessage) error
	// ListByRun returns every step output for a run, keyed by step id with each
	// output JSON-parsed into a flat map. Unscoped by design: a worker handling
	// a downstream task has only the opaque run id (no tenancy ctx), mirroring
	// notifications.GetByRunID. A row whose output is not a JSON object is
	// skipped rather than failing the whole read.
	ListByRun(ctx context.Context, runID string) (map[string]map[string]any, error)
}

type stepOutputRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *stepOutputRepo) Save(ctx context.Context, scope domain.Scope, runID, stepID string, output json.RawMessage) error {
	if len(output) == 0 {
		output = json.RawMessage(`{}`)
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO step_outputs (org_id, environment_id, run_id, step_id, output, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run_id, step_id) DO UPDATE SET output = excluded.output`,
		scope.OrgID, scope.EnvID, runID, stepID, string(output),
		timestamp.Now(),
	)
	return err
}

func (repo *stepOutputRepo) ListByRun(ctx context.Context, runID string) (map[string]map[string]any, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT step_id, output FROM step_outputs WHERE run_id = ?`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]map[string]any{}
	for rows.Next() {
		var stepID, output string
		if err := rows.Scan(&stepID, &output); err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(output), &m); err != nil || m == nil {
			// A non-object output is unusable as a named source; skip it rather
			// than failing the read for the other steps.
			continue
		}
		out[stepID] = m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
