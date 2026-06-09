package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// PreferenceRepo persists subscriber channel preferences. Every method is
// Scope-first; the unique key is (environment_id, subscriber_id, type,
// workflow_id). Two preference types exist: "subscriberGlobal" (workflow_id
// empty) and "workflow" (per-workflow override).
//
// TODO(parity): the reference platform resolves preferences across four levels — workflow-critical
// override > subscriber per-workflow > subscriber global > workflow default.
// This store implements the two subscriber levels; workflow-default and
// workflow-critical resolution live above the store.
type PreferenceRepo interface {
	// Upsert inserts the preference, minting an id when empty, or updates the
	// channels of an existing row on conflict of the unique key.
	Upsert(ctx context.Context, scope domain.Scope, p domain.Preference) (domain.Preference, error)
	// GetGlobal returns the subscriberGlobal preference (workflow_id='') for the
	// subscriber, or ErrNotFound if none exists.
	GetGlobal(ctx context.Context, scope domain.Scope, subscriberID string) (domain.Preference, error)
	// GetForWorkflow returns the workflow preference for (subscriber, workflow),
	// or ErrNotFound if none exists.
	GetForWorkflow(ctx context.Context, scope domain.Scope, subscriberID, workflowID string) (domain.Preference, error)
}

type preferenceRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *preferenceRepo) Upsert(ctx context.Context, scope domain.Scope, p domain.Preference) (domain.Preference, error) {
	p.OrgID = scope.OrgID
	p.EnvID = scope.EnvID
	if p.ID == "" {
		p.ID = newULID()
	}
	if len(p.Channels) == 0 {
		p.Channels = []byte("{}")
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO preferences
		   (id, org_id, environment_id, type, subscriber_id, workflow_id, channels)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (environment_id, subscriber_id, type, workflow_id)
		 DO UPDATE SET channels = excluded.channels`,
		p.ID, p.OrgID, p.EnvID, p.Type, p.SubscriberID, p.WorkflowID, []byte(p.Channels),
	)
	if err != nil {
		return domain.Preference{}, err
	}
	return p, nil
}

func (repo *preferenceRepo) GetGlobal(ctx context.Context, scope domain.Scope, subscriberID string) (domain.Preference, error) {
	return repo.get(ctx, scope, "subscriberGlobal", subscriberID, "")
}

func (repo *preferenceRepo) GetForWorkflow(ctx context.Context, scope domain.Scope, subscriberID, workflowID string) (domain.Preference, error) {
	return repo.get(ctx, scope, "workflow", subscriberID, workflowID)
}

func (repo *preferenceRepo) get(ctx context.Context, scope domain.Scope, typ, subscriberID, workflowID string) (domain.Preference, error) {
	var (
		p  domain.Preference
		ch []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, type, subscriber_id, workflow_id, channels
		 FROM preferences
		 WHERE org_id = ? AND environment_id = ? AND type = ?
		   AND subscriber_id = ? AND workflow_id = ?`,
		scope.OrgID, scope.EnvID, typ, subscriberID, workflowID,
	).Scan(&p.ID, &p.OrgID, &p.EnvID, &p.Type, &p.SubscriberID, &p.WorkflowID, &ch)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Preference{}, ErrNotFound
	}
	if err != nil {
		return domain.Preference{}, err
	}
	p.Channels = ch
	return p, nil
}
