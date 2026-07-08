package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestProbeBridgeReachableN_RetriesUntilUp pins the boot-probe retry behavior
// (review nit): on the launchd-managed mini, notify can start before the
// dagnats bridge's HTTP listener is up, so a transport failure must be retried
// with backoff, not fatally exit on the first try.
func TestProbeBridgeReachableN_RetriesUntilUp(t *testing.T) {
	// A live bridge, plus a swapTransport that routes the first two probe
	// attempts to a closed address (connection-refused) and every later
	// attempt to the live server — simulating the bridge's HTTP listener
	// coming up mid-retry without rebinding a port.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close() // deadURL now refuses connections

	client := &http.Client{Transport: &swapTransport{
		base:    http.DefaultTransport,
		deadURL: deadURL,
		liveURL: srv.URL,
		flipAt:  2,
		hits:    &hits,
	}}

	ctx := context.Background()
	err := probeBridgeReachableN(ctx, client, deadURL, 10, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("probeBridgeReachableN eventually-up: got fatal error %v, want nil (probe must retry until the bridge answers)", err)
	}
	if got := atomic.LoadInt32(&hits); got < 3 {
		t.Errorf("probe made %d attempts, want >= 3 (should have retried past the refusing attempts)", got)
	}
}

// TestProbeBridgeReachableN_FatalAfterBoundedAttempts pins that a bridge that
// NEVER answers still fails fatally, but only after the bounded retries — so a
// genuinely misconfigured bridge URL surfaces as os.Exit(1) rather than
// hanging forever.
func TestProbeBridgeReachableN_FatalAfterBoundedAttempts(t *testing.T) {
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close() // refuses connections for the whole test

	ctx := context.Background()
	err := probeBridgeReachableN(ctx, http.DefaultClient, deadURL, 3, 5*time.Millisecond)
	if err == nil {
		t.Fatal("probeBridgeReachableN never-up: got nil, want fatal error after bounded attempts")
	}
}

// TestProbeBridgeReachableN_ReachableNon2xxContinues pins that a reachable
// bridge answering non-2xx does NOT block boot and does NOT retry — /readyz
// re-checks live per request.
func TestProbeBridgeReachableN_ReachableNon2xxContinues(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx := context.Background()
	err := probeBridgeReachableN(ctx, http.DefaultClient, srv.URL, 10, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("reachable non-2xx: got fatal error %v, want nil (log-and-continue)", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("probe made %d attempts, want exactly 1 (non-2xx must not retry)", got)
	}
}

// swapTransport routes the probe's requests to deadURL for the first flipAt
// attempts (yielding connection-refused), then to liveURL, letting a test
// simulate the bridge listener coming up mid-retry without rebinding a port.
type swapTransport struct {
	base    http.RoundTripper
	deadURL string
	liveURL string
	flipAt  int32
	hits    *int32
}

func (s *swapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	n := atomic.AddInt32(s.hits, 1)
	target := s.deadURL
	if n > s.flipAt {
		target = s.liveURL
	}
	live, err := req.URL.Parse(target + req.URL.Path)
	if err != nil {
		return nil, err
	}
	req.URL = live
	req.Host = live.Host
	return s.base.RoundTrip(req)
}

// TestReadyzHandler pins /readyz's three observable states (notify hardening
// #6): both dependencies up -> 200 "ok"; the dagnats bridge unreachable ->
// 503; the writer DB pool unreachable -> 503. Both checks run live per
// request against real stand-ins (an httptest bridge, a real sqlite *sql.DB),
// not mocks.
func TestReadyzHandler(t *testing.T) {
	t.Run("bridge ok and db ok: 200", func(t *testing.T) {
		bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ready" {
				t.Errorf("probe hit %s, want /ready", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer bridge.Close()

		db := openWorkingDB(t)
		defer func() { _ = db.Close() }()

		rec := doReadyz(t, bridge.URL, db)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var resp readyzResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if resp.Status != "ok" || !resp.Bridge || !resp.DB {
			t.Errorf("resp = %+v, want status=ok bridge=true db=true", resp)
		}
	})

	t.Run("bridge unreachable: 503", func(t *testing.T) {
		bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		bridgeURL := bridge.URL
		bridge.Close() // nothing listens at bridgeURL now

		db := openWorkingDB(t)
		defer func() { _ = db.Close() }()

		rec := doReadyz(t, bridgeURL, db)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
		}
		var resp readyzResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if resp.Bridge {
			t.Errorf("resp.Bridge = true, want false (bridge is closed)")
		}
	})

	t.Run("db closed: 503", func(t *testing.T) {
		bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer bridge.Close()

		db := openWorkingDB(t)
		_ = db.Close() // closed before the request

		rec := doReadyz(t, bridge.URL, db)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
		}
		var resp readyzResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if resp.DB {
			t.Errorf("resp.DB = true, want false (db is closed)")
		}
	})
}

func doReadyz(t *testing.T, bridgeURL string, db *sql.DB) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	readyzHandler(bridgeURL, http.DefaultClient, db)(rec, req)
	return rec
}

func openWorkingDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping sqlite: %v", err)
	}
	return db
}
