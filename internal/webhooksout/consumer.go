// Package webhooksout consumes notify's delivery-status events off the event
// bus and POSTs each raw event to the active outbound webhook endpoints
// configured for the event's tenancy scope (OrgID/EnvID).
//
// Start rides eventbus.Bus.ConsumeAllDeliveryDurable, a durable JetStream
// consumer: events published while no consumer is running (a notify restart,
// or an outage longer than the old core-NATS subscription's window) are
// still delivered once a consumer resumes — at-least-once, not at-most-once.
//
// # Outbox
//
// The durable handler does not POST. It writes one webhook_deliveries row per
// (endpoint, event) and acks; a worker drains the rows. That split is what
// makes retry per endpoint (#346): each row carries its own next-attempt
// clock, so an endpoint that is down for an hour backs off on its own while
// every other endpoint keeps receiving, and nothing is re-sent to an endpoint
// that already accepted the event. Rows survive a restart, so the retries do
// too. Naking the JetStream message instead would redeliver the event to
// every endpoint in the scope on one shared clock.
//
// # Ack/nak policy
//
// dispatch acks or naks per an asymmetry keyed on whether a retry could ever
// help:
//
//   - malformed JSON / scope-parse error: ACK. A poison event is terminal —
//     redelivering identical bytes will fail identically — so acking drops
//     it rather than letting it wedge the shared durable for every tenant
//     behind it.
//   - store error (listing endpoints, writing the outbox): NAK. This is a
//     transient class — DB busy / lock contention — that a redelivery is
//     likely to clear, and it is exactly what JetStream's MaxDeliver:5 /
//     AckWait exist to survive. Naking one message does NOT head-of-line-
//     block the stream: default MaxAckPending (~1000) lets the rest keep
//     flowing while this one backs off and is redelivered.
//   - outbox rows written: ACK. From here the worker owns delivery.
//
// # Worker
//
// Each due row gets one POST. On success the row is deleted. On failure the
// row is rescheduled with exponential backoff (firstDelay doubling to
// maxDelay) until maxAttempts, then dropped with an error log. An endpoint
// that has been deleted or disabled drops its pending rows. Every attempt is
// signed afresh, so a retry never carries an earlier attempt's signature.
package webhooksout

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

const (
	// maxAttempts bounds the POSTs one delivery gets before it is dropped.
	// With the default backoff that is about an hour and three quarters of
	// trying, long enough to ride out a consumer's deploy or restart.
	maxAttempts = 12
	firstDelay  = 5 * time.Second
	maxDelay    = 30 * time.Minute

	// tick bounds how late a due row can be picked up when nothing woke the
	// worker; dispatch wakes it directly, so a fresh event does not wait.
	tick = 5 * time.Second
	// claimLease is how long a claimed row is invisible to other workers.
	// Longer than the HTTP client's timeout so an in-flight attempt is never
	// duplicated by a second worker.
	claimLease = time.Minute
	dueBatch   = 100

	// durableName is the JetStream durable consumer name Start resumes
	// across process restarts. Shared by every notify instance/replica
	// consuming delivery-status events for outbound webhooks — do not
	// change without a migration plan, since renaming it starts a fresh
	// consumer with no memory of what was already acked.
	durableName = "notify-webhooksout"
)

// endpointStore is the slice of the store Consumer depends on. Narrowing to
// this interface (rather than holding *store.Store) lets tests inject a store
// that fails transiently to exercise the nak/redeliver path.
// *store.WebhookEndpointRepo satisfies it.
type endpointStore interface {
	ListActiveByEnv(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error)
	GetByID(ctx context.Context, scope domain.Scope, id string) (domain.WebhookEndpoint, error)
}

// Consumer fans delivery-status events out to per-scope webhook endpoints.
type Consumer struct {
	bus        *eventbus.Bus
	endpoints  endpointStore
	deliveries store.WebhookDeliveryRepo
	client     *http.Client
	// box opens each endpoint's sealed signing secret. nil means every
	// endpoint is delivered unsigned, which is the pre-#346 behaviour and
	// what tests without a master key get.
	box *secretbox.Box

	// wake nudges the worker after an enqueue so the first attempt is
	// immediate rather than up to a tick late.
	wake chan struct{}
	// backoff and tick are fields so tests can shrink minutes to
	// milliseconds; production uses the package constants.
	backoff     func(attempt int) time.Duration
	tick        time.Duration
	maxAttempts int
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
	c := &Consumer{
		bus:         bus,
		client:      client,
		wake:        make(chan struct{}, 1),
		backoff:     defaultBackoff,
		tick:        tick,
		maxAttempts: maxAttempts,
	}
	// Guard nil: some tests construct a Consumer purely to inspect its HTTP
	// client (SSRF policy) and pass a nil store. dispatch is never reached in
	// that path, so leaving the repos nil is safe.
	if st != nil {
		c.endpoints = st.WebhookEndpoints()
		c.deliveries = st.WebhookDeliveries()
	}
	return c
}

// WithSecrets attaches the secretbox that opens endpoint signing secrets.
func (c *Consumer) WithSecrets(box *secretbox.Box) *Consumer {
	c.box = box
	return c
}

// defaultBackoff is firstDelay doubling per failed attempt, capped at
// maxDelay: 5s, 10s, 20s, ... 30m.
func defaultBackoff(attempt int) time.Duration {
	d := firstDelay
	for i := 1; i < attempt && d < maxDelay; i++ {
		d *= 2
	}
	if d > maxDelay {
		d = maxDelay
	}
	return d
}

// Signing headers. The scheme is the one the first consumer already speaks:
// the signature covers "<timestamp>.<body>" so a captured event cannot be
// replayed later with the same signature, and the timestamp is sent alongside
// so the receiver can bound how old an event it will accept.
const (
	signatureHeader = "X-Notify-Signature"
	timestampHeader = "X-Notify-Timestamp"
)

// sign returns the X-Notify-Signature value for body at ts under secret:
// sha256=hex(HMAC-SHA256(secret, ts + "." + body)).
func sign(secret string, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Start consumes every environment's delivery-status events off the durable
// notify-webhooksout JetStream consumer and starts the outbox worker. The
// returned stop func drains the consumer and waits for the worker.
func (c *Consumer) Start(ctx context.Context) (stop func() error, err error) {
	busStop, err := c.bus.ConsumeAllDeliveryDurable(ctx, durableName, func(data []byte) error {
		return c.dispatch(ctx, data)
	})
	if err != nil {
		return nil, fmt.Errorf("webhooksout: Start: %w", err)
	}
	wctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.run(wctx)
	}()
	return func() error {
		err := busStop()
		cancel()
		wg.Wait()
		return err
	}, nil
}

// dispatch resolves the scope from the event and enqueues one delivery per
// active endpoint. It returns nil to ack and a non-nil error to nak — see the
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
	if len(endpoints) == 0 {
		return nil
	}
	now := timestamp.Now()
	rows := make([]store.WebhookDelivery, 0, len(endpoints))
	for _, e := range endpoints {
		rows = append(rows, store.WebhookDelivery{
			OrgID: scope.OrgID, EnvID: scope.EnvID, EndpointID: e.ID,
			Payload: data, NextAttemptAt: now,
		})
	}
	if err := c.deliveries.Enqueue(ctx, rows); err != nil {
		return fmt.Errorf("webhooksout: enqueue deliveries: %w", err)
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

// run drains the outbox whenever woken or on every tick until ctx ends.
func (c *Consumer) run(ctx context.Context) {
	t := time.NewTicker(c.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-t.C:
		}
		c.drain(ctx)
	}
}

// drain attempts every due row once.
func (c *Consumer) drain(ctx context.Context) {
	for ctx.Err() == nil {
		due, err := c.deliveries.Due(ctx, timestamp.Now(), dueBatch)
		if err != nil {
			slog.Default().Error("webhooksout: read due deliveries", "err", err)
			return
		}
		for _, d := range due {
			if ctx.Err() != nil {
				return
			}
			c.deliver(ctx, d)
		}
		if len(due) < dueBatch {
			return
		}
	}
}

// deliver makes one attempt for d: claim, resolve the endpoint, POST, then
// delete or reschedule.
func (c *Consumer) deliver(ctx context.Context, d store.WebhookDelivery) {
	now := time.Now()
	ok, err := c.deliveries.Claim(ctx, d.ID, timestamp.Format(now), timestamp.Format(now.Add(claimLease)))
	if err != nil {
		slog.Default().Error("webhooksout: claim delivery", "id", d.ID, "err", err)
		return
	}
	if !ok {
		return
	}
	scope := d.Scope()
	ep, err := c.endpoints.GetByID(ctx, scope, d.EndpointID)
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && !ep.Active:
		// The endpoint is gone or switched off: nothing wants this event.
		c.drop(ctx, d.ID)
		return
	case err != nil:
		// Transient store error: try again on the next tick, no attempt
		// charged.
		c.reschedule(ctx, d.ID, d.Attempts, now.Add(c.tick))
		return
	}
	secret, ok := c.secretFor(ep)
	if !ok {
		// A sealed secret this process cannot open is a master-key
		// mismatch. Sending unsigned instead would be rejected by any
		// receiver that verifies, so say why and keep the row for a
		// process that can open it.
		slog.Default().Error("webhooksout: cannot open endpoint signing secret",
			"endpoint", ep.ID, "url", ep.URL)
		c.fail(ctx, d, now)
		return
	}
	if c.post(ctx, ep.URL, secret, d.Payload) {
		c.drop(ctx, d.ID)
		return
	}
	c.fail(ctx, d, now)
}

// fail charges one attempt to d and either reschedules it with backoff or,
// past maxAttempts, drops it loudly.
func (c *Consumer) fail(ctx context.Context, d store.WebhookDelivery, now time.Time) {
	attempts := d.Attempts + 1
	if attempts >= c.maxAttempts {
		slog.Default().Error("webhooksout: giving up on delivery",
			"endpoint", d.EndpointID, "attempts", attempts, "org", d.OrgID, "env", d.EnvID)
		c.drop(ctx, d.ID)
		return
	}
	c.reschedule(ctx, d.ID, attempts, now.Add(c.backoff(attempts)))
}

func (c *Consumer) drop(ctx context.Context, id string) {
	if err := c.deliveries.Delete(ctx, id); err != nil {
		slog.Default().Error("webhooksout: delete delivery", "id", id, "err", err)
	}
}

func (c *Consumer) reschedule(ctx context.Context, id string, attempts int, at time.Time) {
	if err := c.deliveries.Reschedule(ctx, id, attempts, timestamp.Format(at)); err != nil {
		slog.Default().Error("webhooksout: reschedule delivery", "id", id, "err", err)
	}
}

// secretFor opens the endpoint's sealed secret. ("", true) is an unsigned
// endpoint -- no secret stored, or no box to open one.
func (c *Consumer) secretFor(e domain.WebhookEndpoint) (string, bool) {
	if e.Secret == "" || c.box == nil {
		return "", true
	}
	plain, err := c.box.Open(e.Secret)
	if err != nil {
		return "", false
	}
	return plain, true
}

// post sends data as application/json to url once, signed when secret is
// non-empty, and reports whether the endpoint accepted it (2xx).
func (c *Consumer) post(ctx context.Context, url, secret string, data []byte) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req.Header.Set(timestampHeader, ts)
		req.Header.Set(signatureHeader, sign(secret, ts, data))
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
