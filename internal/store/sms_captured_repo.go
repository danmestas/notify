package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// CapturedSMSKeep is how many captured texts an integration keeps; older ones
// are pruned as new ones arrive.
const CapturedSMSKeep = 1000

// CapturedSMS is a text a capture integration accepted instead of sending.
type CapturedSMS struct {
	ID                string
	ProviderMessageID string
	To                string
	From              string
	Body              string
	CreatedAt         string
}

// SMSCapturedRepo keeps the texts capture integrations accepted. Scope-first.
type SMSCapturedRepo interface {
	// Add records a text for integrationID, prunes the integration to the
	// newest CapturedSMSKeep, and returns the synthetic provider message id.
	Add(ctx context.Context, scope domain.Scope, integrationID, to, from, body string) (providerMessageID string, err error)
	// List returns up to limit texts of integrationID, newest first.
	List(ctx context.Context, scope domain.Scope, integrationID string, limit int) ([]CapturedSMS, error)
}

type smsCapturedRepo struct {
	w *sql.DB
	r *sql.DB
}

// SMSCaptured vends the captured SMS repository.
func (s *Store) SMSCaptured() SMSCapturedRepo {
	return &smsCapturedRepo{w: s.writer, r: s.reader}
}

func (repo *smsCapturedRepo) Add(ctx context.Context, scope domain.Scope, integrationID, to, from, body string) (string, error) {
	id := newULID()
	pmid := "capture-" + id
	tx, err := repo.w.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("store: capture sms: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sms_captured
		   (id, org_id, environment_id, integration_id, provider_message_id, to_phone, from_phone, body, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, scope.OrgID, scope.EnvID, integrationID, pmid, to, from, body, timestamp.Format(time.Now())); err != nil {
		return "", fmt.Errorf("store: capture sms: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sms_captured WHERE org_id = ? AND environment_id = ? AND integration_id = ? AND seq NOT IN
		   (SELECT seq FROM sms_captured WHERE org_id = ? AND environment_id = ? AND integration_id = ?
		    ORDER BY seq DESC LIMIT ?)`,
		scope.OrgID, scope.EnvID, integrationID, scope.OrgID, scope.EnvID, integrationID, CapturedSMSKeep); err != nil {
		return "", fmt.Errorf("store: prune captured sms: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: capture sms: %w", err)
	}
	return pmid, nil
}

func (repo *smsCapturedRepo) List(ctx context.Context, scope domain.Scope, integrationID string, limit int) ([]CapturedSMS, error) {
	rows, err := repo.r.QueryContext(ctx,
		`SELECT id, provider_message_id, to_phone, from_phone, body, created_at FROM sms_captured
		 WHERE org_id = ? AND environment_id = ? AND integration_id = ?
		 ORDER BY seq DESC LIMIT ?`,
		scope.OrgID, scope.EnvID, integrationID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list captured sms: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CapturedSMS
	for rows.Next() {
		var c CapturedSMS
		if err := rows.Scan(&c.ID, &c.ProviderMessageID, &c.To, &c.From, &c.Body, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list captured sms: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
