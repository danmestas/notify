package eventbus

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

// startSidecar builds (if necessary) and spawns the dagnats sidecar binary
// with a private data dir and freshly-allocated free ports, polls its HTTP
// /ready endpoint, and returns the NATS client URL. The process is killed
// via t.Cleanup.
func startSidecar(t *testing.T) (natsURL string) {
	t.Helper()

	bin := dagnatsBinary(t)

	// freePort has a TOCTOU window: a probed-free port can be taken before the
	// sidecar binds it ("bind: address already in use" -> never ready). Retry the
	// whole start with fresh ports so a lost race is absorbed, not flaked.
	const attempts = 4
	for attempt := 1; attempt <= attempts; attempt++ {
		natsPort := freePort(t)
		httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
		dataDir := t.TempDir()

		// --die-with-parent (dagnats #476): the sidecar self-terminates when
		// this test process dies — including hard `-timeout`/SIGKILL where
		// t.Cleanup never runs — so interrupted `go test` runs don't orphan a
		// dagnats child (notify #121).
		cmd := exec.Command(bin, "serve", "--die-with-parent")
		// Run in the data dir so no stray ./dagnats.yaml from the test
		// package is picked up, and point XDG elsewhere for the same reason.
		cmd.Dir = dataDir
		cmd.Env = append(
			os.Environ(),
			"DAGNATS_DATA_DIR="+dataDir,
			"DAGNATS_HTTP_ADDR="+httpAddr,
			fmt.Sprintf("DAGNATS_NATS_PORT=%d", natsPort),
			"XDG_CONFIG_HOME="+filepath.Join(dataDir, "xdg"),
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
			return fmt.Sprintf("nats://127.0.0.1:%d", natsPort)
		}

		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Logf("dagnats sidecar not ready on attempt %d/%d (httpAddr=%s); retrying with fresh ports", attempt, attempts, httpAddr)
	}
	t.Fatalf("dagnats sidecar never became ready after %d attempts (GOOS=%s)", attempts, runtime.GOOS)
	return ""
}

// dagnatsBinary returns the path to the dagnats binary, building it from
// the local reference checkout if it is not already present. The DAGNATS_BIN
// env var overrides discovery (set by CI, which builds the binary with
// `go install` at a path outside ~/projects/notify).
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
	if _, err := os.Stat(bin); err == nil {
		return bin
	}

	src := filepath.Join(home, "references", "dagnats")
	build := exec.Command("go", "build", "-o", bin, "./cmd/dagnats")
	build.Dir = src
	build.Env = append(os.Environ(), "GOFLAGS=-mod=readonly")
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build dagnats binary: %v", err)
	}
	return bin
}

// freePort asks the OS for an unused TCP port by binding to :0, reading the
// assigned port, and releasing it. There is an inherent race window before
// the sidecar binds, but it is small enough for test use.
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
// returned 200 within timeout — non-fatal so the caller can retry with fresh
// ports after a lost bind race.
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
