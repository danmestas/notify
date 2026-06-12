package console

// subscribers.go implements W2-C3 (subscriber list + detail) and W2-C5
// (subscriber activity tab) for the operator console.
//
// All data access is in-process via Deps.Audience (*audience.Service). No
// loopback HTTP hops. Every mutation handler is gated on Deps.ReadOnly
// server-side — disabled buttons in the template are not authorisation.

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

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

// ─── view structs ─────────────────────────────────────────────────────────────

// subscribersPageView is what subscribers.html binds.
type subscribersPageView struct {
	Header   pageHeader
	Fragment subscribersListView
	Signals  template.JS
	ReadOnly bool
	// ShowCreate renders the create-subscriber slide-over
	// (/console/subscribers/new).
	ShowCreate bool
}

// subscribersListView is the #subscribers-tbody fragment binding.
type subscribersListView struct {
	Rows       []subscriberRow
	NextCursor string
	TotalCount int
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

// subscriberDetailView is what subscriber_detail.html binds.
type subscriberDetailView struct {
	Header    pageHeader
	Sub       domain.Subscriber
	Activity  subscriberActivityView
	Signals   template.JS
	ReadOnly  bool
	ActiveTab string // "overview" | "activity" | "preferences"
	// ConfirmDelete renders the inline two-step delete confirmation
	// (?confirmDelete=true).
	ConfirmDelete bool
}

// subscriberActivityView is the activity-tab content for a subscriber.
type subscriberActivityView struct {
	Messages []subscriberMessageRow
}

// subscriberMessageRow is one row in the subscriber activity table.
type subscriberMessageRow struct {
	ID        string
	Channel   string
	Status    string
	RunID     string
	RunURL    string
	CreatedAt string
}

// ─── signal helpers ──────────────────────────────────────────────────────────

func subscribersSignalsJSON(email, name, phone, id string) template.JS {
	sig := map[string]string{
		"subscribersEmail": email,
		"subscribersName":  name,
		"subscribersPhone": phone,
		"subscribersId":    id,
		"subscriberTab":    "overview",
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

	page, err := c.deps.Audience.ListSubscribers(r.Context(), scope, after, 50)
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

	return subscribersListView{
		Rows:          rows,
		NextCursor:    page.NextCursor,
		TotalCount:    page.TotalCount,
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
		Header:   header,
		Fragment: frag,
		Signals:  subscribersSignalsJSON("", "", "", ""),
		ReadOnly: c.deps.ReadOnly,
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
	header := pageHeader{Title: "Subscribers", Subtitle: "All subscribers for this environment."}
	view := subscribersPageView{
		Header:   header,
		Fragment: frag,
		Signals:  subscribersSignalsJSON("", "", "", ""),
		ReadOnly: c.deps.ReadOnly,
		// The create slide-over is the whole point of /new; read-only
		// consoles render the plain list instead (mutations are gated
		// server-side regardless).
		ShowCreate: !c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "subscribers", "Add Subscriber", view)
}

// serveFragmentSubscribersList handles GET /console/api/fragments/subscribers-list.
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

	view, err := c.buildSubscribersListView(r)
	if err != nil {
		c.deps.Logger.Error("console: subscribers fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
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

// serveSubscriberDetail renders GET /console/subscribers/{subscriberId}.
func (c *Console) serveSubscriberDetail(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveSubscriberDetail: w is nil")
	}
	if r == nil {
		panic("serveSubscriberDetail: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveSubscriberDetail: no scope in context")
	}

	id := chi.URLParam(r, "subscriberId")
	if id == "" {
		c.serveNotFound(w, r)
		return
	}

	sub, err := c.deps.Audience.GetSubscriberByID(r.Context(), scope, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: subscriber detail", "err", err)
		c.serveError(w, r)
		return
	}

	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "overview"
	}

	var activity subscriberActivityView
	if tab == "activity" {
		activity, err = c.buildSubscriberActivity(r, scope, sub.SubscriberID)
		if err != nil {
			c.deps.Logger.Error("console: subscriber activity", "err", err)
			// non-fatal: render page with empty activity
		}
	}

	first := strings.TrimSpace(sub.FirstName)
	last := strings.TrimSpace(sub.LastName)
	title := sub.SubscriberID
	if first != "" || last != "" {
		title = strings.TrimSpace(first + " " + last)
	}

	header := pageHeader{
		Title:    title,
		Subtitle: sub.SubscriberID,
	}

	sigs, _ := json.Marshal(map[string]string{"subscriberTab": tab})

	view := subscriberDetailView{
		Header:        header,
		Sub:           sub,
		Activity:      activity,
		Signals:       template.JS(sigs),
		ReadOnly:      c.deps.ReadOnly,
		ActiveTab:     tab,
		ConfirmDelete: r.URL.Query().Get("confirmDelete") == "true",
	}
	c.renderPage(w, r, http.StatusOK, "subscriber-detail", title, view)
}

// buildSubscriberActivity fetches messages for a subscriber's activity tab.
func (c *Console) buildSubscriberActivity(r *http.Request, scope domain.Scope, subscriberID string) (subscriberActivityView, error) {
	// Messages.List takes (scope, subscriberID, channel) — pass "" for all channels.
	// The store query filters by subscriber_id and channel; empty channel means
	// we need to query without channel filter. The store interface only supports
	// exact channel match, so we pass a wildcard via notifications instead.
	// Use the Notifications repo: filter by subscriberID across all channels.
	f := store.NotificationFilter{
		SubscriberID: subscriberID,
	}
	notifPage, err := c.deps.Notifications.List(r.Context(), scope, f, "", 50)
	if err != nil {
		return subscriberActivityView{}, err
	}

	rows := make([]subscriberMessageRow, 0, len(notifPage.Items))
	for _, n := range notifPage.Items {
		rows = append(rows, subscriberMessageRow{
			ID:        n.ID,
			Channel:   "", // notifications don't carry channel; messages do
			Status:    "",
			RunID:     n.RunID,
			RunURL:    c.runURL(n.RunID),
			CreatedAt: n.CreatedAt,
		})
	}
	return subscriberActivityView{Messages: rows}, nil
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

	subID := strings.TrimSpace(r.FormValue("subscriberId"))
	if subID == "" {
		http.Error(w, "subscriberId is required", http.StatusBadRequest)
		return
	}

	created, err := c.deps.Audience.Upsert(r.Context(), scope, audience.SubscriberInput{
		SubscriberID: subID,
		Email:        strings.TrimSpace(r.FormValue("email")),
		FirstName:    strings.TrimSpace(r.FormValue("firstName")),
		LastName:     strings.TrimSpace(r.FormValue("lastName")),
		Phone:        strings.TrimSpace(r.FormValue("phone")),
		Locale:       strings.TrimSpace(r.FormValue("locale")),
		Timezone:     strings.TrimSpace(r.FormValue("timezone")),
	})
	if err != nil {
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

	_, err := c.deps.Audience.PatchSubscriber(r.Context(), scope, id, audience.SubscriberInput{
		Email:     strings.TrimSpace(r.FormValue("email")),
		FirstName: strings.TrimSpace(r.FormValue("firstName")),
		LastName:  strings.TrimSpace(r.FormValue("lastName")),
		Phone:     strings.TrimSpace(r.FormValue("phone")),
		Locale:    strings.TrimSpace(r.FormValue("locale")),
		Timezone:  strings.TrimSpace(r.FormValue("timezone")),
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
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
