//go:build duckdb

package notify

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
// is Config.DuckDBPath (NOTIFY_DUCKDB, default notify-analytics.duckdb); the returned
// close func closes that handle. The SQLite store (st) still backs all OLTP
// repos — only analytics moves to DuckDB. Build with `-tags duckdb`.
//
// With Config.AnalyticsLake set (NOTIFY_ANALYTICS_LAKE), queries read the
// analytics lake instead and the projector keeps writing SQLite; DuckDBPath is
// not opened.
func analyticsBackend(ctx context.Context, st *store.Store, bus *eventbus.Bus, cfg Config) (*analytics.Service, func() error, error) {
	if cfg.AnalyticsLake.Enabled() {
		repo, err := analyticsduck.OpenLake(ctx, cfg.AnalyticsLake, st.Analytics())
		if err != nil {
			return nil, nil, fmt.Errorf("analyticsduck.OpenLake: %w", err)
		}
		return analytics.NewWithRepo(repo, bus), repo.Close, nil
	}
	repo, err := analyticsduck.Open(ctx, cfg.DuckDBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("analyticsduck.Open: %w", err)
	}
	return analytics.NewWithRepo(repo, bus), repo.Close, nil
}
