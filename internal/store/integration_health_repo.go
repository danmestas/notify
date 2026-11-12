package store

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"

	"github.com/danmestas/notify/internal/domain"
)

// DeliveryObserver hears every delivery outcome the store learns of. The
// delivery health tracker is one.
//
// MessageCreated is told about every message the store writes, after the
// write succeeds: a sent or failed message is a delivery result for the
// integration that sent it, and every channel's delivery path ends in
// Messages().Create, so observing the store sees every result without each
// handler having to report it.
//
// AttemptFailed is told about a failed attempt that wrote no message: a
// transient provider failure the engine will retry (Store.ReportFailedAttempt).
//
// Both run on the caller's goroutine, so they must be quick, and return
// nothing: an observer can never fail a delivery.
type DeliveryObserver interface {
	MessageCreated(ctx context.Context, scope domain.Scope, m domain.Message)
	AttemptFailed(ctx context.Context, scope domain.Scope, integrationID, errorClass, errorText string)
}

// SetDeliveryObserver installs o (nil removes it). Set it while wiring, before
// anything delivers; it is safe to call concurrently with writes.
func (s *Store) SetDeliveryObserver(o DeliveryObserver) { s.msgObs.store(o) }

// ReportFailedAttempt tells the observer that a delivery attempt through
// integrationID failed without writing a message, with the failure's class
// and text. With no observer, or no integration, it does nothing.
func (s *Store) ReportFailedAttempt(ctx context.Context, scope domain.Scope, integrationID, errorClass, errorText string) {
	if integrationID == "" {
		return
	}
	if o := s.msgObs.load(); o != nil {
		o.AttemptFailed(ctx, scope, integrationID, errorClass, errorText)
	}
}

// messageObserverSlot holds the observer shared by every messageRepo the
// store hands out.
type messageObserverSlot struct {
	v        atomic.Pointer[DeliveryObserver]
	buffered atomic.Pointer[BufferedStatusHook]
}

// BufferedStatusHook is told about a message written with the final status
// that a provider callback delivered before the message existed: m carries
// that status. It runs on the caller's goroutine, after the write commits.
type BufferedStatusHook func(ctx context.Context, scope domain.Scope, m domain.Message)

// SetBufferedStatusHook installs fn (nil removes it), like SetDeliveryObserver.
func (s *Store) SetBufferedStatusHook(fn BufferedStatusHook) {
	if fn == nil {
		s.msgObs.buffered.Store(nil)
		return
	}
	s.msgObs.buffered.Store(&fn)
}

func (s *messageObserverSlot) store(o DeliveryObserver) {
	if o == nil {
		s.v.Store(nil)
		return
	}
	s.v.Store(&o)
}

func (s *messageObserverSlot) load() DeliveryObserver {
	if p := s.v.Load(); p != nil {
		return *p
	}
	return nil
}

// IntegrationHealthRepo persists per-integration delivery health and the
// delivery results its window is counted from.
type IntegrationHealthRepo interface {
	// Get returns the integration's health row, or ErrNotFound when it has
	// none yet (no result or check has landed for it).
	Get(ctx context.Context, scope domain.Scope, integrationID string) (domain.IntegrationHealth, error)
	// Put inserts or replaces the integration's health row.
	Put(ctx context.Context, scope domain.Scope, h domain.IntegrationHealth) error
	// List returns every health row in scope, ordered by integration id.
	List(ctx context.Context, scope domain.Scope) ([]domain.IntegrationHealth, error)
	// RecordResult appends one delivery outcome for the integration (ok, or
	// failed with errorClass) at createdAt, deletes the integration's results
	// created before pruneBefore, and returns the new result's seq.
	RecordResult(ctx context.Context, scope domain.Scope, integrationID string, ok bool, errorClass, createdAt, pruneBefore string) (int64, error)
	// WindowCounts counts the integration's results created at or after
	// since and with a seq above floor: successes and failures.
	WindowCounts(ctx context.Context, scope domain.Scope, integrationID, since string, floor int64) (sent, failed int, err error)
	// LatestResultSeq returns the highest result seq (0 when none).
	LatestResultSeq(ctx context.Context) (int64, error)
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
	last_error_class, last_error_message, last_error_at, last_check_at, last_check_ok, last_check_detail, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanIntegrationHealth(row rowScanner) (domain.IntegrationHealth, error) {
	var h domain.IntegrationHealth
	err := row.Scan(&h.IntegrationID, &h.OrgID, &h.EnvID, &h.State, &h.Since, &h.WindowFloor,
		&h.LastErrorClass, &h.LastErrorMessage, &h.LastErrorAt, &h.LastCheckAt, &h.LastCheckOK, &h.LastCheckDetail, &h.UpdatedAt)
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
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (integration_id) DO UPDATE SET
		   state = excluded.state, since = excluded.since, window_floor = excluded.window_floor,
		   last_error_class = excluded.last_error_class, last_error_message = excluded.last_error_message,
		   last_error_at = excluded.last_error_at, last_check_at = excluded.last_check_at,
		   last_check_ok = excluded.last_check_ok, last_check_detail = excluded.last_check_detail,
		   updated_at = excluded.updated_at
		 WHERE integration_health.org_id = excluded.org_id
		   AND integration_health.environment_id = excluded.environment_id`,
		h.IntegrationID, h.OrgID, h.EnvID, h.State, h.Since, h.WindowFloor,
		h.LastErrorClass, h.LastErrorMessage, h.LastErrorAt, h.LastCheckAt, h.LastCheckOK, h.LastCheckDetail, h.UpdatedAt)
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

func (repo *integrationHealthRepo) RecordResult(ctx context.Context, scope domain.Scope, integrationID string, ok bool, errorClass, createdAt, pruneBefore string) (int64, error) {
	res, err := repo.w.ExecContext(ctx,
		`INSERT INTO delivery_results (integration_id, org_id, environment_id, ok, error_class, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		integrationID, scope.OrgID, scope.EnvID, ok, errorClass, createdAt)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := repo.w.ExecContext(ctx,
		`DELETE FROM delivery_results WHERE integration_id = ? AND created_at < ?`,
		integrationID, pruneBefore); err != nil {
		return 0, err
	}
	return seq, nil
}

func (repo *integrationHealthRepo) WindowCounts(ctx context.Context, scope domain.Scope, integrationID, since string, floor int64) (sent, failed int, err error) {
	err = repo.r.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(ok), 0), COALESCE(SUM(1 - ok), 0)
		 FROM delivery_results
		 WHERE org_id = ? AND environment_id = ? AND integration_id = ?
		   AND created_at >= ? AND seq > ?`,
		scope.OrgID, scope.EnvID, integrationID, since, floor).Scan(&sent, &failed)
	return sent, failed, err
}

func (repo *integrationHealthRepo) LatestResultSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := repo.r.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM delivery_results`).Scan(&seq)
	return seq, err
}

// ActiveIntegrationScopes lists every active integration in every
// organization and environment, as (scope, integration id) pairs and without
// credentials. It is the one deliberately cross-tenant read: the daily
// credential check (#100) must visit every tenant, and each probe then runs
// under its own scope.
func (s *Store) ActiveIntegrationScopes(ctx context.Context) ([]ScopedIntegration, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT org_id, environment_id, id FROM integrations WHERE active = 1
		 ORDER BY org_id, environment_id, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ScopedIntegration
	for rows.Next() {
		var si ScopedIntegration
		if err := rows.Scan(&si.Scope.OrgID, &si.Scope.EnvID, &si.IntegrationID); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// ScopedIntegration names one integration and the tenant it belongs to.
type ScopedIntegration struct {
	Scope         domain.Scope
	IntegrationID string
}
