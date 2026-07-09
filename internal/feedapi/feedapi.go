// Package feedapi exposes notify's durable stream-channel feed over
// Server-Sent Events. The gateway bridges per-request eventbus.ConsumeFeed
// subscriptions (backed by a JetStream ordered consumer) to long-lived HTTP
// streams so clients receive backlog-then-live events and can resume
// losslessly across reconnects via Last-Event-ID.
//
// Unlike internal/realtime (whose slow-client policy is drop-on-full, since
// inbox delivery is best-effort), feedapi disconnects a slow client instead
// of dropping events: the durable stream lets the client resume losslessly
// from where it left off, so a silent drop would betray the whole point of
// a durable feed. See handleStream's events channel handling.
package feedapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
)

// eventsBufferSize bounds how far a slow client may lag behind before the
// connection is closed (see package doc).
const eventsBufferSize = 64

// writeDeadline bounds how long a single SSE frame write may block before
// the client is considered stalled and the connection is closed.
const writeDeadline = 30 * time.Second

// pingInterval is how often a comment frame is sent to keep intermediaries
// from buffering the response and to detect dead connections promptly.
const pingInterval = 15 * time.Second

// Gateway streams a tenant's durable stream-channel feed to HTTP clients via
// SSE, bridging eventbus.Bus.ConsumeFeed to one stream per request.
type Gateway struct {
	bus *eventbus.Bus
}

// New returns a Gateway backed by bus.
func New(bus *eventbus.Bus) *Gateway {
	return &Gateway{bus: bus}
}

// Routes mounts the gateway's endpoints. It expects to run behind
// identity.Middleware so the request context carries a domain.Scope.
func (g *Gateway) Routes(r chi.Router) {
	r.Get("/v1/feed/stream", g.handleStream)
}

// handleStream serves GET /v1/feed/stream?topic=<concrete-topic>&from=<seq|RFC3339>
// as an SSE stream over the environment's durable feed. environmentID comes
// ONLY from the authenticated scope, never from a query param. It resumes
// from Last-Event-ID (if present, taking precedence over from=), or from=
// (a stream sequence or RFC3339 time), or starts live-only when neither is
// present.
func (g *Gateway) handleStream(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "missing tenancy scope", http.StatusUnauthorized)
		return
	}

	topic := r.URL.Query().Get("topic")
	if topic == "" {
		http.Error(w, "topic is required", http.StatusBadRequest)
		return
	}
	if err := domain.ValidateFeedTopic(topic); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fromSeq, fromTime, err := resumeFrom(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	// Buffered so ConsumeFeed's callback never blocks; when the buffer would
	// overflow, signal overflow rather than blocking or silently dropping —
	// the SSE loop below closes the connection on that signal so the client
	// resumes losslessly via Last-Event-ID (see package doc).
	events := make(chan eventbus.FeedMsg, eventsBufferSize)
	overflow := make(chan struct{}, 1)
	stop, err := g.bus.ConsumeFeed(ctx, scope.EnvID, topic, fromSeq, fromTime, func(m eventbus.FeedMsg) {
		select {
		case events <- m:
		default:
			select {
			case overflow <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		http.Error(w, "subscribe failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = stop() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	rc := http.NewResponseController(w)

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-overflow:
			// Slow-client policy: disconnect rather than drop silently. The
			// durable stream makes this the correct backpressure valve.
			return
		case m := <-events:
			if err := rc.SetWriteDeadline(time.Now().Add(writeDeadline)); err != nil {
				return
			}
			if _, err := w.Write([]byte("id: " + strconv.FormatUint(m.Seq, 10) + "\ndata: ")); err != nil {
				return
			}
			if _, err := w.Write(m.Data); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if err := rc.SetWriteDeadline(time.Now().Add(writeDeadline)); err != nil {
				return
			}
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// resumeFrom determines the ConsumeFeed start position from the
// Last-Event-ID header (SSE auto-reconnect; wins over from=) or the from=
// query param (a stream sequence or RFC3339 time), or neither (live-only,
// the zero values). Both the header and a numeric from= are "last seen"
// ids per the SSE Last-Event-ID convention, so the returned fromSeq is
// parsed+1 — ConsumeFeed's fromSeq is inclusive-of-start.
func resumeFrom(r *http.Request) (fromSeq uint64, fromTime time.Time, err error) {
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		seq, perr := strconv.ParseUint(last, 10, 64)
		if perr != nil {
			return 0, time.Time{}, fmt.Errorf("malformed Last-Event-ID %q", last)
		}
		return seq + 1, time.Time{}, nil
	}

	from := r.URL.Query().Get("from")
	if from == "" {
		return 0, time.Time{}, nil
	}
	if seq, perr := strconv.ParseUint(from, 10, 64); perr == nil {
		return seq + 1, time.Time{}, nil
	}
	t, perr := time.Parse(time.RFC3339, from)
	if perr != nil {
		return 0, time.Time{}, fmt.Errorf("malformed from=%q: not a sequence or RFC3339 time", from)
	}
	return 0, t, nil
}
