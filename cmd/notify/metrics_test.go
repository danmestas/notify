package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/metrics"
)

// TestMetricsMuxOrdering pins the ServeMux contract that run()'s root mux relies
// on: with both "/metrics" (expvar.Handler) and a "/" catch-all registered, a
// GET /metrics resolves to the expvar handler (200, JSON carrying the notify_
// keys) and is never shadowed by the catch-all. It reconstructs the same two
// registrations run() uses rather than exercising run()'s mux directly, so it
// validates the handler + longest-prefix behavior we depend on — not that main
// still performs the mount (that wiring is asserted by boot/e2e coverage).
func TestMetricsMuxOrdering(t *testing.T) {
	const catchAllBody = "CATCH_ALL"
	root := http.NewServeMux()
	root.Handle("/metrics", metrics.Handler())
	root.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, catchAllBody)
	})

	srv := httptest.NewServer(root)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), catchAllBody) {
		t.Fatalf("/metrics fell through to the catch-all handler")
	}
	if !strings.Contains(string(body), "notify_delivery_total") {
		t.Fatalf("/metrics body missing notify_delivery_total key; got: %s", body)
	}
}
