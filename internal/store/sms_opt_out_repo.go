package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// SMSOptOutRepo records the numbers that texted STOP in an environment, and
// when each number last got a HELP reply. An opted-out number gets no sms step
// until it texts START. Scope-first.
type SMSOptOutRepo interface {
	// OptOut records phone (E.164) as opted out through integrationID.
	// changed is false when it already was.
	OptOut(ctx context.Context, scope domain.Scope, phone, integrationID string) (changed bool, err error)
	// OptIn clears phone's opt-out. changed is false when there was none.
	OptIn(ctx context.Context, scope domain.Scope, phone string) (changed bool, err error)
	// IsOptedOut reports whether phone is opted out.
	IsOptedOut(ctx context.Context, scope domain.Scope, phone string) (bool, error)
	// ClaimHelpReply reports whether phone may get a HELP reply through
	// integrationID now, at most one per window, and records the reply when
	// it may. Concurrent claims for one number grant one.
	ClaimHelpReply(ctx context.Context, scope domain.Scope, integrationID, phone string, window time.Duration) (bool, error)
}

type smsOptOutRepo struct {
	w *sql.DB
	r *sql.DB
}

// SMSOptOuts vends the SMS opt-out repository.
func (s *Store) SMSOptOuts() SMSOptOutRepo {
	return &smsOptOutRepo{w: s.writer, r: s.reader}
}

// execer is what a statement needs: a *sql.DB or a *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// OptChange is the opt-out change an inbound keyword makes.
type OptChange int

const (
	// OptNone: the text is not an opt keyword.
	OptNone OptChange = iota
	// OptOut: STOP and its synonyms.
	OptOut
	// OptIn: START and its synonyms.
	OptIn
)

// applyOptChange makes change for phone through ex, reporting whether it
// changed anything.
func applyOptChange(ctx context.Context, ex execer, scope domain.Scope, change OptChange, phone, integrationID string) (bool, error) {
	var (
		res sql.Result
		err error
	)
	switch change {
	case OptOut:
		res, err = ex.ExecContext(ctx,
			`INSERT INTO sms_opt_outs (org_id, environment_id, phone, integration_id, created_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(org_id, environment_id, phone) DO NOTHING`,
			scope.OrgID, scope.EnvID, phone, integrationID, timestamp.Now())
	case OptIn:
		res, err = ex.ExecContext(ctx,
			`DELETE FROM sms_opt_outs WHERE org_id = ? AND environment_id = ? AND phone = ?`,
			scope.OrgID, scope.EnvID, phone)
	default:
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: sms opt change: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: sms opt change: %w", err)
	}
	return n == 1, nil
}

func (repo *smsOptOutRepo) OptOut(ctx context.Context, scope domain.Scope, phone, integrationID string) (bool, error) {
	return applyOptChange(ctx, repo.w, scope, OptOut, phone, integrationID)
}

func (repo *smsOptOutRepo) OptIn(ctx context.Context, scope domain.Scope, phone string) (bool, error) {
	return applyOptChange(ctx, repo.w, scope, OptIn, phone, "")
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

func (repo *smsOptOutRepo) ClaimHelpReply(ctx context.Context, scope domain.Scope, integrationID, phone string, window time.Duration) (bool, error) {
	now := time.Now()
	res, err := repo.w.ExecContext(ctx,
		`INSERT INTO sms_help_replies (org_id, environment_id, integration_id, phone, sent_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(org_id, environment_id, integration_id, phone)
		 DO UPDATE SET sent_at = excluded.sent_at WHERE sms_help_replies.sent_at < ?`,
		scope.OrgID, scope.EnvID, integrationID, phone, timestamp.Format(now), timestamp.Format(now.Add(-window)))
	if err != nil {
		return false, fmt.Errorf("store: claim help reply: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim help reply: %w", err)
	}
	return n == 1, nil
}
