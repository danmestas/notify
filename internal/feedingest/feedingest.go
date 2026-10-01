// Package feedingest accepts authenticated webhook-push event batches from
// external producers and publishes them into the environment's durable feed
// (NOTIFY_FEED), where the existing feedapi SSE bridge serves them to
// consumers. It is the inbound mirror of notify's outbound provider catalog:
// an external producer POSTs to /v1/sources/{sourceID}/events with a per-source
// SourceKey secret and its events land on the same durable stream the "stream"
// step type publishes to.
//
// Payload shapes per feed topic: workflow-produced events are domain.Message
// JSON ({"channel":"stream",...}); ingested events are the versioned envelope
// {"v":1,"source_id":"<ULID>","event_id":"...","ts":"<RFC3339>","data":<opaque
// JSON>}. Consumers distinguish the two by the "v" key.
//
// Request-shaped ingest only: this package holds no connections to upstream
// systems, no upstream credentials, and no schedulers or background goroutines,
// by design. Every request is authenticated, limited, normalized, and
// published within the handler; nothing outlives it.
package feedingest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/streamsource"
)

// maxRequestBytes caps the whole POST body before any read: the per-event and
// per-batch caps are applied after decode, but this stops an oversized body
// from ever being buffered. Overflow is answered 413.
const maxRequestBytes = 1 << 20 // 1 MiB

// maxEventIDLen bounds a producer-supplied event id. The id becomes part of a
// Nats-Msg-Id header (src.ID + "/" + id), so it is both length- and
// charset-bounded before it can reach the wire.
const maxEventIDLen = 128

// envelopeVersion is the schema version stamped into every ingested envelope's
// "v" field (plan §0.4). Consumers use it to distinguish ingested events from
// workflow-produced domain.Message JSON.
const envelopeVersion = 1

// sourceKeyPrefix is the Authorization scheme for the ingest endpoint. It is
// deliberately distinct from identity's "ApiKey ": a producer is handed
// push-only SourceKey credentials that never work on /v1/* and a tenant's
// console ApiKey never works here.
const sourceKeyPrefix = "SourceKey "

// Authenticator resolves an inbound source secret to its owning source.
// *streamsource.Service satisfies this.
type Authenticator interface {
	Authenticate(ctx context.Context, presentedSecret string) (domain.StreamSource, error)
}

// SourceToucher records best-effort ingest activity (fire-and-forget; no
// return). *streamsource.Service satisfies this.
type SourceToucher interface {
	TouchActivity(ctx context.Context, id, lastEventAt, lastError string)
}

// Gateway is the ingest endpoint. Unlike delivery (which sits BELOW eventbus in
// the dependency order and uses a narrow FeedPublisher interface), feedingest
// sits ABOVE eventbus exactly like feedapi, so it holds *eventbus.Bus directly
// for PublishFeedBatch/BudgetAllow (no import cycle: eventbus imports only
// domain). The streamsource seams stay narrow interfaces for testability.
type Gateway struct {
	src   Authenticator
	touch SourceToucher
	bus   *eventbus.Bus
}

// New builds a Gateway. The concrete *streamsource.Service satisfies both the
// Authenticator and SourceToucher seams.
func New(src *streamsource.Service, bus *eventbus.Bus) *Gateway {
	return &Gateway{src: src, touch: src, bus: bus}
}

// Routes mounts the ingest endpoint. It is mounted OUTSIDE identity.Middleware
// (like /v1/inbound/email): the handler authenticates the SourceKey itself, so
// a producer needs no console ApiKey.
func (g *Gateway) Routes(r chi.Router) {
	r.Post("/v1/sources/{sourceID}/events", g.handleIngest)
}

// ingestRequest is the POST /v1/sources/{sourceID}/events body.
type ingestRequest struct {
	Topic  string        `json:"topic"`
	Events []ingestEvent `json:"events"`
}

// ingestEvent is one event in an ingest batch. ID and TS are optional producer
// hints; Data is the opaque JSON payload.
type ingestEvent struct {
	ID   string          `json:"id"`
	TS   string          `json:"ts"`
	Data json.RawMessage `json:"data"`
}

// envelope is the versioned wire shape ingested events are published as
// (plan §0.4). It is mechanically distinguishable from domain.Message JSON by
// the "v" field.
type envelope struct {
	V        int             `json:"v"`
	SourceID string          `json:"source_id"`
	EventID  string          `json:"event_id"`
	TS       string          `json:"ts"`
	Data     json.RawMessage `json:"data"`
}

// ingestResponse is the 202/502 body: how many events were durably accepted.
// SeqHint is always null (ordered consumers observe the sequence; the response
// stays minimal).
type ingestResponse struct {
	Accepted int    `json:"accepted"`
	SeqHint  *int   `json:"seq_hint"`
	Error    string `json:"error,omitempty"`
}

// handleIngest runs the fail-closed ingest pipeline in a fixed order — body cap,
// auth, shape caps, topic gate, rate limit, byte budget, normalize, publish,
// touch. Every rejection short-circuits BEFORE PublishFeedBatch, so a rejected
// request publishes nothing.
func (g *Gateway) handleIngest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. Body cap first, before any read.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	// 2. Auth: SourceKey scheme only. All auth failures answer a uniform 401
	// (no oracle); a valid secret whose source id does not match the path is a
	// 404 (a valid credential must not confirm other source ids); an inactive
	// source authenticates then rejects with an attributable 403.
	key, ok := parseSourceKey(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	src, err := g.src.Authenticate(ctx, key)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if src.ID != chi.URLParam(r, "sourceID") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !src.Active {
		http.Error(w, "source inactive", http.StatusForbidden)
		return
	}

	// 3. Decode + shape caps. Includes the per-event ts format check: a
	// malformed ts must 400 here, BEFORE the rate-limit/byte-budget
	// BudgetAllow gates below record a spend for this request.
	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(req.Events) == 0 {
		http.Error(w, "events must be a non-empty array", http.StatusBadRequest)
		return
	}
	if len(req.Events) > src.MaxBatchEvents {
		http.Error(w, "batch exceeds max_batch_events", http.StatusRequestEntityTooLarge)
		return
	}
	var totalDataBytes int64
	for i := range req.Events {
		ev := req.Events[i]
		if len(ev.Data) == 0 || !json.Valid(ev.Data) {
			http.Error(w, "event data must be valid JSON", http.StatusBadRequest)
			return
		}
		if len(ev.Data) > src.MaxEventBytes {
			http.Error(w, "event data exceeds max_event_bytes", http.StatusRequestEntityTooLarge)
			return
		}
		if ev.ID != "" && !validEventID(ev.ID) {
			http.Error(w, "event id too long or contains disallowed characters", http.StatusBadRequest)
			return
		}
		if ev.TS != "" {
			if _, perr := time.Parse(time.RFC3339, ev.TS); perr != nil {
				http.Error(w, "event ts must be RFC3339", http.StatusBadRequest)
				return
			}
		}
		totalDataBytes += int64(len(ev.Data))
	}

	// 4. Topic gate: the request topic must be the source's topic or extend it
	// as a dot-prefix, and pass ValidateFeedTopic. envID comes ONLY from the
	// source row, never the request.
	topic := req.Topic
	if topic == "" {
		topic = src.Topic
	}
	if err := domain.ValidateFeedTopic(topic); err != nil ||
		(topic != src.Topic && !strings.HasPrefix(topic, src.Topic+".")) {
		g.touch.TouchActivity(ctx, src.ID, nowRFC3339(), "invalid topic")
		http.Error(w, "topic must equal or extend the source topic", http.StatusBadRequest)
		return
	}

	// 5. Rate limit (events/min), cost-weighted by batch size. A KV error fails
	// closed with 503; a denial is 429 + Retry-After.
	allowed, err := g.bus.BudgetAllow("ingestevt:"+src.ID, int64(len(req.Events)), int64(src.RateLimitPerMin), time.Minute)
	if err != nil {
		http.Error(w, "ingest limiter unavailable", http.StatusServiceUnavailable)
		return
	}
	if !allowed {
		g.touch.TouchActivity(ctx, src.ID, nowRFC3339(), "rate limit exceeded")
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// 6. Byte budget (bytes/hour). A KV error fails closed with 503; a denial is
	// 429 + Retry-After.
	allowed, err = g.bus.BudgetAllow("ingestbytes:"+src.ID, totalDataBytes, src.ByteBudget, time.Hour)
	if err != nil {
		http.Error(w, "ingest limiter unavailable", http.StatusServiceUnavailable)
		return
	}
	if !allowed {
		g.touch.TouchActivity(ctx, src.ID, nowRFC3339(), "byte budget exceeded")
		w.Header().Set("Retry-After", "3600")
		http.Error(w, "byte budget exceeded", http.StatusTooManyRequests)
		return
	}

	// 7. Normalize each event into the §0.4 envelope. A producer id yields a
	// dedup MsgID (src.ID+"/"+id); a missing id is minted (no dedup). ts was
	// already format-validated in step 3; a missing ts is stamped now (UTC).
	feedEvents := make([]eventbus.FeedEvent, 0, len(req.Events))
	for i := range req.Events {
		ev := req.Events[i]
		eventID := ev.ID
		if eventID == "" {
			eventID = store.NewULID()
		}
		ts := ev.TS
		if ts == "" {
			ts = nowRFC3339()
		}
		data, merr := json.Marshal(envelope{
			V:        envelopeVersion,
			SourceID: src.ID,
			EventID:  eventID,
			TS:       ts,
			Data:     ev.Data,
		})
		if merr != nil {
			http.Error(w, "failed to encode event", http.StatusInternalServerError)
			return
		}
		fe := eventbus.FeedEvent{Data: data}
		if ev.ID != "" {
			fe.MsgID = src.ID + "/" + ev.ID
		}
		feedEvents = append(feedEvents, fe)
	}

	// 8. Publish. Full success → 202; a broker error mid-batch → 502 with the
	// count durably accepted (the producer retries the whole batch; dedup ids
	// make that safe).
	n, err := g.bus.PublishFeedBatch(src.EnvID, topic, feedEvents)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ingestResponse{Accepted: n, Error: "publish failed"})
		return
	}

	// 9. Touch best-effort on success, then answer 202.
	g.touch.TouchActivity(ctx, src.ID, nowRFC3339(), "")
	writeJSON(w, http.StatusAccepted, ingestResponse{Accepted: n})
}

// parseSourceKey extracts the hex secret from an `Authorization: SourceKey
// <hex>` header, mirroring identity.Middleware's ApiKey parse. ok is false for a
// missing header, a wrong scheme, or an empty key.
func parseSourceKey(r *http.Request) (key string, ok bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, sourceKeyPrefix) {
		return "", false
	}
	key = strings.TrimSpace(strings.TrimPrefix(header, sourceKeyPrefix))
	if key == "" {
		return "", false
	}
	return key, true
}

// validEventID reports whether a producer event id is within the length bound
// and uses only the feed-topic token charset ([A-Za-z0-9_-]) — it becomes a
// Nats-Msg-Id header, so it is charset-gated the same way a subject token is.
func validEventID(id string) bool {
	if len(id) > maxEventIDLen {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// nowRFC3339 renders the current instant in UTC RFC3339 — the format the
// envelope ts and the source's LastEventAt use.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
