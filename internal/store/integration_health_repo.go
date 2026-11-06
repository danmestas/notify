package store

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"

	"github.com/danmestas/notify/internal/domain"
)

// MessageObserver is told about every message the store writes, after the
// write succeeds. The delivery health tracker is one: a sent or failed
// message is a delivery result for the integration that sent it, and every
// channel's delivery path ends in Messages().Create, so observing the store
// sees every result without each handler having to report it.
//
// MessageCreated runs on the writer's goroutine, so it must be quick and must
// never fail the write: it returns nothing.
type MessageObserver interface {
	MessageCreated(ctx context.Context, scope domain.Scope, m domain.Message)
}

// SetMessageObserver installs o (nil removes it). Set it while wiring, before
// anything writes messages; it is safe to call concurrently with writes.
func (s *Store) SetMessageObserver(o MessageObserver) { s.msgObs.store(o) }

// messageObserverSlot holds the observer shared by every messageRepo the
// store hands out.
type messageObserverSlot struct {
	v atomic.Pointer[MessageObserver]
}

func (s *messageObserverSlot) store(o MessageObserver) {
	if o == nil {
		s.v.Store(nil)
		return
	}
	s.v.Store(&o)
}

func (s *messageObserverSlot) load() MessageObserver {
	if p := s.v.Load(); p != nil {
		return *p
	}
	return nil
}

// IntegrationHealthRepo persists per-integration delivery health and answers
// the window queries the tracker evaluates it from.
type IntegrationHealthRepo interface {
	// Get returns the integration's health row, or ErrNotFound when it has
	// none yet (no result or check has landed for it).
	Get(ctx context.Context, scope domain.Scope, integrationID string) (domain.IntegrationHealth, error)
	// Put inserts or replaces the integration's health row.
	Put(ctx context.Context, scope domain.Scope, h domain.IntegrationHealth) error
	// List returns every health row in scope, ordered by integration id.
	List(ctx context.Context, scope domain.Scope) ([]domain.IntegrationHealth, error)
	// WindowCounts counts the integration's delivery results created at or
	// after since (a messages created_at value) and with a rowid above floor:
	// sent messages, and failed messages whose class is not "recipient".
	// Served by the msg_integration index.
	WindowCounts(ctx context.Context, scope domain.Scope, integrationID, since string, floor int64) (sent, failed int, err error)
	// MessageSeq returns a message's rowid, the order the store wrote it in.
	MessageSeq(ctx context.Context, scope domain.Scope, messageID string) (int64, error)
	// LatestMessageSeq returns the highest messages rowid (0 when empty).
	LatestMessageSeq(ctx context.Context) (int64, error)
}

// IntegrationHealth returns the integration health repository.
func (s *Store) IntegrationHealth() IntegrationHealthRepo {
	return &integrationHealthRepo{w: s.writer, r: s.reader}
}

type integrationHealthRepo struct {
	w *sql.DB
	r *sql.DB
}

const integrationHealthColumns = `integration_id, org_id, environment_id, state, since, window_floor,
	last_error_class, last_error_message, last_error_at, last_check_at, last_check_ok, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanIntegrationHealth(row rowScanner) (domain.IntegrationHealth, error) {
	var h domain.IntegrationHealth
	err := row.Scan(&h.IntegrationID, &h.OrgID, &h.EnvID, &h.State, &h.Since, &h.WindowFloor,
		&h.LastErrorClass, &h.LastErrorMessage, &h.LastErrorAt, &h.LastCheckAt, &h.LastCheckOK, &h.UpdatedAt)
	return h, err
}

func (repo *integrationHealthRepo) Get(ctx context.Context, scope domain.Scope, integrationID string) (domain.IntegrationHealth, error) {
	row := repo.r.QueryRowContext(ctx,
		`SELECT `+integrationHealthColumns+` FROM integration_health
		 WHERE org_id = ? AND environment_id = ? AND integration_id = ?`,
		scope.OrgID, scope.EnvID, integrationID)
	h, err := scanIntegrationHealth(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.IntegrationHealth{}, ErrNotFound
	}
	return h, err
}

func (repo *integrationHealthRepo) Put(ctx context.Context, scope domain.Scope, h domain.IntegrationHealth) error {
	h.OrgID, h.EnvID = scope.OrgID, scope.EnvID
	_, err := repo.w.ExecContext(ctx,
		`INSERT INTO integration_health (`+integrationHealthColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (integration_id) DO UPDATE SET
		   state = excluded.state, since = excluded.since, window_floor = excluded.window_floor,
		   last_error_class = excluded.last_error_class, last_error_message = excluded.last_error_message,
		   last_error_at = excluded.last_error_at, last_check_at = excluded.last_check_at,
		   last_check_ok = excluded.last_check_ok, updated_at = excluded.updated_at
		 WHERE integration_health.org_id = excluded.org_id
		   AND integration_health.environment_id = excluded.environment_id`,
		h.IntegrationID, h.OrgID, h.EnvID, h.State, h.Since, h.WindowFloor,
		h.LastErrorClass, h.LastErrorMessage, h.LastErrorAt, h.LastCheckAt, h.LastCheckOK, h.UpdatedAt)
	return err
}

func (repo *integrationHealthRepo) List(ctx context.Context, scope domain.Scope) ([]domain.IntegrationHealth, error) {
	rows, err := repo.r.QueryContext(ctx,
		`SELECT `+integrationHealthColumns+` FROM integration_health
		 WHERE org_id = ? AND environment_id = ? ORDER BY integration_id`,
		scope.OrgID, scope.EnvID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.IntegrationHealth
	for rows.Next() {
		h, err := scanIntegrationHealth(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (repo *integrationHealthRepo) WindowCounts(ctx context.Context, scope domain.Scope, integrationID, since string, floor int64) (sent, failed int, err error) {
	err = repo.r.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN status = 'sent' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status = 'error' AND error_class != 'recipient' THEN 1 ELSE 0 END), 0)
		 FROM messages
		 WHERE org_id = ? AND environment_id = ? AND integration_id = ?
		   AND created_at >= ? AND rowid > ? AND status IN ('sent', 'error')`,
		scope.OrgID, scope.EnvID, integrationID, since, floor).Scan(&sent, &failed)
	return sent, failed, err
}

func (repo *integrationHealthRepo) MessageSeq(ctx context.Context, scope domain.Scope, messageID string) (int64, error) {
	var seq int64
	err := repo.r.QueryRowContext(ctx,
		`SELECT rowid FROM messages WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, messageID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return seq, err
}

func (repo *integrationHealthRepo) LatestMessageSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := repo.r.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) FROM messages`).Scan(&seq)
	return seq, err
}
