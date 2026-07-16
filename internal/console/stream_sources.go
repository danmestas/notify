package console

// stream_sources.go implements the S5 console "Stream Sources" page under
// /console/stream-sources: the inbound webhook-push mirror of the outbound
// integrations catalog. It manages per-source push secrets (create → one-time
// reveal, rotate → new reveal), topic + limits, an active toggle, and delete.
//
// Lifecycle differences from the integrations page (why this is a FRESH
// template, not a reuse):
//   - the secret is minted server-side and revealed EXACTLY ONCE (identity's
//     reveal-once discipline) — there is no editable credential form;
//   - a source holds no upstream credentials and dials nothing (no verify /
//     test-send);
//   - the row's affordances are limits + activity, not provider metadata.
//
// Every data access goes through Deps.StreamSources (*streamsource.Service),
// which scope-checks every call. The raw secret is never embedded in a
// redirect Location — create/rotate stash it server-side (reveal.go) and
// redirect with an opaque one-shot token instead; the redirect target
// consumes the token exactly once. The raw secret is never logged, never
// listed.
//
// Mutation handlers are native POST→303 (chassis idiom): the browser's own
// submission navigates to the 303 response. They must NOT carry a
// data-on:submit handler (Datastar would preventDefault() the native submit —
// the L5 regression; see busy_submit_test.go).

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/streamsource"
)

// ─── view structs ─────────────────────────────────────────────────────────────

// streamSourceRow is one row in the stream-sources table.
type streamSourceRow struct {
	ID              string
	Name            string
	Topic           string
	MaskedSecret    string
	Active          bool
	RateLimitPerMin int
	MaxEventBytes   int
	MaxBatchEvents  int
	ByteBudget      int64
	LastEventAt     string
	LastError       string
}

// streamSourcesListView binds the #stream-sources-tbody fragment.
type streamSourcesListView struct {
	Rows     []streamSourceRow
	ReadOnly bool
	// ConfirmDelete names the source id awaiting inline two-step delete
	// confirmation (chassis idiom — no native confirm() dialogs).
	ConfirmDelete string
	// EditLimits names the source id whose row is rendered as the inline
	// limits edit form.
	EditLimits string
	// CSRF is the session token plumbed into the row action forms.
	CSRF  string
	Empty *emptyState
}

// streamSourcesPageView is what stream_sources.html binds.
type streamSourcesPageView struct {
	Header   pageHeader
	Fragment streamSourcesListView
	Signals  template.JS
	ReadOnly bool
	CSRF     string
	// NewSecret is the raw hex secret shown exactly once after a create or
	// rotate (empty on every other render). NewSourceID names the source it
	// belongs to (for the copy-ready producer snippet). Rotated distinguishes
	// the reveal card's heading.
	NewSecret   string
	NewSourceID string
	Rotated     bool
	// IngestBase is the origin the one-time producer snippet points at
	// (PublicBaseURL, else the bound HTTPAddr, else the "$BASE" placeholder).
	IngestBase string
	// CreateError carries the post-create failure banner (invalid name/topic,
	// duplicate). The create handler 303-redirects with ?createError= rather
	// than returning a raw 4xx dead-end.
	CreateError string
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// streamSourceToRow builds a display row, masking the secret.
func (c *Console) streamSourceToRow(src domain.StreamSource) streamSourceRow {
	return streamSourceRow{
		ID:              src.ID,
		Name:            src.Name,
		Topic:           src.Topic,
		MaskedSecret:    c.deps.StreamSources.MaskedSecret(src),
		Active:          src.Active,
		RateLimitPerMin: src.RateLimitPerMin,
		MaxEventBytes:   src.MaxEventBytes,
		MaxBatchEvents:  src.MaxBatchEvents,
		ByteBudget:      src.ByteBudget,
		LastEventAt:     src.LastEventAt,
		LastError:       src.LastError,
	}
}

// ingestBase returns the origin the producer snippet points at.
func (c *Console) ingestBase() string {
	if c.deps.PublicBaseURL != "" {
		return c.deps.PublicBaseURL
	}
	if c.deps.HTTPAddr != "" {
		return c.deps.HTTPAddr
	}
	return "$BASE"
}

// streamSourcesSignalsJSON seeds the page's Datastar signal store. newOpen
// gates the create slide-over.
func streamSourcesSignalsJSON(newOpen bool) template.JS {
	b, err := json.Marshal(map[string]any{"streamSourceNewOpen": newOpen})
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// buildStreamSourcesListView lists the scope's sources and assembles the
// fragment binding, honouring the inline confirmDelete / editLimits params.
func (c *Console) buildStreamSourcesListView(r *http.Request) (streamSourcesListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildStreamSourcesListView: no scope in context")
	}
	sources, err := c.deps.StreamSources.List(r.Context(), scope)
	if err != nil {
		return streamSourcesListView{}, err
	}
	rows := make([]streamSourceRow, 0, len(sources))
	for _, src := range sources {
		rows = append(rows, c.streamSourceToRow(src))
	}
	var empty *emptyState
	if len(rows) == 0 {
		empty = &emptyState{
			Icon:        "unplug",
			Title:       "No stream sources yet",
			Description: "Create a source to let an external producer POST event batches into this environment's feed.",
		}
	}
	s, _ := sessionFromContext(r.Context())
	return streamSourcesListView{
		Rows:          rows,
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete"),
		EditLimits:    r.URL.Query().Get("editLimits"),
		CSRF:          s.CSRF,
		Empty:         empty,
	}, nil
}

// ─── page handlers ────────────────────────────────────────────────────────────

// serveStreamSourcesPage renders GET /console/stream-sources. A create or
// rotate 303-redirects here with ?newSecret=&newId= (and ?rotated=1 for a
// rotation) to surface the one-time reveal.
func (c *Console) serveStreamSourcesPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveStreamSourcesPage: w is nil")
	}
	if r == nil {
		panic("serveStreamSourcesPage: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveStreamSourcesPage: no scope in context")
	}

	frag, err := c.buildStreamSourcesListView(r)
	if err != nil {
		c.deps.Logger.Error("console: stream sources page list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Stream Sources",
		Subtitle: "Inbound webhook-push sources. External producers POST event batches into this environment's feed.",
		Tiles: []tile{
			{Label: "sources", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Stream Sources"}
	}

	// A fresh secret (from create/rotate) rides behind a one-shot reveal
	// TOKEN — never the raw secret itself — so the redirect Location,
	// browser history, and any reverse-proxy/CDN access log fronting the
	// console never see the plaintext. The token is consumed on this first
	// GET; a reload (token already consumed) renders nothing.
	newSecret := ""
	if token := r.URL.Query().Get("revealed"); token != "" {
		newSecret, _ = c.takeReveal(token)
	}

	s, _ := sessionFromContext(r.Context())
	view := streamSourcesPageView{
		Header:      header,
		Fragment:    frag,
		Signals:     streamSourcesSignalsJSON(false),
		ReadOnly:    c.deps.ReadOnly,
		CSRF:        s.CSRF,
		NewSecret:   newSecret,
		NewSourceID: r.URL.Query().Get("newId"),
		Rotated:     r.URL.Query().Get("rotated") == "1",
		IngestBase:  c.ingestBase(),
		CreateError: r.URL.Query().Get("createError"),
	}
	c.renderPage(w, r, http.StatusOK, "stream-sources", "Stream Sources", view)
}

// serveFragmentStreamSourcesList handles
// GET /console/api/fragments/stream-sources-list.
func (c *Console) serveFragmentStreamSourcesList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentStreamSourcesList: w is nil")
	}
	if r == nil {
		panic("serveFragmentStreamSourcesList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentStreamSourcesList: no scope in context")
	}

	view, err := c.buildStreamSourcesListView(r)
	if err != nil {
		c.deps.Logger.Error("console: stream sources fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	html, err := renderFragment(c.ts.base, "stream-sources-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render stream sources fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── mutation handlers ────────────────────────────────────────────────────────

// handleStreamSourceCreate handles POST /console/stream-sources. On success it
// 303-redirects to the page with a one-shot reveal token (never the raw
// secret) in the URL; on a validation failure it redirects with
// ?createError=.
func (c *Console) handleStreamSourceCreate(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.mutationScope(w, r, "handleStreamSourceCreate")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	topic := r.FormValue("topic")

	src, raw, err := c.deps.StreamSources.Create(r.Context(), scope, name, topic)
	if err != nil {
		msg := "Create failed — check the name and topic."
		switch {
		case errors.Is(err, streamsource.ErrInvalidName):
			msg = "Name may only contain lowercase letters, numbers, hyphens, and underscores (max 64)."
		case errors.Is(err, streamsource.ErrInvalidTopic):
			msg = "Topic must be dot-separated tokens of letters, numbers, hyphens, and underscores (max 128 bytes)."
		default:
			c.deps.Logger.Error("console: create stream source", "err", err)
		}
		http.Redirect(w, r, "/console/stream-sources?createError="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	token, err := c.stashReveal(raw)
	if err != nil {
		c.deps.Logger.Error("console: stash revealed stream source secret", "err", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r,
		"/console/stream-sources?revealed="+token+"&newId="+url.QueryEscape(src.ID),
		http.StatusSeeOther)
}

// handleStreamSourceRotate handles POST /console/stream-sources/{id}/rotate.
// It mints a new secret (the old one stops working immediately) and 303s to
// the page with a one-shot reveal token (never the raw secret) in the URL.
func (c *Console) handleStreamSourceRotate(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.mutationScope(w, r, "handleStreamSourceRotate")
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	raw, err := c.deps.StreamSources.RotateSecret(r.Context(), scope, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: rotate stream source", "id", id, "err", err)
		http.Error(w, "rotate failed", http.StatusInternalServerError)
		return
	}
	token, err := c.stashReveal(raw)
	if err != nil {
		c.deps.Logger.Error("console: stash revealed stream source secret", "id", id, "err", err)
		http.Error(w, "rotate failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r,
		"/console/stream-sources?revealed="+token+"&newId="+url.QueryEscape(id)+"&rotated=1",
		http.StatusSeeOther)
}

// handleStreamSourceUpdateLimits handles POST /console/stream-sources/{id}. It
// applies the four ingest limits through the service, which clamps out-of-range
// values into the guardrail band.
func (c *Console) handleStreamSourceUpdateLimits(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.mutationScope(w, r, "handleStreamSourceUpdateLimits")
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	err := c.deps.StreamSources.UpdateLimits(
		r.Context(), scope, id,
		atoiOr(r.FormValue("rate_limit_per_min"), 0),
		atoiOr(r.FormValue("max_event_bytes"), 0),
		atoiOr(r.FormValue("max_batch_events"), 0),
		int64(atoiOr(r.FormValue("byte_budget"), 0)),
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: update stream source limits", "id", id, "err", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/stream-sources", http.StatusSeeOther)
}

// handleStreamSourceToggleActive handles POST /console/stream-sources/{id}/active.
// The form's `active` field ("true"|"false") carries the target state.
func (c *Console) handleStreamSourceToggleActive(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.mutationScope(w, r, "handleStreamSourceToggleActive")
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	active := r.FormValue("active") == "true"
	if err := c.deps.StreamSources.SetActive(r.Context(), scope, id, active); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: toggle stream source", "id", id, "active", active, "err", err)
		http.Error(w, "toggle failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/stream-sources", http.StatusSeeOther)
}

// handleStreamSourceDelete handles POST /console/stream-sources/{id}/delete.
func (c *Console) handleStreamSourceDelete(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.mutationScope(w, r, "handleStreamSourceDelete")
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := c.deps.StreamSources.Delete(r.Context(), scope, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: delete stream source", "id", id, "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/stream-sources", http.StatusSeeOther)
}

// ─── small helpers ────────────────────────────────────────────────────────────

// mutationScope is the shared preamble for the mutation handlers: it asserts a
// non-nil w/r, pulls the scope, and enforces ReadOnly. ok=false means the
// handler already wrote a response and must return.
func (c *Console) mutationScope(w http.ResponseWriter, r *http.Request, who string) (domain.Scope, bool) {
	if w == nil {
		panic(who + ": w is nil")
	}
	if r == nil {
		panic(who + ": r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic(who + ": no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return domain.Scope{}, false
	}
	return scope, true
}

// atoiOr parses s as a base-10 int, returning def when s is empty or invalid.
// The service clamps the parsed value, so a garbage field degrades to the
// default rather than erroring the request.
func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
