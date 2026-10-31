package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// ChatPollCursorRepo persists each chat poller's "already seen" cursor,
// scoped to (org_id, environment_id) and keyed by provider, so a restart
// resumes where the poller left off instead of replaying recent messages.
type ChatPollCursorRepo interface {
	// Get returns the saved cursor, or "" when none exists.
	Get(ctx context.Context, scope domain.Scope, provider string) (string, error)
	// Set stores the cursor, replacing any previous one.
	Set(ctx context.Context, scope domain.Scope, provider, cursor string) error
}

type chatPollCursorRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *chatPollCursorRepo) Get(ctx context.Context, scope domain.Scope, provider string) (string, error) {
	var cursor string
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT cursor FROM chat_poll_cursors
		 WHERE org_id = ? AND environment_id = ? AND provider = ?`,
		scope.OrgID, scope.EnvID, provider,
	).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cursor, err
}

func (repo *chatPollCursorRepo) Set(ctx context.Context, scope domain.Scope, provider, cursor string) error {
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO chat_poll_cursors (org_id, environment_id, provider, cursor, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (org_id, environment_id, provider)
		 DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
		scope.OrgID, scope.EnvID, provider, cursor, timestamp.Now(),
	)
	return err
}
