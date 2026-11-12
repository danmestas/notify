package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// pendingStatusLife is how long a status that arrived before its message
// waits for it.
const pendingStatusLife = time.Hour

// takePendingStatus moves a buffered final status for m's provider id onto m
// and removes it. One past pendingStatusLife is removed without being applied.
func takePendingStatus(ctx context.Context, tx *sql.Tx, scope domain.Scope, m *domain.Message) error {
	var status, class, text, at string
	err := tx.QueryRowContext(ctx,
		`SELECT status, error_class, error_text, received_at FROM sms_status_pending
		 WHERE org_id = ? AND environment_id = ? AND integration_id = ? AND provider_message_id = ?`,
		scope.OrgID, scope.EnvID, m.IntegrationID, m.ProviderMessageID).Scan(&status, &class, &text, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read pending status: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sms_status_pending
		 WHERE org_id = ? AND environment_id = ? AND integration_id = ? AND provider_message_id = ?`,
		scope.OrgID, scope.EnvID, m.IntegrationID, m.ProviderMessageID); err != nil {
		return fmt.Errorf("store: take pending status: %w", err)
	}
	if at < timestamp.Format(time.Now().Add(-pendingStatusLife)) {
		return nil
	}
	m.Status, m.ErrorClass, m.ErrorText = status, class, text
	return nil
}

// StatusCallbackResult is what a status callback did.
type StatusCallbackResult struct {
	// Message is the message the callback named, after the callback. Zero
	// when Buffered.
	Message domain.Message
	// Changed: the message moved from sent to the callback's status.
	Changed bool
	// Buffered: no message had the provider id yet, so the status waits for
	// one to be written.
	Buffered bool
}

func (repo *messageRepo) ApplyStatusCallback(ctx context.Context, scope domain.Scope, integrationID, providerMessageID, status, errorClass, errorText string) (StatusCallbackResult, error) {
	if status != "delivered" && status != "error" {
		return StatusCallbackResult{}, fmt.Errorf("store: status callback: %q is not a final status", status)
	}
	if providerMessageID == "" {
		return StatusCallbackResult{}, errors.New("store: status callback needs a provider message id")
	}
	tx, err := repo.w.BeginTx(ctx, nil)
	if err != nil {
		return StatusCallbackResult{}, fmt.Errorf("store: status callback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM messages
		 WHERE org_id = ? AND environment_id = ? AND integration_id = ? AND provider_message_id = ?
		 ORDER BY id LIMIT 1`,
		scope.OrgID, scope.EnvID, integrationID, providerMessageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		now := time.Now()
		if _, err := tx.ExecContext(ctx, `DELETE FROM sms_status_pending WHERE received_at < ?`,
			timestamp.Format(now.Add(-pendingStatusLife))); err != nil {
			return StatusCallbackResult{}, fmt.Errorf("store: purge pending statuses: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sms_status_pending
			   (org_id, environment_id, integration_id, provider_message_id, status, error_class, error_text, received_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(org_id, environment_id, integration_id, provider_message_id) DO NOTHING`,
			scope.OrgID, scope.EnvID, integrationID, providerMessageID, status, errorClass, errorText, timestamp.Format(now)); err != nil {
			return StatusCallbackResult{}, fmt.Errorf("store: buffer status: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return StatusCallbackResult{}, fmt.Errorf("store: buffer status: %w", err)
		}
		return StatusCallbackResult{Buffered: true}, nil
	}
	if err != nil {
		return StatusCallbackResult{}, fmt.Errorf("store: status callback: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE messages SET status = ?, error_class = ?, error_text = ?
		 WHERE org_id = ? AND environment_id = ? AND id = ? AND status = 'sent'`,
		status, errorClass, errorText, scope.OrgID, scope.EnvID, id)
	if err != nil {
		return StatusCallbackResult{}, fmt.Errorf("store: status callback: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return StatusCallbackResult{}, fmt.Errorf("store: status callback: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StatusCallbackResult{}, fmt.Errorf("store: status callback: %w", err)
	}
	m, err := repo.Get(ctx, scope, id)
	if err != nil {
		return StatusCallbackResult{}, err
	}
	return StatusCallbackResult{Message: m, Changed: n == 1}, nil
}
