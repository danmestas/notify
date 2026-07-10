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
	// Steps, PayloadSchema, and Critical are the full editable-row snapshot
	// added by migration 0028: the exact workflows.steps/payload_schema/
	// critical values at the moment this version was published (Steps is the
	// marshalled JSON of a domain.Step[]). Together with CompiledDef they let
	// a version stand in for the whole workflow row, not just its compiled
	// DAG — authoring.Service.Rollback restores workflows.steps/
	// payload_schema/critical from these fields before re-registering
	// CompiledDef, so delivery (which reads step CONTENT from workflows.steps
	// at send time, internal/delivery/rebuild.go) and the engine (which runs
	// the registered TOPOLOGY) agree again post-rollback.
	//
	// Steps == "" identifies a version captured before migration 0028 (the
	// column defaults empty on ADD COLUMN and no code path wrote it before
	// this change existed); Rollback treats that as "no snapshot available"
	// and falls back to its pre-0028 compiled_def-only behavior rather than
	// overwriting a live row with blank steps.
	Steps         string
	PayloadSchema string
	Critical      bool
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
	//
	// steps/payloadSchema/critical (migration 0028) are the editable-row
	// snapshot: pass the workflow's current steps/payload_schema/critical (as
	// published) so a later Rollback can restore the whole row, not just the
	// compiled DAG. Pass steps="" only when there is no snapshot to record
	// (the caller has no basis for one) — Rollback reads that back as "this
	// version predates full-row snapshots."
	//
	// Retention: Create also caps the version stack at maxVersionsPerWorkflow
	// (50) rows per workflow, pruning the oldest rows beyond that in the same
	// transaction. The latest version is always retained, so LatestByWorkflow
	// (RepublishAll's boot-time source of truth) is unaffected; Rollback is
	// bounded to targeting a version still within that retained window.
	Create(ctx context.Context, scope domain.Scope, workflowID, name, triggerIdentifier string, compiledDef json.RawMessage, steps, payloadSchema string, critical bool) (WorkflowVersion, error)
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

// maxVersionsPerWorkflow caps how many workflow_versions rows Create retains
// per workflow. 50 is plenty of rollback history — far more than an operator
// will realistically page back through — while keeping the table's growth
// bounded instead of the prior unbounded-leak behavior (migration 0027's
// original "grows unbounded" note). The newest row is always kept, so
// LatestByWorkflow (RepublishAll's boot-time source of truth) is never
// affected by pruning.
const maxVersionsPerWorkflow = 50

type workflowVersionRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *workflowVersionRepo) Create(ctx context.Context, scope domain.Scope, workflowID, name, triggerIdentifier string, compiledDef json.RawMessage, steps, payloadSchema string, critical bool) (WorkflowVersion, error) {
	wv := WorkflowVersion{
		ID:                newULID(),
		WorkflowID:        workflowID,
		Name:              name,
		TriggerIdentifier: triggerIdentifier,
		CompiledDef:       compiledDef,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		Steps:             steps,
		PayloadSchema:     payloadSchema,
		Critical:          critical,
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
		   (id, workflow_id, seq, name, trigger_identifier, compiled_def, created_at, steps, payload_schema, critical)
		 SELECT ?, ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ?, ?, ?, ?, ?
		   FROM workflow_versions WHERE workflow_id = ?`,
		wv.ID, wv.WorkflowID, wv.Name, wv.TriggerIdentifier, string(wv.CompiledDef), wv.CreatedAt,
		wv.Steps, wv.PayloadSchema, wv.Critical, wv.WorkflowID,
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
	// Retention cap: prune this workflow's older rows down to the newest
	// maxVersionsPerWorkflow, in the same transaction as the insert above so
	// the version stack is never observed mid-prune. seq is monotonic and
	// gapless within a workflow (Create always allocates MAX(seq)+1, and
	// pruning only ever removes a contiguous prefix), so MAX(seq)-cap is the
	// exact cutoff: rows at or below it are deleted, leaving exactly
	// min(total, cap) rows — never cap-1 or cap+1. When the workflow has at
	// most cap rows, MAX(seq)-cap goes non-positive and the DELETE matches
	// nothing (seq is always >= 1), so an under-cap workflow is untouched.
	// This only ever prunes workflowID's own rows (scoped by the WHERE and by
	// the subquery), never another workflow's, and it never touches the row
	// just inserted above (the newest row's seq is always > MAX(seq)-cap).
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM workflow_versions
		   WHERE workflow_id = ?
		     AND seq <= (SELECT MAX(seq) FROM workflow_versions WHERE workflow_id = ?) - ?`,
		workflowID, workflowID, maxVersionsPerWorkflow,
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
		`SELECT id, workflow_id, seq, name, trigger_identifier, compiled_def, created_at, steps, payload_schema, critical
		 FROM workflow_versions
		 WHERE workflow_id = ?
		 ORDER BY seq DESC
		 LIMIT 1`,
		workflowID,
	).Scan(&wv.ID, &wv.WorkflowID, &wv.Seq, &wv.Name, &wv.TriggerIdentifier, &compiledDef, &wv.CreatedAt,
		&wv.Steps, &wv.PayloadSchema, &wv.Critical)
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
		`SELECT wv.id, wv.workflow_id, wv.seq, wv.name, wv.trigger_identifier, wv.compiled_def, wv.created_at, wv.steps, wv.payload_schema, wv.critical
		 FROM workflow_versions wv
		 JOIN workflows w ON w.id = wv.workflow_id
		 WHERE wv.id = ? AND w.org_id = ? AND w.environment_id = ?`,
		versionID, scope.OrgID, scope.EnvID,
	).Scan(&wv.ID, &wv.WorkflowID, &wv.Seq, &wv.Name, &wv.TriggerIdentifier, &compiledDef, &wv.CreatedAt,
		&wv.Steps, &wv.PayloadSchema, &wv.Critical)
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
		`SELECT wv.id, wv.workflow_id, wv.seq, wv.name, wv.trigger_identifier, wv.compiled_def, wv.created_at, wv.steps, wv.payload_schema, wv.critical
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
		if err := rows.Scan(&wv.ID, &wv.WorkflowID, &wv.Seq, &wv.Name, &wv.TriggerIdentifier, &compiledDef, &wv.CreatedAt,
			&wv.Steps, &wv.PayloadSchema, &wv.Critical); err != nil {
			return nil, err
		}
		wv.CompiledDef = compiledDef
		versions = append(versions, wv)
	}
	return versions, rows.Err()
}
