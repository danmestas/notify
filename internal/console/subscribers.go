package console

// subscribers.go implements W2-C3 (subscriber list) and the create + update +
// delete mutation handlers for the operator console.
//
// The profile custom-field editor helpers (shared by the create form and the
// detail page's edit form) live in subscriber_custom_fields.go. Subscriber
// detail (W2-C3 detail), activity tab (W2-C5), channels, and preferences
// (W4-S1) live in subscriber_detail.go.
//
// All data access is in-process via Deps.Audience (*audience.Service) and
// Deps.Preferences (*preferences.Service). No loopback HTTP hops. Every
// mutation handler is gated on Deps.ReadOnly server-side — disabled buttons
// in the template are not authorisation.

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// ─── signal keys ─────────────────────────────────────────────────────────────

var subscriberSignalKeys = []string{
	"subscribersEmail",
	"subscribersPhone",
	"subscribersName",
	"subscribersId",
}

const (
	subscribersDefaultLimit = 50
	subscribersMaxLimit     = 200

	// maxSubscriberFieldLen bounds console-editable free-text subscriber
	// profile fields. The client's <input type=email> and any maxlength
	// attributes are UX hints only, not a security boundary — a direct POST
	// can send an unbroken multi-thousand-character value that blows out the
	// fixed-width detail-page layout (headings, title, profile table all
	// overflow with no wrapping). 256 chars comfortably covers real-world
	// names, emails, phone numbers, locales, and timezones.
	maxSubscriberFieldLen = 256
)

// validateSubscriberProfileInput server-side validates the console-editable
// free-text subscriber fields shared by create and update. Returns a
// human-readable message for the first invalid field, or "" when the input
// is acceptable to persist. Email may be empty — a subscriber can legitimately
// have no email address — but a NON-EMPTY value must be a well-formed
// address; the client's HTML5 type=email attribute is not a security boundary.
func validateSubscriberProfileInput(in audience.SubscriberInput) string {
	if in.Email != "" {
		if _, err := mail.ParseAddress(in.Email); err != nil {
			return "Enter a valid email address"
		}
	}
	fields := []struct {
		label string
		value string
	}{
		{"Email", in.Email},
		{"First name", in.FirstName},
		{"Last name", in.LastName},
		{"Phone", in.Phone},
		{"Locale", in.Locale},
		{"Timezone", in.Timezone},
	}
	for _, f := range fields {
		if len(f.value) > maxSubscriberFieldLen {
			return f.label + " is too long (max " + strconv.Itoa(maxSubscriberFieldLen) + " characters)"
		}
	}
	return ""
}

// ─── view structs ─────────────────────────────────────────────────────────────

// subscribersPageView is what subscribers.html binds.
type subscribersPageView struct {
	Header   pageHeader
	Fragment subscribersListView
	Signals  template.JS
	ReadOnly bool
	// FormError is an inline validation error surfaced from ?error= after a
	// failed create (e.g. duplicate subscriberId). The slide-over stays open
	// via subNewOpen=true so the operator sees the error in context.
	FormError string
	// CustomFieldsEditor seeds the create slide-over's repeatable custom-field
	// rows. A brand-new subscriber has no stored Data, so this is always the
	// empty-row-pool state (one visible blank row).
	CustomFieldsEditor subscriberCustomFieldsEditorView
}

// subscribersListView is the #subscribers-tbody fragment binding.
type subscribersListView struct {
	Rows       []subscriberRow
	NextCursor string
	TotalCount int
	Limit      int
	HasFilters bool
	ReadOnly   bool
	// filter echoes
	FilterEmail string
	FilterName  string
	FilterPhone string
	FilterID    string
	// ConfirmDelete names the row id awaiting inline two-step delete
	// confirmation (chassis idiom — no native confirm() dialogs).
	ConfirmDelete string
	// CSRF is the session token for the confirm-delete POST form rendered
	// inside the fragment.
	CSRF string
}

// subscriberRow is one <tr> in the subscribers table.
type subscriberRow struct {
	ID           string // internal ULID (used for detail link + delete)
	SubscriberID string // external caller-owned id
	DisplayName  string // FirstName+" "+LastName, falls back to SubscriberID
	Initials     string // up to 2 chars for avatar
	Email        string
	Phone        string
	CreatedAt    string
}

// ─── signal helpers ──────────────────────────────────────────────────────────

func subscribersSignalsJSON(email, name, phone, id string, newOpen bool) template.JS {
	sig := map[string]any{
		"subscribersEmail": email,
		"subscribersName":  name,
		"subscribersPhone": phone,
		"subscribersId":    id,
		"subscriberTab":    "overview",
		// subNewOpen drives the create slide-over's data-show. A real JSON
		// boolean (not the string "false", which is truthy in JS).
		"subNewOpen": newOpen,
	}
	b, err := json.Marshal(sig)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// ─── view builders ───────────────────────────────────────────────────────────

func toSubscriberRow(s domain.Subscriber) subscriberRow {
	first := strings.TrimSpace(s.FirstName)
	last := strings.TrimSpace(s.LastName)
	var displayName, initials string
	if first != "" || last != "" {
		displayName = strings.TrimSpace(first + " " + last)
		if first != "" {
			initials += string([]rune(first)[0])
		}
		if last != "" {
			initials += string([]rune(last)[0])
		}
	} else {
		displayName = s.SubscriberID
		rr := []rune(s.SubscriberID)
		if len(rr) >= 2 {
			initials = string(rr[:2])
		} else if len(rr) == 1 {
			initials = string(rr[0])
		}
	}
	return subscriberRow{
		ID:           s.ID,
		SubscriberID: s.SubscriberID,
		DisplayName:  displayName,
		Initials:     strings.ToUpper(initials),
		Email:        s.Email,
		Phone:        s.Phone,
		CreatedAt:    s.ID, // ULID encodes created_at
	}
}

// buildSubscribersListView fetches subscribers and applies client-side text
// filters. Cursor-pagination is handled by the store; text search filters the
// returned page.
func (c *Console) buildSubscribersListView(r *http.Request) (subscribersListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildSubscribersListView: no scope in context")
	}

	params := mergeFragmentParams(r, subscriberSignalKeys...)
	first := func(key string) string {
		if vs := params[key]; len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	filterEmail := strings.ToLower(strings.TrimSpace(first("subscribersEmail")))
	filterName := strings.ToLower(strings.TrimSpace(first("subscribersName")))
	filterPhone := strings.TrimSpace(first("subscribersPhone"))
	filterID := strings.TrimSpace(first("subscribersId"))
	after := first("after")
	limit := parseSubscribersLimit(params)

	page, err := c.deps.Audience.ListSubscribers(r.Context(), scope, after, limit)
	if err != nil {
		return subscribersListView{}, err
	}

	hasFilters := filterEmail != "" || filterName != "" || filterPhone != "" || filterID != ""

	var rows []subscriberRow
	for _, s := range page.Items {
		if filterEmail != "" && !strings.Contains(strings.ToLower(s.Email), filterEmail) {
			continue
		}
		if filterName != "" {
			full := strings.ToLower(s.FirstName + " " + s.LastName)
			if !strings.Contains(full, filterName) {
				continue
			}
		}
		if filterPhone != "" && !strings.Contains(s.Phone, filterPhone) {
			continue
		}
		if filterID != "" && !strings.Contains(strings.ToLower(s.SubscriberID), strings.ToLower(filterID)) {
			continue
		}
		rows = append(rows, toSubscriberRow(s))
	}

	s, _ := sessionFromContext(r.Context())

	// When a filter is active, suppress the NextCursor. The store-level cursor
	// is anchored to the unfiltered row set; following it while a filter is
	// active fetches the next unfiltered page, discards rows that don't match,
	// and renders an empty "no match" state — the bug. Hiding "Load more"
	// when filters are active is the minimal correct fix.
	nextCursor := page.NextCursor
	if hasFilters {
		nextCursor = ""
	}

	return subscribersListView{
		Rows:          rows,
		NextCursor:    nextCursor,
		TotalCount:    page.TotalCount,
		Limit:         limit,
		HasFilters:    hasFilters,
		ReadOnly:      c.deps.ReadOnly,
		FilterEmail:   filterEmail,
		FilterName:    filterName,
		FilterPhone:   filterPhone,
		FilterID:      filterID,
		ConfirmDelete: r.URL.Query().Get("confirmDelete"),
		CSRF:          s.CSRF,
	}, nil
}

// parseSubscribersLimit reads ?limit= from a merged query; defaults to
// subscribersDefaultLimit, capped at subscribersMaxLimit.
func parseSubscribersLimit(q map[string][]string) int {
	if vs := q["limit"]; len(vs) > 0 {
		if n, err := strconv.Atoi(vs[0]); err == nil && n > 0 {
			if n > subscribersMaxLimit {
				return subscribersMaxLimit
			}
			return n
		}
	}
	return subscribersDefaultLimit
}

// ─── page handlers ────────────────────────────────────────────────────────────

// serveSubscribersPage renders GET /console/subscribers.
func (c *Console) serveSubscribersPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveSubscribersPage: w is nil")
	}
	if r == nil {
		panic("serveSubscribersPage: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveSubscribersPage: no scope in context")
	}

	frag, err := c.buildSubscribersListView(r)
	if err != nil {
		c.deps.Logger.Error("console: subscribers page list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Subscribers",
		Subtitle: "All subscribers for this environment.",
		Tiles: []tile{
			{Label: "total", Count: frag.TotalCount, Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Subscribers"}
	}

	view := subscribersPageView{
		Header:             header,
		Fragment:           frag,
		Signals:            subscribersSignalsJSON("", "", "", "", false),
		ReadOnly:           c.deps.ReadOnly,
		CustomFieldsEditor: subscriberCustomFieldsEditor(nil),
	}
	c.renderPage(w, r, http.StatusOK, "subscribers", "Subscribers", view)
}

// serveSubscriberNew renders GET /console/subscribers/new.
func (c *Console) serveSubscriberNew(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveSubscriberNew: w is nil")
	}
	if r == nil {
		panic("serveSubscriberNew: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveSubscriberNew: no scope in context")
	}
	frag, err := c.buildSubscribersListView(r)
	if err != nil {
		c.deps.Logger.Error("console: subscriber new page", "err", err)
		c.serveError(w, r)
		return
	}
	// /new renders the SAME page as the list (identical header, title, and total
	// tile) so the drawer is a pure overlay — no layout shift versus the list.
	// It just seeds subNewOpen=true so a direct visit opens the slide-over.
	header := pageHeader{
		Title:    "Subscribers",
		Subtitle: "All subscribers for this environment.",
		Tiles: []tile{
			{Label: "total", Count: frag.TotalCount, Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Subscribers"}
	}
	view := subscribersPageView{
		Header:             header,
		Fragment:           frag,
		Signals:            subscribersSignalsJSON("", "", "", "", !c.deps.ReadOnly),
		ReadOnly:           c.deps.ReadOnly,
		FormError:          r.URL.Query().Get("error"),
		CustomFieldsEditor: subscriberCustomFieldsEditor(nil),
	}
	c.renderPage(w, r, http.StatusOK, "subscribers", "Subscribers", view)
}

// serveFragmentSubscribersList handles GET /console/api/fragments/subscribers-list.
// Filter signals + cursor come from the merged query map; the response is one
// PatchElements SSE event repainting #subscribers-tbody.
func (c *Console) serveFragmentSubscribersList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentSubscribersList: w is nil")
	}
	if r == nil {
		panic("serveFragmentSubscribersList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentSubscribersList: no scope in context")
	}

	// Read the after= cursor directly from the URL query — mergeFragmentParams
	// folds signal values but `after` is a one-shot pagination token, not a
	// persistent signal, so the raw query value is authoritative.
	cursor := r.URL.Query().Get("after")

	view, err := c.buildSubscribersListView(r)
	if err != nil {
		c.deps.Logger.Error("console: subscribers fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}

	// When a cursor is present the request is a "Load more" click: append new
	// rows before the existing pagination footer rather than replacing the
	// whole subscribers-tbody wrapper.  Two SSE events are sent:
	//   1. new <tr> rows   — mode=before targeting #subscribers-pagination
	//   2. pagination <tr> — mode=outer (morph) to advance the cursor
	// When cursor is empty (initial load or filter change) a single outer
	// replace of the full #subscribers-tbody is the correct behavior.
	if cursor != "" {
		c.patchSubscribersAppend(w, r, view)
		return
	}

	html, err := renderFragment(c.ts.base, "subscribers-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render subscribers fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// patchSubscribersAppend sends two Datastar SSE events for a "Load more" response:
//  1. The new data rows (subscribers-rows template) inserted before
//     #subscribers-pagination via mode=before — the rows land above the footer
//     while existing rows stay in place.
//  2. The updated pagination footer (subscribers-pagination template) morphed in
//     place via mode=outer so the cursor advances to the next page.
func (c *Console) patchSubscribersAppend(w http.ResponseWriter, r *http.Request, lv subscribersListView) {
	if w == nil {
		panic("patchSubscribersAppend: w is nil")
	}
	if r == nil {
		panic("patchSubscribersAppend: r is nil")
	}

	rowsHTML, err := renderFragment(c.ts.base, "subscribers-rows", lv)
	if err != nil {
		c.deps.Logger.Error("console: render subscribers-rows fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	paginHTML, err := renderFragment(c.ts.base, "subscribers-pagination", lv)
	if err != nil {
		c.deps.Logger.Error("console: render subscribers-pagination fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}

	sse := datastar.NewSSE(w, r)
	// Event 1: insert new rows before the pagination row so they accumulate above it.
	if err := sse.PatchElements(
		rowsHTML,
		datastar.WithSelector("#subscribers-pagination"),
		datastar.WithMode(datastar.ElementPatchModeBefore),
	); err != nil {
		c.deps.Logger.Error("console: patch subscribers rows (append)", "err", err)
		return
	}
	// Event 2: morph the pagination row in place to advance the cursor.
	if err := sse.PatchElements(paginHTML); err != nil {
		c.deps.Logger.Error("console: patch subscribers pagination", "err", err)
	}
}

// ─── mutation handlers ────────────────────────────────────────────────────────

// handleSubscriberCreate handles POST /console/subscribers.
func (c *Console) handleSubscriberCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleSubscriberCreate: w is nil")
	}
	if r == nil {
		panic("handleSubscriberCreate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleSubscriberCreate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	in := audience.SubscriberInput{
		Email:     strings.TrimSpace(r.FormValue("email")),
		FirstName: strings.TrimSpace(r.FormValue("firstName")),
		LastName:  strings.TrimSpace(r.FormValue("lastName")),
		Phone:     strings.TrimSpace(r.FormValue("phone")),
		Locale:    strings.TrimSpace(r.FormValue("locale")),
		Timezone:  strings.TrimSpace(r.FormValue("timezone")),
	}
	// Server-side validation (H6/H7): the client's <input type=email> and any
	// maxlength attributes are UX hints only. Reject before touching the
	// store so a malformed email or pathological free-text value never
	// reaches the subscriber record or the rendered detail page.
	if msg := validateSubscriberProfileInput(in); msg != "" {
		http.Redirect(w, r,
			"/console/subscribers/new?error="+url.QueryEscape(msg),
			http.StatusSeeOther)
		return
	}
	// L10: parse the repeatable custom-field rows (custom_key_N/custom_value_N)
	// into Data. A brand-new subscriber has nothing to preserve, so this is
	// applied unconditionally — an editor submitted with no populated rows
	// just yields "{}".
	customData, customMsg := subscriberCustomFieldsFromForm(r)
	if customMsg != "" {
		http.Redirect(w, r,
			"/console/subscribers/new?error="+url.QueryEscape(customMsg),
			http.StatusSeeOther)
		return
	}
	in.Data = customData

	// Console-only convenience: a subscriber needs a stable, non-empty external
	// key (triggers address subscribers by it), but operators creating one by
	// hand don't always have an external id. When left blank, mint one
	// server-side using the same ULID generator the store uses for primary
	// keys, slugged with a readable "sub_" prefix. The /v1 API stays
	// strictly caller-provided.
	subID := strings.TrimSpace(r.FormValue("subscriberId"))
	userSuppliedID := subID != ""
	if !userSuppliedID {
		subID = "sub_" + strings.ToLower(store.NewULID())
	}

	// Console-create intent: when the operator supplied a subscriberId, reject
	// the form if that id is already taken by a LIVE (non-deleted) record.
	// A previously soft-deleted subscriber with the same id is NOT a collision:
	// we hard-purge the deleted row first so the subsequent Upsert inserts a
	// fresh row with no inherited stale profile. The /v1 API and Upsert
	// semantics for other callers are unchanged.
	//
	// TOCTOU note: between the Get (sees deleted) and the purge+Upsert below,
	// a concurrent live insert of the same id is possible. The AND deleted=1
	// predicate in PurgeDeleted prevents it from removing that live row; the
	// Upsert's ON CONFLICT then merges onto the live row rather than erroring.
	// This race is acceptable for the human-paced console flow and is noted here
	// so it is not a surprise.
	if userSuppliedID {
		if existing, err := c.deps.Audience.Get(r.Context(), scope, subID); err == nil {
			// GetByExternalID has no deleted=0 filter; only block when the
			// found record is still live.
			if !existing.Deleted {
				http.Redirect(w, r,
					"/console/subscribers/new?error="+url.QueryEscape("That Subscriber ID is taken"),
					http.StatusSeeOther)
				return
			}
			// Deleted record — hard-purge the stale row before Upsert so the
			// re-create yields a clean record with no inherited profile fields.
			if _, err := c.deps.Audience.PurgeDeleted(r.Context(), scope, subID); err != nil {
				c.deps.Logger.Error("console: create subscriber: purge deleted", "err", err)
				http.Error(w, "create failed", http.StatusInternalServerError)
				return
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			c.deps.Logger.Error("console: create subscriber: check existence", "err", err)
			http.Error(w, "create failed", http.StatusInternalServerError)
			return
		}
	}

	in.SubscriberID = subID
	created, err := c.deps.Audience.Upsert(r.Context(), scope, in)
	if err != nil {
		// A *audience.ValidationError means the audience service itself
		// rejected the input (e.g. the custom-field count cap, which this
		// handler never pre-checks). Surface it the same way as the console's
		// own pre-checks above rather than a generic 500 — it's a caller
		// error, not a server fault.
		var verr *audience.ValidationError
		if errors.As(err, &verr) {
			http.Redirect(w, r,
				"/console/subscribers/new?error="+url.QueryEscape(verr.Msg),
				http.StatusSeeOther)
			return
		}
		c.deps.Logger.Error("console: create subscriber", "err", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/subscribers/"+created.ID, http.StatusSeeOther)
}

// handleSubscriberUpdate handles POST /console/subscribers/{subscriberId} (?_method=PATCH).
func (c *Console) handleSubscriberUpdate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleSubscriberUpdate: w is nil")
	}
	if r == nil {
		panic("handleSubscriberUpdate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleSubscriberUpdate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	id := chi.URLParam(r, "subscriberId")
	if id == "" {
		http.Error(w, "missing subscriberId", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	in := audience.SubscriberInput{
		Email:     strings.TrimSpace(r.FormValue("email")),
		FirstName: strings.TrimSpace(r.FormValue("firstName")),
		LastName:  strings.TrimSpace(r.FormValue("lastName")),
		Phone:     strings.TrimSpace(r.FormValue("phone")),
		Locale:    strings.TrimSpace(r.FormValue("locale")),
		Timezone:  strings.TrimSpace(r.FormValue("timezone")),
	}
	// Server-side validation (H6/H7): same guard as create — reject before
	// touching the store, redirect back to the detail page (overview tab)
	// with an inline error instead of persisting garbage.
	if msg := validateSubscriberProfileInput(in); msg != "" {
		http.Redirect(w, r,
			"/console/subscribers/"+id+"?error="+url.QueryEscape(msg),
			http.StatusSeeOther)
		return
	}
	// L10: only touch Data when the request actually posted the custom-field
	// editor's row 0 — i.e. our real edit form was submitted (it always
	// renders at least one row, even when empty). A raw PATCH-style POST that
	// never carries custom_key_0 leaves Data untouched: SubscriberInput.Data
	// stays its zero value nil, and the store's merge-upsert
	// (`COALESCE(NULLIF(excluded.data, X''), subscribers.data)`) preserves
	// whatever was already stored rather than silently wiping it.
	if r.Form.Has("custom_key_0") {
		customData, customMsg := subscriberCustomFieldsFromForm(r)
		if customMsg != "" {
			http.Redirect(w, r,
				"/console/subscribers/"+id+"?error="+url.QueryEscape(customMsg),
				http.StatusSeeOther)
			return
		}
		in.Data = customData
	}

	_, err := c.deps.Audience.PatchSubscriber(r.Context(), scope, id, in)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		// A *audience.ValidationError means the audience service itself
		// rejected the input (e.g. the custom-field count cap growing past
		// the grandfathered baseline). Surface it the same way as the
		// console's own pre-checks above rather than a generic 500.
		var verr *audience.ValidationError
		if errors.As(err, &verr) {
			http.Redirect(w, r,
				"/console/subscribers/"+id+"?error="+url.QueryEscape(verr.Msg),
				http.StatusSeeOther)
			return
		}
		c.deps.Logger.Error("console: update subscriber", "err", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/subscribers/"+id, http.StatusSeeOther)
}

// handleSubscriberDelete handles POST /console/subscribers/{subscriberId}/delete.
func (c *Console) handleSubscriberDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleSubscriberDelete: w is nil")
	}
	if r == nil {
		panic("handleSubscriberDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleSubscriberDelete: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	id := chi.URLParam(r, "subscriberId")
	if id == "" {
		http.Error(w, "missing subscriberId", http.StatusBadRequest)
		return
	}

	if err := c.deps.Audience.DeleteSubscriber(r.Context(), scope, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: delete subscriber", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/subscribers", http.StatusSeeOther)
}
