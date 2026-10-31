// Package dagtest provides a shared test helper that runs a real dagnats
// server in-process so other packages' tests can exercise the engine against
// a live NATS + JetStream + control plane instead of mocks.
package dagtest

import (
	"fmt"
	"log"
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

// LoopStateMaxBytes is the agent-loop state cap the test server runs with.
// Agent-loop state rides the task payload and the run snapshot (one KV value
// under NATS' 1 MiB max_payload), and dagnats' default cap is 256 KiB. The
// builder's conversation state can outgrow that, so notify raises it;
// deployments set the same value through DAGNATS_LOOP_STATE_MAX_BYTES.
const LoopStateMaxBytes = 512 << 10

// StartSidecar runs a dagnats server in this test process with a private data
// dir and an OS-assigned NATS port, waits until it is ready, and returns its
// NATS client URL. The server is stopped when the test ends.
func StartSidecar(t *testing.T) (natsURL string) {
	t.Helper()

	cfg := server.DefaultConfig()
	cfg.DataDir = dataDir(t)
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.NATSHost = "127.0.0.1"
	cfg.NATSPort = -1
	cfg.LoopStateMaxBytes = LoopStateMaxBytes
	// An unset store budget is half the free disk (dagnats #687), and
	// dagnats' own streams reserve 8 GiB of it, so below ~20 GB free
	// notify's streams are refused. Pin dagnats' 10 GiB ceiling.
	// JetStream still needs that much real free space.
	cfg.MaxStoreBytes = 10 << 30

	srv := server.New(cfg)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run() }()

	url, runErr := waitReady(srv, errCh, 30*time.Second)
	if url == "" {
		go stopAndRemove(srv, errCh, cfg.DataDir)
		if runErr != nil {
			t.Fatalf("dagnats server failed to start: %v", runErr)
		}
		t.Fatalf("dagnats server not ready within 30s")
	}
	t.Cleanup(func() { go stopAndRemove(srv, errCh, cfg.DataDir) })
	return url
}

// waitReady polls until the server reports its NATS client URL (it does so
// only once fully started) or Run returns early with an error.
func waitReady(srv *server.Server, errCh <-chan error, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if url := srv.NATSURL(); url != "" {
			return url, nil
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
// off the test's goroutine so a graceful stop never adds its drain to a test.
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
