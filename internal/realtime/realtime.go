// Package realtime exposes notify's live inbox over Server-Sent Events.
// The gateway bridges per-subscriber eventbus subscriptions to long-lived
// HTTP streams so browser clients receive inbox events as they are published.
package realtime

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
)

// Gateway streams a subscriber's live inbox events to HTTP clients via SSE,
// fanning out from the event bus to one stream per request.
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
	r.Get("/v1/inbox/stream", g.handleStream)
}

// handleStream serves GET /v1/inbox/stream?subscriberId=... as an SSE stream.
// It subscribes to the (env, subscriber) inbox subject and writes each event
// as a "data: <payload>\n\n" frame until the client disconnects.
func (g *Gateway) handleStream(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "missing tenancy scope", http.StatusUnauthorized)
		return
	}

	subscriberID := r.URL.Query().Get("subscriberId")
	if subscriberID == "" {
		http.Error(w, "subscriberId is required", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Buffered so the bus callback never blocks; a slow client just drops
	// events rather than stalling the NATS delivery goroutine.
	events := make(chan []byte, 16)
	unsub, err := g.bus.SubscribeInbox(scope.EnvID, subscriberID, func(d []byte) {
		select {
		case events <- d:
		default:
		}
	})
	if err != nil {
		http.Error(w, "subscribe failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = unsub() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-events:
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := w.Write(data); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
