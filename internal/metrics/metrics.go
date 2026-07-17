// Package metrics publishes notify's runtime counters through the stdlib
// expvar registry, served as JSON at /metrics. It is deliberately the lighter
// mechanism than prometheus: stdlib-only, zero dependencies, and process-global
// by construction so delivery and eventbus call sites need no injected handle.
//
// The surface is three flat groups of vars, all under a "notify_" prefix so a
// scraper can filter them out of expvar's default cmdline/memstats noise:
//
//   - notify_delivery_total — an expvar.Map keyed by "<channel>.<outcome>"
//     (e.g. "email.delivered", "chat.throttled"), one counter per delivery
//     persist/success path.
//   - notify_nats_disconnects / notify_nats_reconnects — cumulative counters
//     incremented from the shared DialNATS reconnect handlers.
//   - notify_nats_status — an expvar.Func returning {conn-name: status} read
//     live from each registered connection, so a scrape reflects the current
//     link state (the zombie-mode symptom in hardening #1) rather than a stale
//     cached value.
//
// Every entry point is nil-safe and cheap: counters are atomic, and a call
// against an unknown channel/outcome simply creates a new map key.
package metrics

import (
	"expvar"
	"sync"
)

// Delivery outcome constants. These are the values that pair with a channel to
// form a notify_delivery_total map key; the delivery handlers pass them at each
// terminal persist point.
const (
	OutcomeDelivered = "delivered"
	OutcomeSkipped   = "skipped"
	OutcomeThrottled = "throttled"
	OutcomeError     = "error"
)

// Published expvar var names. Kept as consts so tests and any future scraper
// adapter reference the same strings the registry is keyed on.
const (
	deliveryMapName = "notify_delivery_total"
	natsStatusName  = "notify_nats_status"
)

var (
	deliveryTotal   = expvar.NewMap(deliveryMapName)
	natsDisconnects = expvar.NewInt("notify_nats_disconnects")
	natsReconnects  = expvar.NewInt("notify_nats_reconnects")

	statusMu  sync.RWMutex
	statusFns = map[string]func() string{}
)

func init() {
	// notify_nats_status reports the live status of every registered connection.
	// expvar.Func is evaluated on each scrape, so the value always reflects the
	// current *nats.Conn state rather than a snapshot taken at dial time.
	expvar.Publish(natsStatusName, expvar.Func(func() any {
		statusMu.RLock()
		defer statusMu.RUnlock()
		out := make(map[string]string, len(statusFns))
		for name, fn := range statusFns {
			out[name] = fn()
		}
		return out
	}))
}

// Delivery increments the counter for one (channel, outcome) pair. It is the
// single line every delivery persist/success path calls — no handler signature
// changes, safe to call concurrently, and self-registering for unseen keys.
func Delivery(channel, outcome string) {
	deliveryTotal.Add(channel+"."+outcome, 1)
}

// NATSDisconnect increments the cumulative NATS-disconnect counter. Called from
// the shared DialNATS DisconnectErrHandler.
func NATSDisconnect() { natsDisconnects.Add(1) }

// NATSReconnect increments the cumulative NATS-reconnect counter. Called from
// the shared DialNATS ReconnectHandler.
func NATSReconnect() { natsReconnects.Add(1) }

// SetNATSStatus registers (or replaces) a live status provider for a named
// connection. fn is invoked on every /metrics scrape; it must be cheap and
// non-blocking (nats.Conn.Status().String() is both). Registering the same name
// again overwrites the prior provider, so a re-dial under the same name does not
// leak entries.
func SetNATSStatus(name string, fn func() string) {
	statusMu.Lock()
	statusFns[name] = fn
	statusMu.Unlock()
}
