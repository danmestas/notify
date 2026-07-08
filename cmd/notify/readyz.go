package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// bridgeProbeTimeout bounds a single reachability check against the dagnats
// bridge, both each boot-probe attempt and every /readyz request.
const bridgeProbeTimeout = 3 * time.Second

// Boot-probe retry bound: up to bridgeProbeAttempts tries, bridgeProbeBackoff
// apart, so a notify that starts before the dagnats bridge's HTTP listener is
// up (mini reboot / dagnats restart) waits ~30s for it rather than exiting
// immediately. The deploy target is a launchd-managed always-on mac-mini (no
// docker-compose health-gating), so a single-shot fatal exit here would just
// make launchd crash-loop notify until the bridge happens to be ready.
const (
	bridgeProbeAttempts = 10
	bridgeProbeBackoff  = 3 * time.Second
)

// bridgeReady makes a short-timeout GET to the dagnats bridge's /ready
// endpoint — the same endpoint internal/dagtest polls to know a sidecar has
// finished starting (see internal/dagtest/sidecar.go's waitReadyOK) — and
// reports whether it answered 2xx. The returned error is only ever a
// transport-level failure (connection refused, DNS, timeout, ...); a
// reachable bridge that answers non-2xx reports ok=false, err=nil.
func bridgeReady(ctx context.Context, client *http.Client, bridgeURL string) (ok bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bridgeURL+"/ready", nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}

// probeBridgeReachable runs the boot-time reachability check against the
// dagnats bridge with the production retry bound. See probeBridgeReachableN.
func probeBridgeReachable(ctx context.Context, client *http.Client, bridgeURL string) error {
	return probeBridgeReachableN(ctx, client, bridgeURL, bridgeProbeAttempts, bridgeProbeBackoff)
}

// probeBridgeReachableN retries the reachability check up to attempts times,
// backoff apart, and only returns a fatal error if the bridge is STILL
// unreachable after every attempt. A transport-level failure (nothing
// listening at bridgeURL) is retried rather than immediately fatal: on the
// launchd-managed mini, notify can start before the bridge's HTTP listener is
// up, and an immediate exit would crash-loop it. Once all attempts are spent
// the failure is fatal — a persistently-unreachable bridge is almost always a
// misconfigured -bridge-url/NOTIFY_BRIDGE_URL, and continuing would let every
// send.* task silently fail forever once the engine starts polling. A bridge
// that is reachable but not yet answering 2xx (still finishing its own
// startup) is logged and swallowed WITHOUT retrying: /readyz re-checks live on
// every request and flips to ready once the bridge catches up, so blocking
// boot on a transient non-2xx would just trade one startup race for another.
func probeBridgeReachableN(ctx context.Context, client *http.Client, bridgeURL string, attempts int, backoff time.Duration) error {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, bridgeProbeTimeout)
		ok, err := bridgeReady(pctx, client, bridgeURL)
		cancel()
		if err == nil {
			if !ok {
				slog.Default().Warn("dagnats bridge probe: reachable but not answering ready yet, continuing boot", "url", bridgeURL)
			}
			return nil
		}
		lastErr = err
		if attempt < attempts {
			slog.Default().Info("dagnats bridge probe: unreachable, retrying",
				"url", bridgeURL, "attempt", attempt, "attempts", attempts, "retry_in", backoff, "err", err)
			select {
			case <-ctx.Done():
				return fmt.Errorf("dagnats bridge unreachable at %s: %w", bridgeURL, ctx.Err())
			case <-time.After(backoff):
			}
		}
	}
	return fmt.Errorf("dagnats bridge unreachable at %s after %d attempts: %w", bridgeURL, attempts, lastErr)
}

// readyzResponse is the GET /readyz JSON body.
type readyzResponse struct {
	Status string `json:"status"`
	Bridge bool   `json:"bridge"`
	DB     bool   `json:"db"`
}

// readyzHandler reports 200 once the dagnats bridge answers /ready and writer
// answers a ping; until both pass it reports 503. Both checks run live on
// every request (not cached at boot), so the endpoint reflects current
// reachability rather than a stale boot-time snapshot.
func readyzHandler(bridgeURL string, client *http.Client, writer *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), bridgeProbeTimeout)
		defer cancel()

		bridgeOK, _ := bridgeReady(ctx, client, bridgeURL)
		dbOK := writer.PingContext(ctx) == nil
		resp := readyzResponse{Status: "not ready", Bridge: bridgeOK, DB: dbOK}

		w.Header().Set("Content-Type", "application/json")
		if bridgeOK && dbOK {
			resp.Status = "ok"
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
