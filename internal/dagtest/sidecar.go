// Package dagtest provides a shared test helper that runs a real dagnats
// server in-process so other packages' tests can exercise the engine against
// a live NATS + JetStream + HTTP bridge instead of mocks.
package dagtest

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/danmestas/dagnats/server"
)

// StartSidecar runs a dagnats server in this test process with a private data
// dir, waits until it is ready, and returns the NATS client URL and the HTTP
// base URL (which also hosts the worker bridge). The server is stopped when
// the test ends.
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
		cfg.DataDir = dataDir(t)
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
			t.Cleanup(func() { go stopAndRemove(srv, errCh, cfg.DataDir) })
			return fmt.Sprintf("nats://127.0.0.1:%d", natsPort), "http://" + addr
		}
		if runErr == nil {
			go stopAndRemove(srv, errCh, cfg.DataDir)
			t.Fatalf("dagnats server not ready within 10s")
		}
		_ = os.RemoveAll(cfg.DataDir)
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

// stopAndRemove shuts the server down and then deletes its data dir. It runs
// off the test's goroutine: a graceful stop waits out any in-flight bridge
// long-poll (a JetStream Fetch that ignores the client going away, up to the
// engine's 5s poll timeout), which would otherwise add that wait to every test.
func stopAndRemove(srv *server.Server, errCh <-chan error, dir string) {
	srv.Stop()
	select {
	case err := <-errCh:
		if err != nil {
			log.Printf("dagtest: dagnats server shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		log.Printf("dagtest: dagnats server did not stop within 30s")
	}
	_ = os.RemoveAll(dir)
}

// Data dirs live under a per-process parent, so dirs a test binary leaves when
// it exits before a background stop finishes are swept by the next run.
const parentPrefix = "notify-dagtest-"

var sweepOnce sync.Once

// dataDir returns a fresh data dir under this process's parent, first sweeping
// parents left behind by test processes that have since exited.
func dataDir(t *testing.T) string {
	t.Helper()
	sweepOnce.Do(sweepDeadParents)
	parent := filepath.Join(os.TempDir(), parentPrefix+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("create dagtest parent dir: %v", err)
	}
	dir, err := os.MkdirTemp(parent, "srv-")
	if err != nil {
		t.Fatalf("create dagtest data dir: %v", err)
	}
	return dir
}

func sweepDeadParents() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), parentPrefix)
		if !ok || !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(rest)
		if err != nil || pid == os.Getpid() {
			continue
		}
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			_ = os.RemoveAll(filepath.Join(os.TempDir(), e.Name()))
		}
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
