//go:build !duckdb

package notify_test

// Methodology: an in-process dagnats and a real notify.New with a capturing
// slog handler. Operators need to see which analytics backend a process
// serves (#119): the lake and SQLite answer the same API, so nothing else
// tells them apart. Positive: a default build logs the SQLite backend once.
// Negative: it doesn't claim the lake.

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/notify"
)

func TestNew_LogsAnalyticsBackend(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t), engine.InboxOption())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	n, err := notify.New(context.Background(), notify.Host{NC: nc, Logger: logger}, notify.Config{
		DBPath:    filepath.Join(t.TempDir(), "notify.db"),
		MasterKey: make([]byte, 32),
	})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	t.Cleanup(func() { _ = n.Stop(context.Background()) })

	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "analytics backend") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want exactly one analytics backend line, got %d:\n%s", len(lines), logs.String())
	}
	if !strings.Contains(lines[0], "backend=sqlite") {
		t.Fatalf("default build should log backend=sqlite: %s", lines[0])
	}
	if strings.Contains(lines[0], "lake") {
		t.Fatalf("default build must not claim the lake: %s", lines[0])
	}
}
