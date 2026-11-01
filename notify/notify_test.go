package notify_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/notify"
)

// newNotify builds a notify server on an in-process dagnats, with the host's
// connection returned so a test can check notify left it alone.
func newNotify(t *testing.T, basePath string) (*notify.Notify, *nats.Conn) {
	t.Helper()
	nc, err := nats.Connect(dagtest.StartSidecar(t), engine.InboxOption())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	n, err := notify.New(context.Background(), notify.Host{NC: nc, BasePath: basePath}, notify.Config{
		DBPath:    filepath.Join(t.TempDir(), "notify.db"),
		MasterKey: make([]byte, 32),
	})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	t.Cleanup(func() { _ = n.Stop(context.Background()) })
	return n, nc
}

func get(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}

func TestNotify_StartStop(t *testing.T) {
	n, nc := newNotify(t, "")
	ctx := context.Background()
	if err := n.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if code := get(t, n.Handler(), "/readyz"); code != http.StatusOK {
		t.Fatalf("GET /readyz after Start = %d, want 200", code)
	}

	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := n.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if nc.IsClosed() {
		t.Fatal("Stop closed the host's connection")
	}
	if err := n.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestNotify_BasePath(t *testing.T) {
	n, _ := newNotify(t, "/n")
	h := n.Handler()
	for path, want := range map[string]int{
		"/n/readyz":  http.StatusOK,
		"/n/healthz": http.StatusOK,
		"/readyz":    http.StatusNotFound,
		"/healthz":   http.StatusNotFound,
	} {
		if got := get(t, h, path); got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}
}

func TestNotify_Backup(t *testing.T) {
	n, _ := newNotify(t, "")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backup.db")
	if err := n.Backup(ctx, path); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer func() { _ = db.Close() }()
	var name string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'organizations'`).Scan(&name); err != nil {
		t.Fatalf("backup has no organizations table: %v", err)
	}

	if err := n.Backup(ctx, path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("Backup over an existing file = %v, want os.ErrExist", err)
	}
}
