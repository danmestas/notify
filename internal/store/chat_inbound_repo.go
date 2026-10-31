package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// ChatInboundRepo records which inbound chat messages a poller has already
// claimed for dispatch, so each message produces at most one agent turn.
type ChatInboundRepo interface {
	// MarkHandled claims (provider, messageID) within scope. It reports true
	// when this call created the claim and false when it already existed.
	MarkHandled(ctx context.Context, scope domain.Scope, provider, messageID string) (bool, error)
	// PurgeOlderThan deletes claims made before cutoff and reports how many
	// were removed. Unscoped by design: retention is system-wide.
	PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

type chatInboundRepo struct {
	w *sql.DB
}

func (repo *chatInboundRepo) MarkHandled(ctx context.Context, scope domain.Scope, provider, messageID string) (bool, error) {
	res, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO chat_inbound_handled (org_id, environment_id, provider, message_id, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (org_id, environment_id, provider, message_id) DO NOTHING`,
		scope.OrgID, scope.EnvID, provider, messageID, timestamp.Now(),
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PurgeOlderThan compares created_at as a string, which is correct because it
// is written through the fixed-width timestamp layout (see internal/timestamp).
func (repo *chatInboundRepo) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM chat_inbound_handled WHERE created_at < ?`,
		timestamp.Format(cutoff),
	)
	if err != nil {
		return 0, fmt.Errorf("store: purge chat_inbound_handled: %w", err)
	}
	return res.RowsAffected()
}
