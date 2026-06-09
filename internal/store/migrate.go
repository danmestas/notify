package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies every embedded migrations/NNNN_*.sql not yet recorded in
// schema_migrations, in lexical order, each inside its own transaction. The
// schema_migrations table is created by 0001_init.sql, so the recorded-set
// query tolerates its absence on a fresh database. Idempotent.
func Migrate(ctx context.Context, writer *sql.DB) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	applied, err := appliedMigrations(ctx, writer)
	if err != nil {
		return err
	}

	for _, name := range names {
		if applied[name] {
			continue
		}
		if err := applyMigration(ctx, writer, name); err != nil {
			return err
		}
	}
	return nil
}

// appliedMigrations returns the set of already-recorded migration filenames.
// A missing schema_migrations table (fresh db) yields an empty set.
func appliedMigrations(ctx context.Context, writer *sql.DB) (map[string]bool, error) {
	applied := make(map[string]bool)
	rows, err := writer.QueryContext(ctx, `SELECT filename FROM schema_migrations`)
	if err != nil {
		// Fresh database: schema_migrations not created yet.
		if strings.Contains(err.Error(), "no such table") {
			return applied, nil
		}
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = true
	}
	return applied, rows.Err()
}

// applyMigration runs one migration file and records it, atomically.
func applyMigration(ctx context.Context, writer *sql.DB, name string) error {
	body, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("store: read migration %s: %w", name, err)
	}
	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: apply %s: %w", name, err)
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO schema_migrations (filename, applied_at) VALUES (?, ?)`,
		name, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: record %s: %w", name, err)
	}
	return tx.Commit()
}
