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

	natsPort := freePort(t)
	httpPort := freePort(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)
	dataDir := t.TempDir()

	cmd := exec.Command(bin, "serve")
	// Run in the data dir so no stray ./dagnats.yaml is picked up, and point
	// XDG elsewhere for the same reason.
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

	return fmt.Sprintf("nats://127.0.0.1:%d", natsPort), "http://" + httpAddr
}

// dagnatsBinary returns the path to the prebuilt dagnats binary, failing
// clearly if it is missing.
func dagnatsBinary(t *testing.T) string {
	t.Helper()

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
