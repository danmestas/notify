package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/danmestas/notify/internal/domain"
)

// WorkflowRepo persists workflows; the unique key is
// (environment_id, trigger_identifier). Scope-first throughout.
type WorkflowRepo interface {
	Create(ctx context.Context, scope domain.Scope, wf domain.Workflow) (domain.Workflow, error)
	GetByTriggerIdentifier(ctx context.Context, scope domain.Scope, triggerID string) (domain.Workflow, error)
	GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Workflow, error)
	List(ctx context.Context, scope domain.Scope) ([]domain.Workflow, error)
	// Update replaces the mutable fields (name, steps, payload_schema) of the
	// workflow identified by wf.ID within the scope. Returns ErrNotFound when
	// no matching row exists in this scope.
	Update(ctx context.Context, scope domain.Scope, wf domain.Workflow) error
	// Delete removes the workflow row identified by triggerID within the scope.
	// Returns ErrNotFound when no matching row exists.
	Delete(ctx context.Context, scope domain.Scope, triggerID string) error
	// SetActive updates only the active column for the workflow identified by
	// triggerID within the scope. Returns ErrNotFound when no matching row
	// exists.
	SetActive(ctx context.Context, scope domain.Scope, triggerID string, active bool) error
}

type workflowRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *workflowRepo) Create(ctx context.Context, scope domain.Scope, wf domain.Workflow) (domain.Workflow, error) {
	wf.OrgID = scope.OrgID
	wf.EnvID = scope.EnvID
	if wf.ID == "" {
		wf.ID = newULID()
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO workflows
		   (id, org_id, environment_id, name, trigger_identifier, active, origin, steps, payload_schema, compiled_dag_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		wf.ID, wf.OrgID, wf.EnvID, wf.Name, wf.TriggerIdentifier, wf.Active, wf.Origin,
		[]byte(wf.Steps), []byte(wf.PayloadSchema), wf.CompiledDAGVer,
	)
	if err != nil {
		// A duplicate (environment_id, trigger_identifier) trips the UNIQUE
		// index; surface it as ErrConflict so callers can answer 409 rather
		// than leaking a raw driver error as a 500 (mirrors layout_repo).
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return domain.Workflow{}, ErrConflict
		}
		return domain.Workflow{}, err
	}
	return wf, nil
}

func (repo *workflowRepo) GetByTriggerIdentifier(ctx context.Context, scope domain.Scope, triggerID string) (domain.Workflow, error) {
	var (
		wf            domain.Workflow
		steps, schema []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, name, trigger_identifier, active, origin, steps, payload_schema, compiled_dag_version
		 FROM workflows WHERE org_id = ? AND environment_id = ? AND trigger_identifier = ?`,
		scope.OrgID, scope.EnvID, triggerID,
	).Scan(&wf.ID, &wf.OrgID, &wf.EnvID, &wf.Name, &wf.TriggerIdentifier, &wf.Active, &wf.Origin,
		&steps, &schema, &wf.CompiledDAGVer)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Workflow{}, ErrNotFound
	}
	if err != nil {
		return domain.Workflow{}, err
	}
	wf.Steps = steps
	wf.PayloadSchema = schema
	return wf, nil
}

func (repo *workflowRepo) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Workflow, error) {
	var (
		wf            domain.Workflow
		steps, schema []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, name, trigger_identifier, active, origin, steps, payload_schema, compiled_dag_version
		 FROM workflows WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, id,
	).Scan(&wf.ID, &wf.OrgID, &wf.EnvID, &wf.Name, &wf.TriggerIdentifier, &wf.Active, &wf.Origin,
		&steps, &schema, &wf.CompiledDAGVer)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Workflow{}, ErrNotFound
	}
	if err != nil {
		return domain.Workflow{}, err
	}
	wf.Steps = steps
	wf.PayloadSchema = schema
	return wf, nil
}

func (repo *workflowRepo) List(ctx context.Context, scope domain.Scope) ([]domain.Workflow, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, name, trigger_identifier, active, origin, steps, payload_schema, compiled_dag_version
		 FROM workflows WHERE org_id = ? AND environment_id = ? ORDER BY rowid`,
		scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var wfs []domain.Workflow
	for rows.Next() {
		var (
			wf            domain.Workflow
			steps, schema []byte
		)
		if err := rows.Scan(&wf.ID, &wf.OrgID, &wf.EnvID, &wf.Name, &wf.TriggerIdentifier, &wf.Active, &wf.Origin,
			&steps, &schema, &wf.CompiledDAGVer); err != nil {
			return nil, err
		}
		wf.Steps = steps
		wf.PayloadSchema = schema
		wfs = append(wfs, wf)
	}
	return wfs, rows.Err()
}

// Update replaces the mutable fields (name, steps, payload_schema) for the
// workflow identified by wf.ID within scope. The trigger_identifier is
// intentionally not updateable (renaming an engine DAG key orphans run
// history). Returns ErrNotFound when no row matches.
func (repo *workflowRepo) Update(ctx context.Context, scope domain.Scope, wf domain.Workflow) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE workflows
		 SET name = ?, steps = ?, payload_schema = ?
		 WHERE org_id = ? AND environment_id = ? AND id = ?`,
		wf.Name, []byte(wf.Steps), []byte(wf.PayloadSchema),
		scope.OrgID, scope.EnvID, wf.ID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes the workflow row identified by triggerID within scope.
// Hard-delete is intentional: the engine DAG is also deregistered by the
// authoring layer before this runs, so no orphaned engine state remains.
// Notification history references the workflow_id string (not a FK), so those
// rows survive and remain queryable for audit purposes.
// Returns ErrNotFound when no matching row exists.
func (repo *workflowRepo) Delete(ctx context.Context, scope domain.Scope, triggerID string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM workflows WHERE org_id = ? AND environment_id = ? AND trigger_identifier = ?`,
		scope.OrgID, scope.EnvID, triggerID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetActive flips the active flag for the workflow identified by triggerID
// within scope. Returns ErrNotFound when no matching row exists.
func (repo *workflowRepo) SetActive(ctx context.Context, scope domain.Scope, triggerID string, active bool) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE workflows SET active = ? WHERE org_id = ? AND environment_id = ? AND trigger_identifier = ?`,
		active, scope.OrgID, scope.EnvID, triggerID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
