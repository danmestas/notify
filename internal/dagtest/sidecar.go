// Package dagtest provides a shared test helper that spawns a real dagnats
// sidecar process so other packages' tests can exercise the engine against
// a live NATS + JetStream + HTTP bridge instead of mocks.
package dagtest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// StartSidecar spawns the prebuilt dagnats binary with a private data dir and
// freshly-allocated free ports, polls its HTTP /ready endpoint until it serves
// 200, and returns the NATS client URL and the HTTP base URL (which also hosts
// the worker bridge). The process is killed via t.Cleanup.
func StartSidecar(t *testing.T) (natsURL, httpURL string) {
	t.Helper()

	bin := dagnatsBinary(t)

	// freePort has an inherent TOCTOU window: a port that is free when probed can
	// be taken by another process before the sidecar binds it, surfacing as
	// "bind: address already in use" and a sidecar that never reaches /ready.
	// Retry the whole start with freshly allocated ports so a lost race is
	// absorbed instead of flaking the test.
	const attempts = 4
	for attempt := 1; attempt <= attempts; attempt++ {
		natsPort := freePort(t)
		httpPort := freePort(t)
		httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)
		dataDir := t.TempDir()

		// --die-with-parent (dagnats #476): the sidecar self-terminates when
		// this test process dies — including hard `-timeout`/SIGKILL where
		// t.Cleanup never runs — so interrupted `go test` runs don't orphan a
		// dagnats child (notify #121).
		cmd := exec.Command(bin, "serve", "--die-with-parent")
		// Run in the data dir so no stray ./dagnats.yaml is picked up, and point
		// XDG elsewhere for the same reason.
		cmd.Dir = dataDir
		cmd.Env = append(
			os.Environ(),
			"DAGNATS_DATA_DIR="+dataDir,
			"DAGNATS_HTTP_ADDR="+httpAddr,
			fmt.Sprintf("DAGNATS_NATS_PORT=%d", natsPort),
			"XDG_CONFIG_HOME="+filepath.Join(dataDir, "xdg"),
			// An unset store budget is half the free disk (dagnats #687), and
			// dagnats' own streams reserve 8 GiB of it, so below ~20 GB free
			// notify's streams are refused. Pin dagnats' 10 GiB ceiling, as
			// v0.0.14 had. JetStream still needs that much real free space.
			"DAGNATS_MAX_STORE_BYTES=10737418240",
		)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			t.Fatalf("start dagnats sidecar: %v", err)
		}

		if waitReadyOK(httpAddr, 8*time.Second) {
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			})
			return fmt.Sprintf("nats://127.0.0.1:%d", natsPort), "http://" + httpAddr
		}

		// Most likely a lost port-bind race; tear this attempt down and retry.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Logf("dagnats sidecar not ready on attempt %d/%d (httpAddr=%s); retrying with fresh ports", attempt, attempts, httpAddr)
	}
	t.Fatalf("dagnats sidecar never became ready after %d attempts (GOOS=%s)", attempts, runtime.GOOS)
	return "", ""
}

// Binary returns the path to the prebuilt dagnats binary so tests can drive
// the dagnats CLI (e.g. `dlq list` / `dlq replay`) against a sidecar started
// by StartSidecar. Same resolution rules as the sidecar spawn.
func Binary(t *testing.T) string {
	t.Helper()
	return dagnatsBinary(t)
}

// dagnatsBinary returns the path to the prebuilt dagnats binary, failing
// clearly if it is missing. The DAGNATS_BIN env var overrides the default
// location (set by CI, which builds the binary at a path that is not under
// ~/projects/notify); otherwise the local development path is used.
func dagnatsBinary(t *testing.T) string {
	t.Helper()

	if bin := os.Getenv("DAGNATS_BIN"); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("dagnats binary not found at DAGNATS_BIN=%s: %v", bin, err)
		}
		return bin
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	bin := filepath.Join(home, "projects", "notify", ".bin", "dagnats")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("dagnats binary not found at %s: %v", bin, err)
	}
	return bin
}

// freePort asks the OS for an unused TCP port by binding to :0, reading the
// assigned port, and releasing it. There is an inherent race window before the
// sidecar binds, but it is small enough for test use.
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

// waitReadyOK polls the sidecar's /ready endpoint and reports whether it
// returned 200 within timeout — non-fatal so callers can retry with fresh ports
// after a lost bind race.
func waitReadyOK(httpAddr string, timeout time.Duration) bool {
	url := "http://" + httpAddr + "/ready"
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodGet, url, nil,
		)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
