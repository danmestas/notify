//go:build !duckdb

package notify

import (
	"context"
	"fmt"

	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/store"
)

// analyticsBackend builds the analytics Service on the default pure-Go SQLite
// AnalyticsRepo, and names the backend for the startup log. The returned close
// func is a no-op — the SQLite store is owned and closed by the caller. Build with `-tags duckdb` to route analytics into
// an embedded DuckDB OLAP store, or to read it from the lake (see
// analytics_backend_duck.go). A configured lake is refused here rather than
// ignored, so a misbuilt binary does not silently serve SQLite.
func analyticsBackend(_ context.Context, st *store.Store, bus *eventbus.Bus, cfg Config) (*analytics.Service, func() error, string, error) {
	if cfg.AnalyticsLake.Enabled() {
		return nil, nil, "", fmt.Errorf("the analytics lake (%s) needs a `-tags duckdb` build", cfg.AnalyticsLake)
	}
	return analytics.New(st, bus), func() error { return nil }, "sqlite", nil
}
