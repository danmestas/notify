// Package inboxapi serves the in-app message feed: listing a subscriber's
// messages (mirroring the reference messages.controller GET “ endpoint) and marking
// them seen/read/archived, all scoped to the tenant carried on the request
// context.
package inboxapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// Service exposes the in-app message feed over HTTP.
type Service struct {
	store *store.Store
}

// New builds a Service backed by the given store.
func New(s *store.Store) *Service {
	return &Service{store: s}
}

// Routes registers the feed endpoints on r. It expects to run behind
// identity.Middleware so the request context carries a tenancy Scope.
func (svc *Service) Routes(r chi.Router) {
	r.Get("/v1/messages", svc.listMessages)
	r.Get("/v1/messages/count", svc.countMessages)
	r.Post("/v1/messages/mark", svc.markMessagesBulk)
	r.Post("/v1/messages/{id}/{action}", svc.markMessage)
}

const (
	listDefaultLimit = 50
	listMaxLimit     = 200
)

// listMessages handles GET /v1/messages.
//
// When ?integrationId= is provided (Wave-4 #42), it returns up to 100
// messages for that integration (newest first) as a bare JSON array — the
// "activity tab" filter, unrelated to the subscriber feed below and
// unchanged by the envelope work.
//
// Otherwise this is the subscriber inbox feed. By default it returns the
// paginated envelope:
//
//	{"data":[...], "next":"<id>", "unreadCount":n, "unseenCount":n}
//
// Query params: subscriberId, channel (defaults to "in_app" when omitted —
// the feed must never silently widen to every channel's delivery records),
// transactionId (narrows the page to one transaction), limit (default 50,
// max 200), before (a message id boundary — only messages older than this
// id are returned), archived (true|false|all; default excludes archived
// messages from the feed and from unreadCount/unseenCount, mirroring
// the reference platform's inbox — an archived message is "done", not merely hidden). "next"
// is the id to pass as the following page's "before", omitted (empty) once
// the feed is exhausted.
//
// The pre-envelope bare-array shape stays available for existing callers
// that filter by subscriberId/channel/transactionId: send an Accept header
// whose media type (ignoring parameters like ";q=") is
// "application/vnd.notify.v1+json" — including within a comma-separated
// Accept list — or pass "?format=array", to opt back into it. This is a
// deliberate, permanent compatibility switch, not a migration window —
// pick whichever mechanism is convenient. Note the bare-array branch does
// NOT default an omitted channel; it is an exact match on the legacy
// column filter, unchanged from its pre-envelope behavior.
func (svc *Service) listMessages(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	q := r.URL.Query()

	// Wave-4 #42: integrationId filter takes priority.
	if integrationID := q.Get("integrationId"); integrationID != "" {
		msgs, err := svc.store.Messages().ListByIntegrationID(r.Context(), scope, integrationID, 0)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if msgs == nil {
			msgs = []domain.Message{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msgs)
		return
	}

	subscriberID := q.Get("subscriberId")
	channel := q.Get("channel")
	transactionID := q.Get("transactionId")

	if q.Get("format") == "array" || acceptsCompatArray(r.Header.Get("Accept")) {
		msgs, err := svc.store.Messages().List(r.Context(), scope, subscriberID, channel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := msgs
		if transactionID != "" {
			out = out[:0]
			for _, m := range msgs {
				if m.TransactionID == transactionID {
					out = append(out, m)
				}
			}
		}
		if out == nil {
			out = []domain.Message{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}

	limit := listDefaultLimit
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	if limit > listMaxLimit {
		limit = listMaxLimit
	}
	before := q.Get("before")
	archived := q.Get("archived")

	// Default channel to "in_app" on the envelope path when omitted. The
	// legacy array branch above filters by an exact (possibly empty)
	// channel match, so an omitted channel there already returns nothing
	// rather than every channel; ListPage/Count treat "" as "no channel
	// condition", so leaving it empty here would surface delivery records
	// — email/SMS/push, including rendered content — for every channel
	// instead of just the in-app feed.
	if channel == "" {
		channel = "in_app"
	}

	// Fetch one extra row so we can tell whether a next page exists without
	// a second query.
	msgs, err := svc.store.Messages().ListPage(r.Context(), scope, subscriberID, channel, before, limit+1, archived, transactionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var next string
	if len(msgs) > limit {
		msgs = msgs[:limit]
		// next anchors on the last row actually returned, not the extra
		// lookahead row: the following page asks for "id < next", which must
		// exclude exactly the rows already shown, not one row further back.
		next = msgs[limit-1].ID
	}
	if msgs == nil {
		msgs = []domain.Message{}
	}

	// unreadCount/unseenCount always exclude archived messages ("" here, not
	// the page's own archived filter): archiving is the "done" action, and
	// a message the caller has triaged out of the feed must stop counting
	// toward the badge even while viewing the archived tab.
	unreadCount, err := svc.store.Messages().Count(r.Context(), scope, subscriberID, channel, true, false, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	unseenCount, err := svc.store.Messages().Count(r.Context(), scope, subscriberID, channel, false, true, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type envelope struct {
		Data        []domain.Message `json:"data"`
		Next        string           `json:"next,omitempty"`
		UnreadCount int              `json:"unreadCount"`
		UnseenCount int              `json:"unseenCount"`
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(envelope{
		Data:        msgs,
		Next:        next,
		UnreadCount: unreadCount,
		UnseenCount: unseenCount,
	})
}

// acceptsCompatArray reports whether an Accept header opts into the legacy
// bare-array response shape. It parses the header as a comma-separated list
// of media ranges, splits each entry on ';' to drop parameters (e.g.
// "q=1"), and trims whitespace before comparing the media type — so both
// "application/vnd.notify.v1+json" and
// "application/vnd.notify.v1+json;q=1" match, as does that type appearing
// anywhere in a multi-value Accept list.
func acceptsCompatArray(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		mediaType, _, _ := strings.Cut(part, ";")
		if strings.TrimSpace(mediaType) == "application/vnd.notify.v1+json" {
			return true
		}
	}
	return false
}

// countMessages handles
// GET /v1/messages/count?subscriberId=&channel=&unread=true&unseen=true&archived=.
// The cheap badge query: {"count": n}. unread=true narrows to read=false
// rows, unseen=true to seen=false rows (both may be combined); with neither
// set it counts every message matching subscriberId/channel that also
// passes the archived filter. archived defaults to excluding archived rows
// (true|false|all), matching listMessages. channel defaults to "in_app"
// when omitted, for the same data-exposure reason as listMessages's
// envelope path: otherwise this would count delivery records across every
// channel instead of just the in-app feed.
func (svc *Service) countMessages(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	// Default channel to "in_app" when omitted — same reasoning as
	// listMessages's envelope path: an omitted channel here would
	// otherwise count delivery records across every channel, not just the
	// in-app feed.
	channel := q.Get("channel")
	if channel == "" {
		channel = "in_app"
	}
	n, err := svc.store.Messages().Count(
		r.Context(), scope, q.Get("subscriberId"), channel,
		q.Get("unread") == "true", q.Get("unseen") == "true", q.Get("archived"),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"count": n})
}

// markSpec names the flag column an action sets and the value it sets it to.
type markSpec struct {
	field string
	value bool
}

// singleMarkActions maps the {action} path segment of
// POST /v1/messages/{id}/{action} to the flag it sets. There is no "unseen"
// action: once seen, a message stays seen, mirroring the reference platform.
var singleMarkActions = map[string]markSpec{
	"seen":      {"seen", true},
	"read":      {"read", true},
	"unread":    {"read", false},
	"archive":   {"archived", true},
	"unarchive": {"archived", false},
}

// markRequest is the POST /v1/messages/{id}/{action} body. subscriberId is
// required: it is the caller's assertion of which subscriber's inbox it is
// acting on, and is checked against the message's actual subscriber_id so
// one subscriber can never mark another's message. A mismatch (or an id
// that doesn't exist in this org/env) answers 404, not 403, so the response
// can't be used to probe for the existence of another subscriber's
// messages.
type markRequest struct {
	SubscriberID string `json:"subscriberId"`
}

// markMessage handles POST /v1/messages/{id}/{seen|read|unread|archive|unarchive}.
func (svc *Service) markMessage(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	spec, ok := singleMarkActions[chi.URLParam(r, "action")]
	if !ok {
		http.Error(w, "unknown action; allowed: seen, read, unread, archive, unarchive", http.StatusBadRequest)
		return
	}

	var req markRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.SubscriberID == "" {
		http.Error(w, "subscriberId is required", http.StatusBadRequest)
		return
	}

	id := chi.URLParam(r, "id")
	err := svc.store.Messages().MarkOne(r.Context(), scope, id, req.SubscriberID, spec.field, spec.value)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bulkMarkActions maps the "as" field of the POST /v1/messages/mark body to
// the flag it sets. Unlike singleMarkActions, the inverses are spelled
// adjectivally ("unread", "unarchived") to read naturally as a target state
// rather than a verb.
var bulkMarkActions = map[string]markSpec{
	"seen":       {"seen", true},
	"read":       {"read", true},
	"unread":     {"read", false},
	"archived":   {"archived", true},
	"unarchived": {"archived", false},
}

// markMessagesBulkRequest is the POST /v1/messages/mark body:
//
//	{"subscriberId":"...", "ids":["m1","m2"], "as":"read"}
//	{"subscriberId":"...", "all":true, "as":"archived"}
//
// subscriberId is required for the same reason as markRequest.SubscriberID.
// Exactly one of ids/all selects the target set; ids belonging to a
// different subscriber are silently excluded rather than erroring, so a
// caller can safely bulk-mark a mixed id list without leaking which ids
// were somebody else's.
type markMessagesBulkRequest struct {
	SubscriberID string   `json:"subscriberId"`
	IDs          []string `json:"ids"`
	All          bool     `json:"all"`
	As           string   `json:"as"`
}

// markMessagesBulk handles POST /v1/messages/mark.
func (svc *Service) markMessagesBulk(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req markMessagesBulkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.SubscriberID == "" {
		http.Error(w, "subscriberId is required", http.StatusBadRequest)
		return
	}
	spec, ok := bulkMarkActions[req.As]
	if !ok {
		http.Error(w, "unknown as; allowed: seen, read, unread, archived, unarchived", http.StatusBadRequest)
		return
	}
	if !req.All && len(req.IDs) == 0 {
		http.Error(w, "ids is required unless all is true", http.StatusBadRequest)
		return
	}

	n, err := svc.store.Messages().MarkBulk(r.Context(), scope, req.SubscriberID, req.IDs, req.All, spec.field, spec.value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"updated": n})
}
