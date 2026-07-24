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
	// ListAll returns every workflow row across all orgs/envs, ordered by
	// rowid. It exists for boot-time re-registration (RepublishAll): the
	// engine loses its in-memory workflow registry on restart, and main.go
	// must re-register every persisted workflow without knowing the set of
	// scopes ahead of time. It is intentionally NOT scope-filtered.
	ListAll(ctx context.Context) ([]domain.Workflow, error)
	// Update replaces the mutable fields (name, steps, payload_schema, critical)
	// of the workflow identified by wf.ID within the scope. Returns ErrNotFound
	// when no matching row exists in this scope.
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
		   (id, org_id, environment_id, name, trigger_identifier, active, origin, steps, payload_schema, compiled_dag_version, critical)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		wf.ID, wf.OrgID, wf.EnvID, wf.Name, wf.TriggerIdentifier, wf.Active, wf.Origin,
		[]byte(wf.Steps), []byte(wf.PayloadSchema), wf.CompiledDAGVer, wf.Critical,
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
		`SELECT id, org_id, environment_id, name, trigger_identifier, active, origin, steps, payload_schema, compiled_dag_version, critical
		 FROM workflows WHERE org_id = ? AND environment_id = ? AND trigger_identifier = ?`,
		scope.OrgID, scope.EnvID, triggerID,
	).Scan(&wf.ID, &wf.OrgID, &wf.EnvID, &wf.Name, &wf.TriggerIdentifier, &wf.Active, &wf.Origin,
		&steps, &schema, &wf.CompiledDAGVer, &wf.Critical)
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
		`SELECT id, org_id, environment_id, name, trigger_identifier, active, origin, steps, payload_schema, compiled_dag_version, critical
		 FROM workflows WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, id,
	).Scan(&wf.ID, &wf.OrgID, &wf.EnvID, &wf.Name, &wf.TriggerIdentifier, &wf.Active, &wf.Origin,
		&steps, &schema, &wf.CompiledDAGVer, &wf.Critical)
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
		`SELECT id, org_id, environment_id, name, trigger_identifier, active, origin, steps, payload_schema, compiled_dag_version, critical
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
			&steps, &schema, &wf.CompiledDAGVer, &wf.Critical); err != nil {
			return nil, err
		}
		wf.Steps = steps
		wf.PayloadSchema = schema
		wfs = append(wfs, wf)
	}
	return wfs, rows.Err()
}

// ListAll returns every workflow across all scopes, ordered by rowid. Used by
// authoring.Service.RepublishAll at boot to re-register each persisted workflow
// with a freshly-restarted engine.
func (repo *workflowRepo) ListAll(ctx context.Context) ([]domain.Workflow, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, name, trigger_identifier, active, origin, steps, payload_schema, compiled_dag_version, critical
		 FROM workflows ORDER BY rowid`,
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
			&steps, &schema, &wf.CompiledDAGVer, &wf.Critical); err != nil {
			return nil, err
		}
		wf.Steps = steps
		wf.PayloadSchema = schema
		wfs = append(wfs, wf)
	}
	return wfs, rows.Err()
}

// Update replaces the mutable fields (name, steps, payload_schema, critical)
// for the workflow identified by wf.ID within scope. The trigger_identifier is
// intentionally not updateable (renaming an engine DAG key orphans run
// history). Returns ErrNotFound when no row matches.
func (repo *workflowRepo) Update(ctx context.Context, scope domain.Scope, wf domain.Workflow) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE workflows
		 SET name = ?, steps = ?, payload_schema = ?, critical = ?
		 WHERE org_id = ? AND environment_id = ? AND id = ?`,
		wf.Name, []byte(wf.Steps), []byte(wf.PayloadSchema), wf.Critical,
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
//
// Hard-delete is intentional, but NOT because the engine is cleaned up first --
// it is not. The engine exposes no deregister API (there is no KV delete on the
// NATS subject, and registering an empty-step DAG would corrupt the registry),
// so the compiled DAG deliberately outlives this row. That is safe: reaching a
// DAG requires resolving the workflow row first, so an orphaned registration is
// unreachable, and in-flight runs hold their own snapshot inside the engine and
// complete normally.
//
// Notification history references workflow_id as a plain string (no FK), so
// those rows survive and remain queryable for audit purposes.
//
// See authoring.Service.Delete for the full rationale.
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
