package store

import (
	"context"
	"database/sql"
	"fmt"
)

// queryer is what the repositories need from a connection: satisfied by a
// *sql.DB pool and by the single *sql.Conn a write transaction pins.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx vends the tenancy repositories bound to one open write transaction.
type Tx struct{ conn *sql.Conn }

func (t *Tx) Organizations() OrganizationRepo { return &orgRepo{w: t.conn, r: t.conn} }
func (t *Tx) Environments() EnvironmentRepo   { return &envRepo{w: t.conn, r: t.conn} }
func (t *Tx) ApiKeys() ApiKeyRepo             { return &apiKeyRepo{w: t.conn, r: t.conn} }

// WithImmediateTx runs fn inside BEGIN IMMEDIATE ... COMMIT on the writer
// connection. IMMEDIATE takes SQLite's write lock before fn's first read, so
// a find-then-create inside fn is atomic against every other writer, including
// other processes sharing the database file (they wait out the busy timeout).
// fn's error rolls the transaction back and is returned unchanged.
func (s *Store) WithImmediateTx(ctx context.Context, fn func(*Tx) error) error {
	conn, err := s.writer.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: writer conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: begin immediate: %w", err)
	}
	if err := fn(&Tx{conn: conn}); err != nil {
		// Roll back even when ctx is already cancelled, or the lock leaks
		// with the pooled connection.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
