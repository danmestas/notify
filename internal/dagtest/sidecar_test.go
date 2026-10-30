package dagtest_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/notify/internal/dagtest"
)

// TestStartSidecar_InProcess proves the sidecar needs no dagnats binary: with
// PATH empty, HOME pointed at an empty dir and DAGNATS_BIN naming nothing, it
// still serves NATS + JetStream (with dagnats' streams provisioned) and the
// HTTP /ready endpoint.
func TestStartSidecar_InProcess(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DAGNATS_BIN", "/nonexistent/dagnats")

	natsURL, httpURL := dagtest.StartSidecar(t)

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect %s: %v", natsURL, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.Stream(ctx, "DEAD_LETTERS"); err != nil {
		t.Fatalf("dagnats streams not provisioned: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpURL+"/ready", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /ready: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ready = %d, want 200", resp.StatusCode)
	}
}
