package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// WorkflowVersion is one append-only snapshot of a workflow's compiled DAG,
// captured every time authoring.Publish (or Rollback, which is itself a
// publish) registers a def with the engine. CompiledDef holds the exact JSON
// bytes handed to engine.RegisterWorkflow — not the authoring input — so a
// rollback re-registers byte-for-byte.
type WorkflowVersion struct {
	ID                string
	WorkflowID        string
	Seq               int
	Name              string
	TriggerIdentifier string
	CompiledDef       json.RawMessage
	CreatedAt         string
}

// WorkflowVersionRepo persists the per-publish compiled-DAG snapshot stack
// that authoring.Service.Publish and .Rollback append to. workflow_versions
// is the audit trail a bad publish rolls back against instead of a rebuild.
type WorkflowVersionRepo interface {
	// Create appends a new version row for workflowID: Seq is
	// COALESCE(MAX(seq),0)+1 computed atomically inside the INSERT (mirrors
	// BuilderSessionRepo.PushCheckpoint) so two concurrent publishes on the
	// same workflow cannot allocate the same seq — the idx_workflow_versions_wf
	// UNIQUE index (migration 0027) is the backstop that fails a genuine race
	// loudly instead of corrupting the stack's ordering. It also repoints
	// workflows.compiled_dag_version at the new version's id: that column was
	// a scaffolded-and-abandoned marker (0001_init.sql) never set to a
	// non-empty value; Create retires it by making it the live pointer to the
	// latest published version instead of a second, independent marker.
	Create(ctx context.Context, scope domain.Scope, workflowID, name, triggerIdentifier string, compiledDef json.RawMessage) (WorkflowVersion, error)
	// Get loads a single version row by id, scope-checked via a
	// workflow_id -> workflows join so a cross-tenant version id returns
	// ErrNotFound rather than leaking another tenant's compiled DAG.
	Get(ctx context.Context, scope domain.Scope, versionID string) (WorkflowVersion, error)
	// ListByWorkflow returns every version for workflowID within scope,
	// ordered by seq ascending (oldest first).
	ListByWorkflow(ctx context.Context, scope domain.Scope, workflowID string) ([]WorkflowVersion, error)
	// LatestByWorkflow returns the highest-seq (newest) version for workflowID,
	// or ErrNotFound when the workflow has no versions. It is intentionally NOT
	// scope-filtered: it exists for boot-time re-registration (authoring.RepublishAll),
	// which spans every org/env via WorkflowRepo.ListAll and re-registers each
	// workflow's last-published compiled DAG byte-for-byte. The workflow_id is
	// already a tenant-owned ULID resolved from that scoped list, so a bare
	// lookup here mirrors ListAll's deliberate non-scoping.
	LatestByWorkflow(ctx context.Context, workflowID string) (WorkflowVersion, error)
}

type workflowVersionRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *workflowVersionRepo) Create(ctx context.Context, scope domain.Scope, workflowID, name, triggerIdentifier string, compiledDef json.RawMessage) (WorkflowVersion, error) {
	wv := WorkflowVersion{
		ID:                newULID(),
		WorkflowID:        workflowID,
		Name:              name,
		TriggerIdentifier: triggerIdentifier,
		CompiledDef:       compiledDef,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	// The seq allocation, its read-back, and the compiled_dag_version repoint
	// run in one transaction so the version row and the workflow's
	// latest-version pointer can never diverge under a crash or a concurrent
	// publish.
	tx, err := repo.w.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowVersion{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Allocate seq and insert in ONE statement: the MAX(seq)+1 is computed
	// inside the INSERT...SELECT, so two concurrent Creates on the same
	// workflow cannot both read the same stale MAX between a separate SELECT
	// and INSERT. The UNIQUE (workflow_id, seq) index is the hard backstop.
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO workflow_versions
		   (id, workflow_id, seq, name, trigger_identifier, compiled_def, created_at)
		 SELECT ?, ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ?, ?
		   FROM workflow_versions WHERE workflow_id = ?`,
		wv.ID, wv.WorkflowID, wv.Name, wv.TriggerIdentifier, string(wv.CompiledDef), wv.CreatedAt, wv.WorkflowID,
	); err != nil {
		return WorkflowVersion{}, err
	}
	// Read back the allocated seq for the returned value (mirrors
	// PushCheckpoint's follow-up read).
	if err := tx.QueryRowContext(
		ctx,
		`SELECT seq FROM workflow_versions WHERE id = ?`,
		wv.ID,
	).Scan(&wv.Seq); err != nil {
		return WorkflowVersion{}, err
	}
	// Repoint the retired compiled_dag_version marker at this new version's
	// id, scope-guarded so it can only ever update a workflow row this Scope
	// owns.
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE workflows SET compiled_dag_version = ? WHERE id = ? AND org_id = ? AND environment_id = ?`,
		wv.ID, workflowID, scope.OrgID, scope.EnvID,
	); err != nil {
		return WorkflowVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowVersion{}, err
	}
	return wv, nil
}

func (repo *workflowVersionRepo) LatestByWorkflow(ctx context.Context, workflowID string) (WorkflowVersion, error) {
	var (
		wv          WorkflowVersion
		compiledDef []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, workflow_id, seq, name, trigger_identifier, compiled_def, created_at
		 FROM workflow_versions
		 WHERE workflow_id = ?
		 ORDER BY seq DESC
		 LIMIT 1`,
		workflowID,
	).Scan(&wv.ID, &wv.WorkflowID, &wv.Seq, &wv.Name, &wv.TriggerIdentifier, &compiledDef, &wv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowVersion{}, ErrNotFound
	}
	if err != nil {
		return WorkflowVersion{}, err
	}
	wv.CompiledDef = compiledDef
	return wv, nil
}

func (repo *workflowVersionRepo) Get(ctx context.Context, scope domain.Scope, versionID string) (WorkflowVersion, error) {
	var (
		wv          WorkflowVersion
		compiledDef []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT wv.id, wv.workflow_id, wv.seq, wv.name, wv.trigger_identifier, wv.compiled_def, wv.created_at
		 FROM workflow_versions wv
		 JOIN workflows w ON w.id = wv.workflow_id
		 WHERE wv.id = ? AND w.org_id = ? AND w.environment_id = ?`,
		versionID, scope.OrgID, scope.EnvID,
	).Scan(&wv.ID, &wv.WorkflowID, &wv.Seq, &wv.Name, &wv.TriggerIdentifier, &compiledDef, &wv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowVersion{}, ErrNotFound
	}
	if err != nil {
		return WorkflowVersion{}, err
	}
	wv.CompiledDef = compiledDef
	return wv, nil
}

func (repo *workflowVersionRepo) ListByWorkflow(ctx context.Context, scope domain.Scope, workflowID string) ([]WorkflowVersion, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT wv.id, wv.workflow_id, wv.seq, wv.name, wv.trigger_identifier, wv.compiled_def, wv.created_at
		 FROM workflow_versions wv
		 JOIN workflows w ON w.id = wv.workflow_id
		 WHERE wv.workflow_id = ? AND w.org_id = ? AND w.environment_id = ?
		 ORDER BY wv.seq ASC`,
		workflowID, scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var versions []WorkflowVersion
	for rows.Next() {
		var (
			wv          WorkflowVersion
			compiledDef []byte
		)
		if err := rows.Scan(&wv.ID, &wv.WorkflowID, &wv.Seq, &wv.Name, &wv.TriggerIdentifier, &compiledDef, &wv.CreatedAt); err != nil {
			return nil, err
		}
		wv.CompiledDef = compiledDef
		versions = append(versions, wv)
	}
	return versions, rows.Err()
}
