package console

// subscriber_detail.go implements W2-C3 (subscriber detail), W2-C5
// (subscriber activity tab), and W4-S1 (subscriber preferences + channels
// tab) for the operator console.
//
// The subscriber list and create/update/delete mutations live in
// subscribers.go; the shared custom-field editor helpers live in
// subscriber_custom_fields.go.
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
	"net/url"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

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
	// FormError is an inline validation error surfaced from ?error= after a
	// failed profile update (e.g. malformed email, over-length field).
	// Rendered in the overview tab next to the edit-profile form.
	FormError string
	// CustomFields is the read-only sorted list of Sub.Data entries, rendered
	// in the profile card. Empty when the subscriber has no custom data.
	CustomFields []subscriberCustomFieldRow
	// CustomFieldsEditor seeds the edit-profile form's repeatable custom-field
	// rows, pre-populated from Sub.Data.
	CustomFieldsEditor subscriberCustomFieldsEditorView
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
	// FormError is an inline validation error surfaced from ?error= after a
	// failed save (e.g. missing provider_id). Rendered in the channels tab.
	FormError string
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
		channels, chanErr = c.buildSubscriberChannels(r.Context(), scope, sub.SubscriberID, r.URL.Query().Get("error"))
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
		Header:             header,
		Sub:                sub,
		Activity:           activity,
		Preferences:        prefs,
		Channels:           channels,
		Signals:            template.JS(sigs),
		ReadOnly:           c.deps.ReadOnly,
		ActiveTab:          tab,
		ConfirmDelete:      r.URL.Query().Get("confirmDelete") == "true",
		FormError:          r.URL.Query().Get("error"),
		CustomFields:       decodeSubscriberCustomFields(sub.Data),
		CustomFieldsEditor: subscriberCustomFieldsEditor(sub.Data),
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
// formError is threaded from ?error= on the detail URL after a failed POST.
func (c *Console) buildSubscriberChannels(ctx context.Context, scope domain.Scope, subscriberID, formError string) (*subscriberChannelsView, error) {
	view := &subscriberChannelsView{FormError: formError}

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
		// Missing provider_id: redirect back to the channels tab with an
		// inline error rather than a raw text/plain response.
		chanTabURL := "/console/subscribers/" + id + "?tab=channels"
		http.Redirect(w, r, chanTabURL+"&error="+url.QueryEscape("provider_id is required"), http.StatusSeeOther)
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
