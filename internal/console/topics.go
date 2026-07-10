package console

// topics.go implements W2-C4 (topic list + detail with subscriber management)
// and W4-S2 (topic activity tab) for the operator console.
//
// All data access is in-process via Deps.Audience (*audience.Service) and
// Deps.TopicTriggers (store.TopicTriggerRepo). No loopback HTTP hops. Every
// mutation handler is gated on Deps.ReadOnly server-side.

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// ─── signal keys ─────────────────────────────────────────────────────────────

var topicSignalKeys = []string{
	"topicsName",
	"topicsKey",
}

// ─── view structs ─────────────────────────────────────────────────────────────

// topicsPageView is what topics.html binds.
type topicsPageView struct {
	Header   pageHeader
	Fragment topicsListView
	Signals  template.JS
	ReadOnly bool
	// FormError carries an inline validation error surfaced from ?error= after
	// a failed create (e.g. a duplicate key), read on the /new redirect target.
	FormError string
}

// topicsListView is the #topics-tbody fragment binding.
type topicsListView struct {
	Rows       []topicRow
	HasFilters bool
	ReadOnly   bool
	FilterName string
	FilterKey  string
	// ConfirmDelete names the row key awaiting inline two-step delete
	// confirmation (chassis idiom — no native confirm() dialogs).
	ConfirmDelete string
	// CSRF is the session token for the confirm-delete POST form rendered
	// inside the fragment.
	CSRF string
}

// topicRow is one <tr> in the topics table.
type topicRow struct {
	Key         string
	Name        string
	MemberCount int
}

// topicDetailView is what topic_detail.html binds.
type topicDetailView struct {
	Header      pageHeader
	Topic       domain.Topic
	Members     []subscriberRow // subscriber rows for the members tab
	MemberCount int
	Activity    topicActivityView // activity tab (W4-S2)
	Signals     template.JS
	ReadOnly    bool
	ActiveTab   string // "overview" | "subscribers" | "activity"
	// ConfirmDelete renders the inline two-step topic-delete confirmation
	// (?confirmDelete=true).
	ConfirmDelete bool
	// ConfirmRemove names the member subscriberId awaiting inline removal
	// confirmation (?confirmRemove=<id>).
	ConfirmRemove string
	// FormError carries an inline error surfaced from ?error= after a failed
	// mutation on this page (e.g. add-member against an unknown subscriber id).
	FormError string
}

// topicActivityView is the Activity tab content for a topic.
type topicActivityView struct {
	Rows       []topicActivityRow
	NextCursor string
	// HasData is true when at least one row exists (even if Rows is empty after
	// filtering).
	HasData bool
}

// topicActivityRow is one row of topic broadcast activity.
type topicActivityRow struct {
	TransactionID string
	Channel       string
	Status        string
	SubscriberID  string
	WorkflowID    string
	RunURL        string
	CreatedAt     string
}

// ─── signal helpers ──────────────────────────────────────────────────────────

func topicsSignalsJSON(name, key string, newOpen bool) template.JS {
	sig := map[string]any{
		"topicsName": name,
		"topicsKey":  key,
		"topicTab":   "overview",
		// subscriber autocomplete signals
		"topicAddSubscriberID": "",
		// real JSON boolean — the string "false" is truthy in JS
		"topicNewOpen": newOpen,
	}
	b, err := json.Marshal(sig)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// ─── view builders ───────────────────────────────────────────────────────────

func (c *Console) buildTopicsListView(r *http.Request) (topicsListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildTopicsListView: no scope in context")
	}

	params := mergeFragmentParams(r, topicSignalKeys...)
	first := func(key string) string {
		if vs := params[key]; len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	filterName := strings.ToLower(strings.TrimSpace(first("topicsName")))
	filterKey := strings.ToLower(strings.TrimSpace(first("topicsKey")))

	topics, err := c.deps.Audience.ListTopics(r.Context(), scope)
	if err != nil {
		return topicsListView{}, err
	}

	hasFilters := filterName != "" || filterKey != ""

	var rows []topicRow
	for _, t := range topics {
		if filterName != "" && !strings.Contains(strings.ToLower(t.Name), filterName) {
			continue
		}
		if filterKey != "" && !strings.Contains(strings.ToLower(t.Key), filterKey) {
			continue
		}
		rows = append(rows, topicRow{
			Key:         t.Key,
			Name:        t.Name,
			MemberCount: t.MemberCount,
		})
	}

	s, _ := sessionFromContext(r.Context())

	return topicsListView{
		Rows:          rows,
		HasFilters:    hasFilters,
		ReadOnly:      c.deps.ReadOnly,
		FilterName:    filterName,
		FilterKey:     filterKey,
		ConfirmDelete: r.URL.Query().Get("confirmDelete"),
		CSRF:          s.CSRF,
	}, nil
}

// ─── page handlers ────────────────────────────────────────────────────────────

// serveTopicsPage renders GET /console/topics.
func (c *Console) serveTopicsPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveTopicsPage: w is nil")
	}
	if r == nil {
		panic("serveTopicsPage: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveTopicsPage: no scope in context")
	}

	frag, err := c.buildTopicsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: topics page list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Topics",
		Subtitle: "Named subscriber groups for broadcast delivery.",
		Tiles: []tile{
			{Label: "topics", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Topics"}
	}

	view := topicsPageView{
		Header:   header,
		Fragment: frag,
		Signals:  topicsSignalsJSON("", "", false),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "topics", "Topics", view)
}

// serveTopicNew renders GET /console/topics/new.
func (c *Console) serveTopicNew(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveTopicNew: w is nil")
	}
	if r == nil {
		panic("serveTopicNew: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveTopicNew: no scope in context")
	}
	frag, err := c.buildTopicsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: topic new page", "err", err)
		c.serveError(w, r)
		return
	}
	header := pageHeader{
		Title:    "Topics",
		Subtitle: "Named subscriber groups for broadcast delivery.",
		Tiles: []tile{
			{Label: "topics", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Topics"}
	}
	view := topicsPageView{
		Header:    header,
		Fragment:  frag,
		Signals:   topicsSignalsJSON("", "", !c.deps.ReadOnly),
		ReadOnly:  c.deps.ReadOnly,
		FormError: r.URL.Query().Get("error"),
	}
	c.renderPage(w, r, http.StatusOK, "topics", "Topics", view)
}

// serveFragmentTopicsList handles GET /console/api/fragments/topics-list.
func (c *Console) serveFragmentTopicsList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentTopicsList: w is nil")
	}
	if r == nil {
		panic("serveFragmentTopicsList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentTopicsList: no scope in context")
	}

	view, err := c.buildTopicsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: topics fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}

	html, err := renderFragment(c.ts.base, "topics-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render topics fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// serveFragmentSubscriberAutocomplete handles GET /console/api/fragments/subscriber-autocomplete.
// Returns a datalist-style fragment of matching subscribers for the topic
// member add input.
func (c *Console) serveFragmentSubscriberAutocomplete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentSubscriberAutocomplete: w is nil")
	}
	if r == nil {
		panic("serveFragmentSubscriberAutocomplete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentSubscriberAutocomplete: no scope in context")
	}

	page, err := c.deps.Audience.ListSubscribers(r.Context(), scope, "", 100)
	if err != nil {
		c.deps.Logger.Error("console: subscriber autocomplete", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}

	rows := make([]subscriberRow, 0, len(page.Items))
	for _, s := range page.Items {
		rows = append(rows, toSubscriberRow(s))
	}

	html, err := renderFragment(c.ts.base, "subscriber-autocomplete", rows)
	if err != nil {
		c.deps.Logger.Error("console: render autocomplete fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// serveTopicDetail renders GET /console/topics/{key}.
func (c *Console) serveTopicDetail(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveTopicDetail: w is nil")
	}
	if r == nil {
		panic("serveTopicDetail: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveTopicDetail: no scope in context")
	}

	key := chi.URLParam(r, "key")
	if key == "" {
		c.serveNotFound(w, r)
		return
	}

	topic, err := c.deps.Audience.GetTopic(r.Context(), scope, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: topic detail", "err", err)
		c.serveError(w, r)
		return
	}

	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "overview"
	}

	var members []subscriberRow
	if tab == "subscribers" {
		members, err = c.buildTopicMembers(r, scope, key)
		if err != nil {
			c.deps.Logger.Error("console: topic members", "err", err)
			// non-fatal
		}
	}

	var activity topicActivityView
	if tab == "activity" {
		var actErr error
		activity, actErr = c.buildTopicActivity(r, scope, key)
		if actErr != nil {
			c.deps.Logger.Error("console: topic activity", "err", actErr)
			// non-fatal
		}
	}

	header := pageHeader{
		Title:    topic.Name,
		Subtitle: topic.Key,
	}

	sigs, _ := json.Marshal(map[string]string{
		"topicTab":             tab,
		"topicAddSubscriberID": "",
	})

	view := topicDetailView{
		Header:        header,
		Topic:         topic.Topic,
		Members:       members,
		MemberCount:   topic.MemberCount,
		Activity:      activity,
		ReadOnly:      c.deps.ReadOnly,
		Signals:       template.JS(sigs),
		ActiveTab:     tab,
		ConfirmDelete: r.URL.Query().Get("confirmDelete") == "true",
		ConfirmRemove: r.URL.Query().Get("confirmRemove"),
		FormError:     r.URL.Query().Get("error"),
	}
	c.renderPage(w, r, http.StatusOK, "topic-detail", topic.Name, view)
}

// buildTopicActivity fetches activity rows for the Topic Activity tab.
func (c *Console) buildTopicActivity(r *http.Request, scope domain.Scope, topicKey string) (topicActivityView, error) {
	if c.deps.TopicTriggers == nil {
		return topicActivityView{}, nil
	}
	cursor := r.URL.Query().Get("cursor")
	page, err := c.deps.TopicTriggers.ListByTopic(r.Context(), scope, topicKey, cursor, 25)
	if err != nil {
		return topicActivityView{}, err
	}

	rows := make([]topicActivityRow, 0, len(page.Items))
	for _, item := range page.Items {
		rows = append(rows, topicActivityRow{
			TransactionID: item.TransactionID,
			Channel:       item.Channel,
			Status:        item.Status,
			SubscriberID:  item.SubscriberID,
			WorkflowID:    item.WorkflowID,
			RunURL:        "",
			CreatedAt:     item.CreatedAt,
		})
	}
	return topicActivityView{
		Rows:       rows,
		NextCursor: page.NextCursor,
		HasData:    len(rows) > 0 || cursor != "",
	}, nil
}

// buildTopicMembers resolves subscriber rows for a topic's member list.
func (c *Console) buildTopicMembers(r *http.Request, scope domain.Scope, topicKey string) ([]subscriberRow, error) {
	ids, err := c.deps.Audience.ListTopicSubscriberIDs(r.Context(), scope, topicKey)
	if err != nil {
		return nil, err
	}

	rows := make([]subscriberRow, 0, len(ids))
	for _, subID := range ids {
		s, err := c.deps.Audience.Get(r.Context(), scope, subID)
		if err != nil {
			// Subscriber may be soft-deleted; skip gracefully.
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		rows = append(rows, toSubscriberRow(s))
	}
	return rows, nil
}

// ─── mutation handlers ────────────────────────────────────────────────────────

// handleTopicCreate handles POST /console/topics.
func (c *Console) handleTopicCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleTopicCreate: w is nil")
	}
	if r == nil {
		panic("handleTopicCreate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleTopicCreate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	key := strings.TrimSpace(r.FormValue("key"))
	name := strings.TrimSpace(r.FormValue("name"))
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	// Audience.CreateTopic upserts by (environment_id, key) — a re-used key
	// would otherwise silently rename the existing topic (M3). Pre-check
	// existence and reject the create instead; an intentional rename stays via
	// handleTopicUpdate, which legitimately relies on the upsert.
	if _, err := c.deps.Audience.GetTopic(r.Context(), scope, key); err == nil {
		redirectTopicNewErr(w, r, "Topic key already exists.")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		c.deps.Logger.Error("console: check existing topic", "err", err)
		redirectTopicNewErr(w, r, "Create failed.")
		return
	}

	created, err := c.deps.Audience.CreateTopic(r.Context(), scope, key, name)
	if err != nil {
		c.deps.Logger.Error("console: create topic", "err", err)
		redirectTopicNewErr(w, r, "Create failed.")
		return
	}

	http.Redirect(w, r, "/console/topics/"+created.Key, http.StatusSeeOther)
}

// redirectTopicNewErr 303-redirects back to /console/topics/new with an
// inline ?error= banner instead of a raw 4xx/5xx plaintext response (M3).
func redirectTopicNewErr(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/console/topics/new?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleTopicUpdate handles POST /console/topics/{key} (?_method=PATCH).
// Renames the topic (key is immutable; only name changes).
func (c *Console) handleTopicUpdate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleTopicUpdate: w is nil")
	}
	if r == nil {
		panic("handleTopicUpdate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleTopicUpdate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	key := chi.URLParam(r, "key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	// CreateTopic is an upsert — re-creating with same key updates name.
	_, err := c.deps.Audience.CreateTopic(r.Context(), scope, key, name)
	if err != nil {
		c.deps.Logger.Error("console: update topic", "err", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/topics/"+key, http.StatusSeeOther)
}

// handleTopicDelete handles POST /console/topics/{key}/delete.
func (c *Console) handleTopicDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleTopicDelete: w is nil")
	}
	if r == nil {
		panic("handleTopicDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleTopicDelete: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	key := chi.URLParam(r, "key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}

	if err := c.deps.Audience.DeleteTopic(r.Context(), scope, key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: delete topic", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/topics", http.StatusSeeOther)
}

// handleTopicAddSubscriber handles POST /console/topics/{key}/subscribers.
func (c *Console) handleTopicAddSubscriber(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleTopicAddSubscriber: w is nil")
	}
	if r == nil {
		panic("handleTopicAddSubscriber: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleTopicAddSubscriber: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	key := chi.URLParam(r, "key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
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

	// The subscriber must exist in scope — memberships can never point at
	// nothing (same guard as the public API; prevents phantom member rows).
	if _, err := c.deps.Audience.Get(r.Context(), scope, subID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A nonexistent subscriber id must not surface a raw 422 plaintext
			// dead-end (no console chrome, no way back) — redirect to the topic
			// page with an inline error banner instead (M4).
			redirectTopicAddSubscriberErr(w, r, key, "Subscriber not found.")
			return
		}
		c.deps.Logger.Error("console: add topic subscriber lookup", "err", err)
		redirectTopicAddSubscriberErr(w, r, key, "Add failed.")
		return
	}

	if err := c.deps.Audience.AddTopicSubscriber(r.Context(), scope, key, subID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			redirectTopicAddSubscriberErr(w, r, key, "Topic not found.")
			return
		}
		c.deps.Logger.Error("console: add topic subscriber", "err", err)
		redirectTopicAddSubscriberErr(w, r, key, "Add failed.")
		return
	}

	http.Redirect(w, r, "/console/topics/"+key+"?tab=subscribers", http.StatusSeeOther)
}

// redirectTopicAddSubscriberErr 303-redirects back to the topic's subscribers
// tab with an inline ?error= banner instead of a raw 4xx/5xx plaintext
// response (M4).
func redirectTopicAddSubscriberErr(w http.ResponseWriter, r *http.Request, key, msg string) {
	http.Redirect(w, r,
		"/console/topics/"+key+"?tab=subscribers&error="+url.QueryEscape(msg),
		http.StatusSeeOther)
}

// handleTopicRemoveSubscriber handles POST /console/topics/{key}/subscribers/{subscriberId}/remove.
func (c *Console) handleTopicRemoveSubscriber(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleTopicRemoveSubscriber: w is nil")
	}
	if r == nil {
		panic("handleTopicRemoveSubscriber: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleTopicRemoveSubscriber: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	key := chi.URLParam(r, "key")
	subID := chi.URLParam(r, "subscriberId")
	if key == "" || subID == "" {
		http.Error(w, "missing key or subscriberId", http.StatusBadRequest)
		return
	}

	if err := c.deps.Audience.RemoveTopicSubscriber(r.Context(), scope, key, subID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: remove topic subscriber", "err", err)
		http.Error(w, "remove failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/topics/"+key+"?tab=subscribers", http.StatusSeeOther)
}
