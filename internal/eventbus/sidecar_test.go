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

	natsPort := freePort(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	dataDir := t.TempDir()

	cmd := exec.Command(bin, "serve")
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

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	waitReady(t, httpAddr)

	return fmt.Sprintf("nats://127.0.0.1:%d", natsPort)
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

// waitReady polls the sidecar's /ready endpoint until it returns 200 or the
// timeout elapses.
func waitReady(t *testing.T, httpAddr string) {
	t.Helper()
	url := "http://" + httpAddr + "/ready"
	deadline := time.Now().Add(20 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodGet, url, nil,
		)
		if err != nil {
			t.Fatalf("build ready request: %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("dagnats sidecar at %s never became ready within 20s (GOOS=%s)", url, runtime.GOOS)
}
