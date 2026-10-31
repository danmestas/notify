//go:build !duckdb

package notify

import (
	"context"

	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/store"
)

// analyticsBackend builds the analytics Service on the default pure-Go SQLite
// AnalyticsRepo. The returned close func is a no-op — the SQLite store is owned
// and closed by the caller. Build with `-tags duckdb` to route analytics into
// an embedded DuckDB OLAP store instead (see analytics_backend_duck.go).
func analyticsBackend(_ context.Context, st *store.Store, bus *eventbus.Bus, _ string) (*analytics.Service, func() error, error) {
	return analytics.New(st, bus), func() error { return nil }, nil
}
