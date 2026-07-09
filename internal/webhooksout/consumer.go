// Package webhooksout consumes notify's delivery-status events off the event
// bus and POSTs each raw event to the active outbound webhook endpoints
// configured for the event's tenancy scope (OrgID/EnvID).
//
// Start rides eventbus.Bus.ConsumeAllDeliveryDurable, a durable JetStream
// consumer: events published while no consumer is running (a notify restart,
// or an outage longer than the old core-NATS subscription's window) are
// still delivered once a consumer resumes — at-least-once, not at-most-once.
//
// # Ack/nak policy
//
// dispatch (the durable handler) acks or naks per an asymmetry keyed on
// whether a retry could ever help:
//
//   - malformed JSON / scope-parse error: ACK. A poison event is terminal —
//     redelivering identical bytes will fail identically — so acking drops
//     it rather than letting it wedge the shared durable for every tenant
//     behind it.
//   - store lookup error (ListActiveByEnv): NAK (dispatch returns the
//     error). This is a transient class — DB busy / lock contention — that
//     a redelivery is likely to clear, and it is exactly what JetStream's
//     MaxDeliver:5 / AckWait exist to survive. Acking here would silently
//     drop the event with zero retries. Naking one message does NOT
//     head-of-line-block the stream: default MaxAckPending (~1000) lets the
//     rest keep flowing while this one backs off and is redelivered.
//   - endpoint POST failure: ACK once post's own bounded retry
//     (maxAttempts x retryDelay) is exhausted. "At-least-once" here means
//     the event survives notify restarts / consumer downtime, NOT that
//     notify will retry an HTTP POST forever — turning a permanently-dead
//     tenant endpoint into a nak would, on immediate redelivery, loop the
//     single dispatch path on that endpoint indefinitely (bounded only by
//     MaxDeliver), so a dead endpoint is acked rather than churned.
//
// The consumer's MaxDeliver:5 (set in ConsumeAllDeliveryDurable) bounds the
// store-error nak retries AND backstops a genuine crash-loop (the handler
// goroutine panicking or the process dying mid-event, before an ack/nak is
// sent). There is deliberately no dead-letter queue: the terminal failure
// paths (poison, dead endpoint) already ack, so the only messages that can
// reach the 5-delivery cap are a store that stays broken across five
// redeliveries or a crash-loop — both scenarios MaxDeliver exists to bound
// rather than steady-state modes this package needs to surface further.
package webhooksout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/store"
)

const (
	maxAttempts = 3
	retryDelay  = 50 * time.Millisecond

	// durableName is the JetStream durable consumer name Start resumes
	// across process restarts. Shared by every notify instance/replica
	// consuming delivery-status events for outbound webhooks — do not
	// change without a migration plan, since renaming it starts a fresh
	// consumer with no memory of what was already acked.
	durableName = "notify-webhooksout"
)

// endpointLister is the slice of the store Consumer depends on: resolving the
// active webhook endpoints for a scope. Narrowing to this interface (rather
// than holding *store.Store) lets tests inject a store that fails transiently
// to exercise the nak/redeliver path. *store.WebhookEndpointRepo satisfies it.
type endpointLister interface {
	ListActiveByEnv(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error)
}

// Consumer fans delivery-status events out to per-scope webhook endpoints.
type Consumer struct {
	bus       *eventbus.Bus
	endpoints endpointLister
	client    *http.Client
}

// New builds a Consumer bound to bus and st with a short-timeout, SSRF-safe
// HTTP client: endpoint URLs come from tenant-configured webhook rows, so a
// plain client would let a tenant point delivery-status fan-out at an internal
// host.
func New(bus *eventbus.Bus, st *store.Store) *Consumer {
	return NewWithClient(bus, st, httpaction.NewSSRFSafeClientWithTimeout(5*time.Second))
}

// NewWithClient builds a Consumer bound to bus and st using the provided HTTP
// client. Callers are responsible for applying any security policy on client;
// this constructor is primarily for tests that need a bypass (e.g. to reach an
// httptest server on loopback, which the SSRF-safe client from New rejects by
// design).
func NewWithClient(bus *eventbus.Bus, st *store.Store, client *http.Client) *Consumer {
	c := &Consumer{bus: bus, client: client}
	// Guard nil: some tests construct a Consumer purely to inspect its HTTP
	// client (SSRF policy) and pass a nil store. dispatch is never reached in
	// that path, so leaving endpoints nil is safe.
	if st != nil {
		c.endpoints = st.WebhookEndpoints()
	}
	return c
}

// Start consumes every environment's delivery-status events off the durable
// notify-webhooksout JetStream consumer (see the package doc comment for the
// ack/nak policy). For each event it parses the tenancy scope from the
// message JSON, looks up the active webhook endpoints for that scope, and
// POSTs the raw event bytes to each endpoint URL with a small bounded retry
// on error/non-2xx. The returned stop func drains the consumer.
func (c *Consumer) Start(ctx context.Context) (stop func() error, err error) {
	stop, err = c.bus.ConsumeAllDeliveryDurable(ctx, durableName, func(data []byte) error {
		return c.dispatch(ctx, data)
	})
	if err != nil {
		return nil, fmt.Errorf("webhooksout: Start: %w", err)
	}
	return stop, nil
}

// dispatch resolves the scope from the event and POSTs to each active
// endpoint. It returns nil to ack and a non-nil error to nak — see the
// package doc comment for the ack/nak asymmetry.
func (c *Consumer) dispatch(ctx context.Context, data []byte) error {
	var hdr struct {
		OrgID string
		EnvID string
	}
	if err := json.Unmarshal(data, &hdr); err != nil {
		// Poison event: terminal, so ack — redelivery can't help and must
		// not wedge the shared durable.
		return nil
	}
	scope := domain.Scope{OrgID: hdr.OrgID, EnvID: hdr.EnvID}

	endpoints, err := c.endpoints.ListActiveByEnv(ctx, scope)
	if err != nil {
		// Transient store error (DB busy / lock contention): nak so
		// JetStream redelivers (bounded by MaxDeliver:5) rather than
		// silently dropping the event. Naking one message does not
		// head-of-line-block the rest of the stream.
		return fmt.Errorf("webhooksout: list active endpoints: %w", err)
	}
	for _, e := range endpoints {
		c.post(ctx, e.URL, data)
	}
	return nil
}

// post sends data as application/json to url, retrying up to maxAttempts on
// transport error or non-2xx response.
func (c *Consumer) post(ctx context.Context, url string, data []byte) {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return
		}
	}
}
