package console

// subscribers.go implements W2-C3 (subscriber list + detail), W2-C5
// (subscriber activity tab), and W4-S1 (subscriber preferences tab) for the
// operator console.
//
// All data access is in-process via Deps.Audience (*audience.Service) and
// Deps.Preferences (*preferences.Service). No loopback HTTP hops. Every
// mutation handler is gated on Deps.ReadOnly server-side — disabled buttons
// in the template are not authorisation.

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"sort"
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
	Header      pageHeader
	Sub         domain.Subscriber
	Activity    subscriberActivityView
	Preferences subscriberPrefsView
	Channels    *subscriberChannelsView
	Signals     template.JS
	ReadOnly    bool
	ActiveTab   string // "overview" | "activity" | "preferences" | "channels"
	// ConfirmDelete renders the inline two-step delete confirmation
	// (?confirmDelete=true).
	ConfirmDelete bool
}

// subscriberChannelsView is the channels-tab content for a subscriber: the
// list of existing per-subscriber delivery addresses plus the set of
// connectable providers offered in the add-form.
type subscriberChannelsView struct {
	// Existing holds the channels already addressed for this subscriber.
	Existing []subscriberChannelRow
	// Connectable holds the providers that have an active chat/push
	// integration in scope — the only ones the add-form offers.
	Connectable []uiSelectOption
}

// subscriberChannelRow is one existing per-subscriber channel address.
type subscriberChannelRow struct {
	ProviderID    string
	ProviderLabel string
	ChannelType   string // "chat" | "push"
	ChannelLabel  string // "Chat" | "Push"
	Value         string // the address (chat) or topic (push)
}

// subscriberPrefsView is the preferences-tab content for a subscriber.
type subscriberPrefsView struct {
	// Global holds the subscriber's global channel preferences.
	Global subscriberPrefRow
	// Workflows holds per-workflow overrides.
	Workflows []subscriberPrefRow
	// HasData is true when at least one preference row exists.
	HasData bool
}

// subscriberPrefRow is one row in the preferences table.
type subscriberPrefRow struct {
	// WorkflowID is empty for the global row.
	WorkflowID string
	// Label is a display label — "Global" for the global row, WorkflowID otherwise.
	Label string
	// Channels is the ordered list of per-channel toggle states.
	Channels []prefChannelState
}

// prefChannelState is one channel toggle in a preference row.
type prefChannelState struct {
	Channel string
	Enabled bool
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
		Signals:  subscribersSignalsJSON("", "", "", "", false),
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
		Header:   header,
		Fragment: frag,
		Signals:  subscribersSignalsJSON("", "", "", "", !c.deps.ReadOnly),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "subscribers", "Subscribers", view)
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

	var prefs subscriberPrefsView
	if tab == "preferences" {
		var prefsErr error
		prefs, prefsErr = c.buildSubscriberPreferences(r.Context(), scope, sub.SubscriberID)
		if prefsErr != nil {
			c.deps.Logger.Error("console: subscriber preferences", "err", prefsErr)
			// non-fatal: render page with empty prefs view
		}
	}

	var channels *subscriberChannelsView
	if tab == "channels" {
		var chanErr error
		channels, chanErr = c.buildSubscriberChannels(r.Context(), scope, sub.SubscriberID)
		if chanErr != nil {
			c.deps.Logger.Error("console: subscriber channels", "err", chanErr)
			// non-fatal: render page with whatever was assembled
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
		Preferences:   prefs,
		Channels:      channels,
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

// ─── channel addressing (chat/push delivery values) ──────────────────────────

// chatProviderIDs are the providers whose per-subscriber credential is a chat
// address, stored as {"address": value}. delivery/chat.go reads
// credentials.address.
var chatProviderIDs = map[string]bool{
	"discord":      true,
	"telegram":     true,
	"slack":        true,
	"whatsapp":     true,
	"chat-webhook": true,
}

// pushProviderIDs are the providers whose per-subscriber credential is a push
// topic/token, stored as {"topic": value}. delivery/push.go reads
// credentials.topic.
var pushProviderIDs = map[string]bool{
	"ntfy":         true,
	"expo":         true,
	"fcm":          true,
	"push-webhook": true,
}

// channelTypeForProvider returns "chat", "push", or "" for a provider id. The
// mapping is explicit (not derived from the catalog Channel string) because the
// catalog values are not a reliable type discriminator across providers.
func channelTypeForProvider(providerID string) string {
	switch {
	case chatProviderIDs[providerID]:
		return "chat"
	case pushProviderIDs[providerID]:
		return "push"
	default:
		return ""
	}
}

// channelCredentials builds the plaintext credentials JSON for a provider's
// addressable value: chat → {"address": value}, push → {"topic": value}.
func channelCredentials(providerID, value string) (channelType string, creds json.RawMessage, ok bool) {
	switch channelTypeForProvider(providerID) {
	case "chat":
		b, _ := json.Marshal(map[string]string{"address": value})
		return "chat", b, true
	case "push":
		b, _ := json.Marshal(map[string]string{"topic": value})
		return "push", b, true
	default:
		return "", nil, false
	}
}

// channelValueFromCredentials extracts the operator-visible value (address for
// chat, topic for push) from a stored channel's credentials JSON.
func channelValueFromCredentials(channelType string, creds json.RawMessage) string {
	if len(creds) == 0 {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal(creds, &m); err != nil {
		return ""
	}
	switch channelType {
	case "chat":
		return m["address"]
	case "push":
		return m["topic"]
	default:
		return ""
	}
}

// connectableChannelProviders returns the chat/push providers that have at
// least one active integration in scope, as ui-select options (value=provider
// id, label=catalog label), sorted by label. These are the only providers the
// add-form offers.
func (c *Console) connectableChannelProviders(ctx context.Context, scope domain.Scope) ([]uiSelectOption, error) {
	if c.deps.Integrations == nil {
		return nil, nil
	}
	integrations, err := c.deps.Integrations.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var opts []uiSelectOption
	for _, in := range integrations {
		if !in.Active {
			continue
		}
		if channelTypeForProvider(in.ProviderID) == "" {
			continue // not a chat/push addressable provider
		}
		if seen[in.ProviderID] {
			continue
		}
		seen[in.ProviderID] = true
		opts = append(opts, uiSelectOption{Value: in.ProviderID, Label: providerLabel(in.ProviderID)})
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].Label < opts[j].Label })
	return opts, nil
}

// buildSubscriberChannels assembles the channels-tab view: existing addresses
// (via ListBySubscriber) plus the connectable providers for the add-form.
func (c *Console) buildSubscriberChannels(ctx context.Context, scope domain.Scope, subscriberID string) (*subscriberChannelsView, error) {
	view := &subscriberChannelsView{}

	channels, err := c.deps.Audience.ListChannels(ctx, scope, subscriberID)
	if err != nil {
		return view, err
	}
	for _, ch := range channels {
		view.Existing = append(view.Existing, subscriberChannelRow{
			ProviderID:    ch.ProviderID,
			ProviderLabel: providerLabel(ch.ProviderID),
			ChannelType:   ch.Type,
			ChannelLabel:  channelLabel(ch.Type),
			Value:         channelValueFromCredentials(ch.Type, ch.Credentials),
		})
	}

	connectable, err := c.connectableChannelProviders(ctx, scope)
	if err != nil {
		return view, err
	}
	view.Connectable = connectable
	return view, nil
}

// buildSubscriberPreferences fetches the subscriber's stored preferences and
// returns a view struct for the Preferences tab. Absent preferences return a
// view with HasData=false (empty-state stub in the template).
func (c *Console) buildSubscriberPreferences(ctx context.Context, scope domain.Scope, subscriberID string) (subscriberPrefsView, error) {
	if c.deps.Preferences == nil {
		return subscriberPrefsView{}, nil
	}
	rows, err := c.deps.Preferences.GetForSubscriber(ctx, scope, subscriberID)
	if err != nil {
		return subscriberPrefsView{}, err
	}
	if len(rows) == 0 {
		return subscriberPrefsView{}, nil
	}

	// Known ordered channel names (display order).
	knownChannels := []string{"in_app", "email", "sms", "push"}

	var global subscriberPrefRow
	var workflows []subscriberPrefRow

	for _, p := range rows {
		// Decode channels JSON into map.
		chanMap := make(map[string]bool)
		if len(p.Channels) > 0 {
			_ = json.Unmarshal(p.Channels, &chanMap)
		}

		// Build ordered channel state list; absent key => enabled (true).
		var channels []prefChannelState
		seen := make(map[string]bool)
		for _, ch := range knownChannels {
			enabled := true
			if v, ok := chanMap[ch]; ok {
				enabled = v
			}
			channels = append(channels, prefChannelState{Channel: ch, Enabled: enabled})
			seen[ch] = true
		}
		// Append any extra channels not in the known list, sorted for stability.
		var extras []string
		for k := range chanMap {
			if !seen[k] {
				extras = append(extras, k)
			}
		}
		sort.Strings(extras)
		for _, ch := range extras {
			channels = append(channels, prefChannelState{Channel: ch, Enabled: chanMap[ch]})
		}

		if p.Type == "subscriberGlobal" {
			global = subscriberPrefRow{Label: "Global", Channels: channels}
		} else {
			label := p.WorkflowID
			if label == "" {
				label = "Unknown workflow"
			}
			workflows = append(workflows, subscriberPrefRow{WorkflowID: p.WorkflowID, Label: label, Channels: channels})
		}
	}

	return subscriberPrefsView{
		Global:    global,
		Workflows: workflows,
		HasData:   true,
	}, nil
}

// handleSubscriberPreferencesPatch handles POST /console/subscribers/{subscriberId}/preferences.
// Form fields: channel (the channel name), enabled ("true"/"false"), workflowId (empty = global).
// Native form POST → 303 redirect back to the Preferences tab.
func (c *Console) handleSubscriberPreferencesPatch(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleSubscriberPreferencesPatch: w is nil")
	}
	if r == nil {
		panic("handleSubscriberPreferencesPatch: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleSubscriberPreferencesPatch: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if c.deps.Preferences == nil {
		http.Error(w, "preferences service unavailable", http.StatusServiceUnavailable)
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

	// Resolve the subscriber's external ID from the internal ULID id.
	sub, err := c.deps.Audience.GetSubscriberByID(r.Context(), scope, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: preferences patch: get subscriber", "err", err)
		c.serveError(w, r)
		return
	}

	channel := strings.TrimSpace(r.FormValue("channel"))
	enabledStr := strings.TrimSpace(r.FormValue("enabled"))
	workflowID := strings.TrimSpace(r.FormValue("workflowId"))
	if channel == "" {
		http.Error(w, "channel is required", http.StatusBadRequest)
		return
	}
	enabled := enabledStr == "true" || enabledStr == "1"

	if workflowID == "" {
		// Global preference.
		// Load current global to merge (avoid clobbering other channels).
		existing, loadErr := c.deps.Preferences.GetForSubscriber(r.Context(), scope, sub.SubscriberID)
		chanMap := make(map[string]bool)
		for _, p := range existing {
			if p.Type == "subscriberGlobal" {
				if len(p.Channels) > 0 {
					_ = json.Unmarshal(p.Channels, &chanMap)
				}
			}
		}
		_ = loadErr
		chanMap[channel] = enabled
		if err := c.deps.Preferences.SetGlobal(r.Context(), scope, sub.SubscriberID, chanMap); err != nil {
			c.deps.Logger.Error("console: preferences patch: set global", "err", err)
			c.serveError(w, r)
			return
		}
	} else {
		// Per-workflow preference.
		existing, _ := c.deps.Preferences.GetForSubscriber(r.Context(), scope, sub.SubscriberID)
		chanMap := make(map[string]bool)
		for _, p := range existing {
			if p.Type == "workflow" && p.WorkflowID == workflowID {
				if len(p.Channels) > 0 {
					_ = json.Unmarshal(p.Channels, &chanMap)
				}
			}
		}
		chanMap[channel] = enabled
		if err := c.deps.Preferences.SetForWorkflow(r.Context(), scope, sub.SubscriberID, workflowID, chanMap); err != nil {
			c.deps.Logger.Error("console: preferences patch: set workflow", "err", err)
			c.serveError(w, r)
			return
		}
	}

	http.Redirect(w, r, "/console/subscribers/"+id+"?tab=preferences", http.StatusSeeOther)
}

// handleSubscriberChannels handles POST /console/subscribers/{subscriberId}/channels.
// Form fields: provider_id (a chat/push provider with an active integration),
// value (the chat address or push topic/token). Builds the plaintext
// credentials JSON per the chat/push mapping and upserts via Audience. Native
// form POST → 303 redirect back to the Channels tab.
func (c *Console) handleSubscriberChannels(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleSubscriberChannels: w is nil")
	}
	if r == nil {
		panic("handleSubscriberChannels: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleSubscriberChannels: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if c.deps.Audience == nil {
		http.Error(w, "audience service unavailable", http.StatusServiceUnavailable)
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

	// Resolve the subscriber's external ID from the internal ULID id.
	sub, err := c.deps.Audience.GetSubscriberByID(r.Context(), scope, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: channels patch: get subscriber", "err", err)
		c.serveError(w, r)
		return
	}

	providerID := strings.TrimSpace(r.FormValue("provider_id"))
	value := strings.TrimSpace(r.FormValue("value"))
	if providerID == "" {
		http.Error(w, "provider_id is required", http.StatusBadRequest)
		return
	}
	if value == "" {
		http.Error(w, "value is required", http.StatusBadRequest)
		return
	}

	// Validate the provider is a known chat/push addressable provider.
	channelType, creds, mapped := channelCredentials(providerID, value)
	if !mapped {
		http.Error(w, "unknown channel provider", http.StatusBadRequest)
		return
	}

	// Validate the provider has an active integration in scope.
	connectable, err := c.connectableChannelProviders(r.Context(), scope)
	if err != nil {
		c.deps.Logger.Error("console: channels patch: list connectable", "err", err)
		c.serveError(w, r)
		return
	}
	active := false
	for _, opt := range connectable {
		if opt.Value == providerID {
			active = true
			break
		}
	}
	if !active {
		http.Error(w, "provider has no active integration", http.StatusBadRequest)
		return
	}

	if _, err := c.deps.Audience.SetCredentials(r.Context(), scope, sub.SubscriberID, providerID, channelType, creds); err != nil {
		c.deps.Logger.Error("console: channels patch: set credentials", "err", err)
		c.serveError(w, r)
		return
	}

	http.Redirect(w, r, "/console/subscribers/"+id+"?tab=channels", http.StatusSeeOther)
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

	// Console-only convenience: a subscriber needs a stable, non-empty external
	// key (triggers address subscribers by it), but operators creating one by
	// hand don't always have an external id. When left blank, mint one
	// server-side using the same ULID generator the store uses for primary
	// keys, slugged with a readable "sub_" prefix. The /v1 API stays
	// strictly caller-provided.
	subID := strings.TrimSpace(r.FormValue("subscriberId"))
	if subID == "" {
		subID = "sub_" + strings.ToLower(store.NewULID())
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
