package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
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

// Bridge-auth preflight bounds: a rejected credential is fatal on the first
// answer (it is never transient), but a 5xx or transport error may be a bridge
// still finishing its own startup, so those get a few short retries.
const (
	bridgeAuthAttempts = 3
	bridgeAuthBackoff  = 2 * time.Second
)

// probeBridgeAuth verifies that the dagnats bridge accepts our credentials
// before notify starts serving. It is the companion to probeBridgeReachable:
// that one proves something is listening, this one proves it will hand us
// tasks.
//
// The probe POSTs a DELIBERATELY INVALID body -- {"task_types":[]} -- to
// /v1/tasks/poll. That is not a trick for its own sake: dagnats authenticates
// in middleware, before it parses the request, so an unparseable body reaches
// the auth decision and stops there. That makes it the only cheap probe
// available. A *valid* poll would either block for the full long-poll window
// (dagnats clamps timeout_ms <= 0 up to 5000) or fetch and silently drop a
// real task (it clamps max_tasks <= 0 up to 1) -- a boot check must not eat a
// customer's notification to prove it can read the queue.
//
// So the status code carries the whole answer:
//
//	400        credentials accepted, body rejected as designed -- pass
//	200        also pass; the bridge somehow accepted the body, which is fine
//	401 / 403  bad or missing token -- fatal, and never retried
//	404 / 405  not a dagnats bridge at all (the -bridge-url default is
//	           :8080, which is dockerd on some hosts) -- fatal
//	5xx        possibly still starting -- retried, then fatal
//
// Booting past any of those is what #325 cost four days: notify accepted 117
// triggers, executed none of them, and logged nothing.
func probeBridgeAuth(ctx context.Context, client *http.Client, bridgeURL, token string) error {
	return probeBridgeAuthN(ctx, client, bridgeURL, token, bridgeAuthAttempts, bridgeAuthBackoff)
}

// probeBridgeAuthN is probeBridgeAuth with the retry bound injected, for tests.
func probeBridgeAuthN(ctx context.Context, client *http.Client, bridgeURL, token string, attempts int, backoff time.Duration) error {
	// Describes how notify is configured without ever revealing the secret.
	credentials := "no DAGNATS_BRIDGE_TOKEN set"
	if token != "" {
		credentials = "DAGNATS_BRIDGE_TOKEN set"
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		status, err := bridgeAuthProbeStatus(ctx, client, bridgeURL, token)
		if err == nil {
			switch {
			case status == http.StatusBadRequest, status == http.StatusOK:
				return nil
			case status == http.StatusUnauthorized, status == http.StatusForbidden:
				return fmt.Errorf(
					"dagnats bridge at %s rejected notify's credentials (HTTP %d, %s): "+
						"the bridge requires Authorization: Bearer <DAGNATS_BRIDGE_TOKEN> and notify must be "+
						"given the same value the sidecar was started with; without it every task poll is "+
						"refused and no workflow step ever runs",
					bridgeURL, status, credentials,
				)
			case status == http.StatusNotFound, status == http.StatusMethodNotAllowed:
				return fmt.Errorf(
					"%s does not look like a dagnats worker bridge (HTTP %d on /v1/tasks/poll): "+
						"check -bridge-url/NOTIFY_BRIDGE_URL points at the sidecar's bridge port",
					bridgeURL, status,
				)
			case status >= 500:
				lastErr = fmt.Errorf("dagnats bridge at %s answered HTTP %d", bridgeURL, status)
			default:
				return fmt.Errorf(
					"dagnats bridge at %s answered unexpected HTTP %d to the auth preflight (%s)",
					bridgeURL, status, credentials,
				)
			}
		} else {
			lastErr = err
		}
		if attempt < attempts {
			slog.Default().Info("dagnats bridge auth preflight: retrying",
				"url", bridgeURL, "attempt", attempt, "attempts", attempts, "retry_in", backoff, "err", lastErr)
			select {
			case <-ctx.Done():
				return fmt.Errorf("dagnats bridge auth preflight at %s: %w", bridgeURL, ctx.Err())
			case <-time.After(backoff):
			}
		}
	}
	return fmt.Errorf("dagnats bridge auth preflight failed at %s after %d attempts: %w", bridgeURL, attempts, lastErr)
}

// bridgeAuthProbeStatus performs one preflight POST and returns its status
// code. The returned error is only ever a transport-level failure.
func bridgeAuthProbeStatus(ctx context.Context, client *http.Client, bridgeURL, token string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, bridgeProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bridgeURL+"/v1/tasks/poll",
		strings.NewReader(`{"task_types":[]}`))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the connection can be reused; the body is never interesting.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
