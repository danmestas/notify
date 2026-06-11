package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// WorkflowRepo persists workflows; the unique key is
// (environment_id, trigger_identifier). Scope-first throughout.
type WorkflowRepo interface {
	Create(ctx context.Context, scope domain.Scope, wf domain.Workflow) (domain.Workflow, error)
	GetByTriggerIdentifier(ctx context.Context, scope domain.Scope, triggerID string) (domain.Workflow, error)
	GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Workflow, error)
	List(ctx context.Context, scope domain.Scope) ([]domain.Workflow, error)
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
