//go:build duckdb

package main

import (
	"context"
	"fmt"

	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/analyticsduck"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/store"
)

// analyticsBackend (duckdb build) routes delivery-status analytics into an
// embedded DuckDB columnar store instead of SQLite. The DuckDB database path
// comes from NOTIFY_DUCKDB (default notify-analytics.duckdb); the returned
// close func closes that handle. The SQLite store (st) still backs all OLTP
// repos — only analytics moves to DuckDB. Build with `-tags duckdb`.
func analyticsBackend(ctx context.Context, _ *store.Store, bus *eventbus.Bus) (*analytics.Service, func() error, error) {
	path := envOr("NOTIFY_DUCKDB", "notify-analytics.duckdb")
	repo, err := analyticsduck.Open(ctx, path)
	if err != nil {
		return nil, nil, fmt.Errorf("analyticsduck.Open: %w", err)
	}
	return analytics.NewWithRepo(repo, bus), repo.Close, nil
}
