package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// SMSOptOutRepo records the numbers that texted STOP in an environment. An
// opted-out number gets no sms step until it texts START. Scope-first.
type SMSOptOutRepo interface {
	// OptOut records phone (E.164) as opted out through integrationID. A
	// repeat is harmless.
	OptOut(ctx context.Context, scope domain.Scope, phone, integrationID string) error
	// OptIn clears phone's opt-out, if any.
	OptIn(ctx context.Context, scope domain.Scope, phone string) error
	// IsOptedOut reports whether phone is opted out.
	IsOptedOut(ctx context.Context, scope domain.Scope, phone string) (bool, error)
}

type smsOptOutRepo struct {
	w *sql.DB
	r *sql.DB
}

// SMSOptOuts vends the SMS opt-out repository.
func (s *Store) SMSOptOuts() SMSOptOutRepo {
	return &smsOptOutRepo{w: s.writer, r: s.reader}
}

func (repo *smsOptOutRepo) OptOut(ctx context.Context, scope domain.Scope, phone, integrationID string) error {
	_, err := repo.w.ExecContext(ctx,
		`INSERT INTO sms_opt_outs (org_id, environment_id, phone, integration_id, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(org_id, environment_id, phone) DO NOTHING`,
		scope.OrgID, scope.EnvID, phone, integrationID, timestamp.Now())
	if err != nil {
		return fmt.Errorf("store: sms opt-out: %w", err)
	}
	return nil
}

func (repo *smsOptOutRepo) OptIn(ctx context.Context, scope domain.Scope, phone string) error {
	_, err := repo.w.ExecContext(ctx,
		`DELETE FROM sms_opt_outs WHERE org_id = ? AND environment_id = ? AND phone = ?`,
		scope.OrgID, scope.EnvID, phone)
	if err != nil {
		return fmt.Errorf("store: sms opt-in: %w", err)
	}
	return nil
}

func (repo *smsOptOutRepo) IsOptedOut(ctx context.Context, scope domain.Scope, phone string) (bool, error) {
	var n int
	err := repo.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sms_opt_outs WHERE org_id = ? AND environment_id = ? AND phone = ?`,
		scope.OrgID, scope.EnvID, phone).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: sms opt-out lookup: %w", err)
	}
	return n > 0, nil
}
