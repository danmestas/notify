package store

import (
	"context"
	"database/sql"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// WebhookDelivery is one pending POST of one event to one endpoint (#346).
type WebhookDelivery struct {
	ID            string
	OrgID         string
	EnvID         string
	EndpointID    string
	Payload       []byte
	Attempts      int
	NextAttemptAt string
	CreatedAt     string
}

// Scope returns the tenancy the delivery belongs to.
func (d WebhookDelivery) Scope() domain.Scope {
	return domain.Scope{OrgID: d.OrgID, EnvID: d.EnvID}
}

// WebhookDeliveryRepo is the outbound webhook outbox. Rows are written by the
// stream consumer and drained by a worker; a row lives until the endpoint
// accepts the event or the worker gives up on it.
type WebhookDeliveryRepo interface {
	// Enqueue inserts rows, assigning ids and created_at. NextAttemptAt must
	// be set by the caller.
	Enqueue(ctx context.Context, rows []WebhookDelivery) error
	// Due returns up to limit rows whose next attempt is at or before now,
	// oldest attempt first.
	Due(ctx context.Context, now string, limit int) ([]WebhookDelivery, error)
	// Claim pushes a due row's next attempt to until, so one worker owns the
	// attempt. ok is false when the row was not due (another worker has it,
	// or it is gone).
	Claim(ctx context.Context, id, now, until string) (ok bool, err error)
	// Reschedule records a failed attempt and when to try again.
	Reschedule(ctx context.Context, id string, attempts int, nextAttemptAt string) error
	// Delete removes a row. A missing row is not an error.
	Delete(ctx context.Context, id string) error
}

type webhookDeliveryRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *webhookDeliveryRepo) Enqueue(ctx context.Context, rows []WebhookDelivery) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := repo.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := timestamp.Now()
	for _, d := range rows {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO webhook_deliveries (id, org_id, environment_id, endpoint_id, payload, attempts, next_attempt_at, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			newULID(), d.OrgID, d.EnvID, d.EndpointID, d.Payload, d.Attempts, d.NextAttemptAt, now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (repo *webhookDeliveryRepo) Due(ctx context.Context, now string, limit int) ([]WebhookDelivery, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := repo.r.QueryContext(ctx,
		`SELECT id, org_id, environment_id, endpoint_id, payload, attempts, next_attempt_at, created_at
		 FROM webhook_deliveries WHERE next_attempt_at <= ? ORDER BY next_attempt_at ASC LIMIT ?`,
		now, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []WebhookDelivery
	for rows.Next() {
		var d WebhookDelivery
		if err := rows.Scan(&d.ID, &d.OrgID, &d.EnvID, &d.EndpointID, &d.Payload, &d.Attempts, &d.NextAttemptAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (repo *webhookDeliveryRepo) Claim(ctx context.Context, id, now, until string) (bool, error) {
	res, err := repo.w.ExecContext(ctx,
		`UPDATE webhook_deliveries SET next_attempt_at = ? WHERE id = ? AND next_attempt_at <= ?`,
		until, id, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (repo *webhookDeliveryRepo) Reschedule(ctx context.Context, id string, attempts int, nextAttemptAt string) error {
	_, err := repo.w.ExecContext(ctx,
		`UPDATE webhook_deliveries SET attempts = ?, next_attempt_at = ? WHERE id = ?`,
		attempts, nextAttemptAt, id)
	return err
}

func (repo *webhookDeliveryRepo) Delete(ctx context.Context, id string) error {
	_, err := repo.w.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE id = ?`, id)
	return err
}
