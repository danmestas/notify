// Package dagtest provides a shared test helper that runs a real dagnats
// server in-process so other packages' tests can exercise the engine against
// a live NATS + JetStream + HTTP bridge instead of mocks.
package dagtest

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/danmestas/dagnats/server"
)

// StartSidecar runs a dagnats server in this test process with a private data
// dir, waits until it is ready, and returns the NATS client URL and the HTTP
// base URL (which also hosts the worker bridge). The server is stopped via
// t.Cleanup.
func StartSidecar(t *testing.T) (natsURL, httpURL string) {
	t.Helper()

	// HTTP binds :0 and reports the bound address back, but the server exposes
	// no accessor for the NATS client URL, so the NATS port is probed free
	// first. That has a TOCTOU window ("address already in use" from Run);
	// retry the whole start with a fresh port so a lost race is absorbed.
	const attempts = 4
	for attempt := 1; attempt <= attempts; attempt++ {
		natsPort := freePort(t)

		cfg := server.DefaultConfig()
		cfg.DataDir = t.TempDir()
		cfg.HTTPAddr = "127.0.0.1:0"
		cfg.NATSHost = "127.0.0.1"
		cfg.NATSPort = natsPort
		// An unset store budget is half the free disk (dagnats #687), and
		// dagnats' own streams reserve 8 GiB of it, so below ~20 GB free
		// notify's streams are refused. Pin dagnats' 10 GiB ceiling.
		// JetStream still needs that much real free space.
		cfg.MaxStoreBytes = 10 << 30

		srv := server.New(cfg)
		errCh := make(chan error, 1)
		go func() { errCh <- srv.Run() }()

		addr, runErr := waitReady(srv, errCh, 10*time.Second)
		if addr != "" {
			t.Cleanup(func() { stop(t, srv, errCh) })
			return fmt.Sprintf("nats://127.0.0.1:%d", natsPort), "http://" + addr
		}
		if runErr == nil {
			stop(t, srv, errCh)
			t.Fatalf("dagnats server not ready within 10s")
		}
		t.Logf("dagnats server failed on attempt %d/%d: %v; retrying with a fresh port", attempt, attempts, runErr)
	}
	t.Fatalf("dagnats server never became ready after %d attempts", attempts)
	return "", ""
}

// waitReady polls until the server reports its bound HTTP address (it does so
// only once fully started) or Run returns early with an error.
func waitReady(srv *server.Server, errCh <-chan error, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if addr := srv.HTTPAddr(); addr != "" {
			return addr, nil
		}
		select {
		case err := <-errCh:
			if err == nil {
				err = fmt.Errorf("server exited before becoming ready")
			}
			return "", err
		case <-time.After(20 * time.Millisecond):
		}
	}
	return "", nil
}

func stop(t *testing.T, srv *server.Server, errCh <-chan error) {
	srv.Stop()
	select {
	case err := <-errCh:
		if err != nil {
			t.Logf("dagnats server shutdown: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Errorf("dagnats server did not stop within 20s")
	}
}

// freePort asks the OS for an unused TCP port by binding to :0, reading the
// assigned port, and releasing it.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("close free-port listener: %v", err)
	}
	return port
}
