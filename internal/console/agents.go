// agents.go owns the Wave-3 console area: the agents CRUD pages, the agent
// detail tabs (overview / channels / conversations), and the conversation
// transcript + compose view. It follows the established console idioms — full
// server-rendered pages at routes, POST → 303 redirects for mutations,
// two-step inline delete confirmation (no native confirm()), CSRF on every
// mutating form, and server-side ReadOnly gating — over the agents store and
// the ingress compose port.
package console

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// ─── signal keys ──────────────────────────────────────────────────────────────

// agentsSignalKeys gates which envelope signals the agents-list fragment
// honours; convSignalKeys does the same for the conversation pager.
var (
	agentsSignalKeys = []string{"agentsFilter", "agentsPage"}
	convSignalKeys   = []string{"convPage"}
)

const agentConvPageSize = 20

// ─── view structs ─────────────────────────────────────────────────────────────

// agentsPageView binds templates/agents.html.
type agentsPageView struct {
	Header    pageHeader
	Fragment  agentsListView
	Signals   template.JS
	ReadOnly  bool
	FormError string   // surfaced from ?error= after a failed create
	Runtimes  []string // runtime <option> values; only implemented runtimes (domain.SupportedRuntimes, #103)
	CSRF      string
}

// RuntimeSelectOptions adapts the supported runtime values into the
// []uiSelectOption the ui-select component renders. Each runtime is both the
// value and the label (the catalog uses bare identifiers); no empty-value row
// is included so the placeholder stands for "nothing chosen" and server-side
// required validation rejects an empty submit (#103).
func (v agentsPageView) RuntimeSelectOptions() []uiSelectOption {
	opts := make([]uiSelectOption, 0, len(v.Runtimes))
	for _, rt := range v.Runtimes {
		opts = append(opts, uiSelectOption{Value: rt, Label: rt})
	}
	return opts
}

// agentsListView binds the agents-tbody fragment.
type agentsListView struct {
	Rows          []agentRow
	Filter        string
	ReadOnly      bool
	ConfirmDelete string // identifier of the row currently in confirm state
	CSRF          string
	Empty         *emptyState
}

type agentRow struct {
	ID         string
	Identifier string
	Name       string
	Runtime    string
	Active     bool
	Channels   int
	UpdatedAt  string
}

// agentDetailView binds templates/agent_detail.html.
type agentDetailView struct {
	Header        pageHeader
	Agent         agentdomain.Agent
	ActiveTab     string // "overview" | "channels" | "conversations"
	Signals       template.JS
	ReadOnly      bool
	ConfirmDelete bool
	FormError     string
	Links         []agentLinkRow             // channels tab
	Linkable      []agentLinkableIntegration // channels tab: integrations not yet linked
	ShowLink      bool                       // channels tab: render the link picker (?link=true)
	ConvsFragment agentConvsListView         // conversations tab
	CSRF          string
}

// LinkableSelectOptions adapts the linkable integrations into the
// []uiSelectOption the ui-select "Link channel" picker renders. The label
// mirrors the old <option> text ("identifier — Provider (Channel)"). No
// empty-value row: the placeholder stands for "choose an integration" and the
// server enforces the required field.
func (v agentDetailView) LinkableSelectOptions() []uiSelectOption {
	opts := make([]uiSelectOption, 0, len(v.Linkable))
	for _, li := range v.Linkable {
		opts = append(opts, uiSelectOption{
			Value: li.ID,
			Label: li.Identifier + " — " + li.ProviderLabel + " (" + li.ChannelLabel + ")",
		})
	}
	return opts
}

// agentLinkRow is one linked channel integration shown on the channels tab.
type agentLinkRow struct {
	IntegrationID string
	Identifier    string // integration identifier (human handle), "" when the row is gone
	ProviderLabel string // human provider name, e.g. "Telegram"
	ChannelLabel  string // human channel name, e.g. "Chat"
	IsChat        bool   // chat integrations expose an inbound webhook URL
	SetupURL      string // inbound webhook URL the operator registers with the provider
	ConnectedAt   string
}

// agentLinkableIntegration is one pickable integration in the "Link channel"
// form — the scope's integrations not yet linked to this agent.
type agentLinkableIntegration struct {
	ID            string
	Identifier    string
	ProviderLabel string
	ChannelLabel  string
}

type agentConvsListView struct {
	Identifier string
	Rows       []agentConvRow
	NextCursor string
	Empty      *emptyState
	ReadOnly   bool
}

type agentConvRow struct {
	ID               string
	Identifier       string // agent identifier (for URL building)
	Status           string
	LastActivityAt   string
	SubscriberID     string
	PlatformThreadID string
}

// conversationPageView binds templates/agent_conversation.html.
type conversationPageView struct {
	Header      pageHeader
	Agent       agentdomain.Agent
	Conv        agentdomain.Conversation
	Turns       []turnRow
	OlderCursor string // keyset cursor for "load older"
	Signals     template.JS
	ReadOnly    bool
	CanCompose  bool // Composer wired AND not read-only
	CSRF        string
}

type turnRow struct {
	ID        string
	Role      string // user|agent|system|action
	Text      string // decoded from content JSON {parts:[{text}]}
	RunURL    string // non-empty when role=agent and run_id exists
	CreatedAt string
}

// agentsSignalsJSON returns the Datastar signals for the agents list page.
// newOpen seeds the agentNewOpen signal; pass false for every route except
// /console/agents/new (which seeds it true so the slide-over opens immediately).
func agentsSignalsJSON(filter string, newOpen bool) template.JS {
	b, err := json.Marshal(map[string]any{"agentsFilter": filter, "agentsPage": "", "agentNewOpen": newOpen})
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

func convSignalsJSON() template.JS {
	b, err := json.Marshal(map[string]string{"convPage": ""})
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// ─── list builder ─────────────────────────────────────────────────────────────

// buildAgentRows lists agents in scope and resolves each one's channel count.
// filter, when non-empty, matches the identifier or name case-insensitively.
func (c *Console) buildAgentRows(r *http.Request, scope domain.Scope, filter string) ([]agentRow, error) {
	agents, err := c.deps.Agents.Agents().List(r.Context(), scope)
	if err != nil {
		return nil, err
	}
	lf := strings.ToLower(filter)
	rows := make([]agentRow, 0, len(agents))
	for _, a := range agents {
		if filter != "" &&
			!strings.Contains(strings.ToLower(a.Identifier), lf) &&
			!strings.Contains(strings.ToLower(a.Name), lf) {
			continue
		}
		links, _ := c.deps.Agents.Links().ListByAgent(r.Context(), scope, a.ID)
		rows = append(rows, agentRow{
			ID:         a.ID,
			Identifier: a.Identifier,
			Name:       a.Name,
			Runtime:    a.Runtime,
			Active:     a.Active,
			Channels:   len(links),
			UpdatedAt:  a.UpdatedAt,
		})
	}
	return rows, nil
}

// ─── page handlers ────────────────────────────────────────────────────────────

func (c *Console) serveAgentsPage(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveAgentsPage: no scope in context")
	}
	filter := r.URL.Query().Get("q")
	rows, err := c.buildAgentRows(r, scope, filter)
	if err != nil {
		c.deps.Logger.Error("console: list agents", "err", err)
		c.serveError(w, r)
		return
	}
	s, _ := sessionFromContext(r.Context())
	frag := agentsListView{Rows: rows, Filter: filter, ReadOnly: c.deps.ReadOnly, CSRF: s.CSRF}
	if len(rows) == 0 {
		frag.Empty = emptyAgentsState(c.deps.ReadOnly, filter != "")
	}
	pd := agentsPageView{
		Header:    pageHeader{Title: "Agents", Subtitle: "Conversational endpoints registered with this environment."},
		Fragment:  frag,
		Signals:   agentsSignalsJSON(filter, false),
		ReadOnly:  c.deps.ReadOnly,
		FormError: r.URL.Query().Get("error"),
		CSRF:      s.CSRF,
	}
	c.renderPage(w, r, http.StatusOK, "agents", "Agents", pd)
}

// serveAgentNew renders GET /console/agents/new: the list plus the create
// slide-over. Read-only consoles render the plain list (mutations are gated
// server-side regardless).
func (c *Console) serveAgentNew(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveAgentNew: no scope in context")
	}
	rows, err := c.buildAgentRows(r, scope, "")
	if err != nil {
		c.deps.Logger.Error("console: agents new page", "err", err)
		c.serveError(w, r)
		return
	}
	s, _ := sessionFromContext(r.Context())
	frag := agentsListView{Rows: rows, ReadOnly: c.deps.ReadOnly, CSRF: s.CSRF}
	if len(rows) == 0 {
		frag.Empty = emptyAgentsState(c.deps.ReadOnly, false)
	}
	pd := agentsPageView{
		Header:    pageHeader{Title: "Agents", Subtitle: "Conversational endpoints registered with this environment."},
		Fragment:  frag,
		Signals:   agentsSignalsJSON("", !c.deps.ReadOnly),
		ReadOnly:  c.deps.ReadOnly,
		FormError: r.URL.Query().Get("error"),
		Runtimes:  agentdomain.SupportedRuntimes,
		CSRF:      s.CSRF,
	}
	c.renderPage(w, r, http.StatusOK, "agents", "Agents", pd)
}

func emptyAgentsState(readOnly, filtered bool) *emptyState {
	if filtered {
		return &emptyState{
			Icon:        "🤖",
			Title:       "No agents found",
			Description: "Try adjusting the search filter.",
		}
	}
	es := &emptyState{
		Icon:        "🤖",
		Title:       "No agents yet",
		Description: "Create an agent to start building conversational experiences.",
		ReadOnly:    readOnly,
	}
	if !readOnly {
		es.PrimaryAction = &emptyStateAction{Label: "Create agent", OnClick: "$agentNewOpen = true"}
	}
	return es
}

// serveAgentDetailPage renders the agent detail page for every tab. The active
// tab comes from the URL path suffix (/tabs/{tab}); the bare /console/agents/
// {identifier} defaults to overview.
func (c *Console) serveAgentDetailPage(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveAgentDetailPage: no scope in context")
	}
	identifier := chi.URLParam(r, "identifier")
	if identifier == "" {
		c.serveNotFound(w, r)
		return
	}
	agent, err := c.deps.Agents.Agents().GetByIdentifier(r.Context(), scope, identifier)
	if err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: get agent", "identifier", identifier, "err", err)
		c.serveError(w, r)
		return
	}

	activeTab := "overview"
	switch {
	case strings.HasSuffix(r.URL.Path, "/tabs/channels"):
		activeTab = "channels"
	case strings.HasSuffix(r.URL.Path, "/tabs/conversations"):
		activeTab = "conversations"
	}

	var links []agentLinkRow
	var linkable []agentLinkableIntegration
	var convsFragment agentConvsListView

	switch activeTab {
	case "channels":
		links, linkable, err = c.buildAgentChannelsView(r, scope, agent.ID)
		if err != nil {
			c.deps.Logger.Error("console: list agent links", "identifier", identifier, "err", err)
			c.serveError(w, r)
			return
		}
	case "conversations":
		before := r.URL.Query().Get("before")
		convsFragment = c.buildAgentConvsView(r, scope, agent, before)
	}

	s, _ := sessionFromContext(r.Context())
	pd := agentDetailView{
		Header:        pageHeader{Title: agentTitle(agent)},
		Agent:         agent,
		ActiveTab:     activeTab,
		Signals:       agentsSignalsJSON("", false),
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete") == "true",
		FormError:     r.URL.Query().Get("error"),
		Links:         links,
		Linkable:      linkable,
		ShowLink:      !c.deps.ReadOnly && r.URL.Query().Get("link") == "true",
		ConvsFragment: convsFragment,
		CSRF:          s.CSRF,
	}
	c.renderPage(w, r, http.StatusOK, "agent-detail", agentTitle(agent), pd)
}

// buildAgentChannelsView assembles the channels tab: the linked-integration
// rows (enriched with provider/channel labels and, for chat integrations, the
// inbound setup URL) plus the set of scope integrations not yet linked (the
// link picker's options). Both reads are scope-guarded — an integration the
// scope cannot see is neither rendered as a link detail nor offered to link.
func (c *Console) buildAgentChannelsView(r *http.Request, scope domain.Scope, agentID string) ([]agentLinkRow, []agentLinkableIntegration, error) {
	rawLinks, err := c.deps.Agents.Links().ListByAgent(r.Context(), scope, agentID)
	if err != nil {
		return nil, nil, err
	}

	// Resolve integration metadata from the scope's integration list so the
	// channels tab shows provider/channel names instead of opaque IDs, and so
	// the picker only ever offers integrations within this tenant.
	integrations, err := c.deps.Integrations.List(r.Context(), scope)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]domain.Integration, len(integrations))
	for _, in := range integrations {
		byID[in.ID] = in
	}

	linked := make(map[string]struct{}, len(rawLinks))
	links := make([]agentLinkRow, 0, len(rawLinks))
	for _, l := range rawLinks {
		linked[l.IntegrationID] = struct{}{}
		row := agentLinkRow{
			IntegrationID: l.IntegrationID,
			ConnectedAt:   l.ConnectedAt,
		}
		if in, ok := byID[l.IntegrationID]; ok {
			row.Identifier = in.Identifier
			row.ProviderLabel = providerLabel(in.ProviderID)
			row.ChannelLabel = channelLabel(in.Channel)
			row.IsChat = in.Channel == "chat"
			if row.IsChat {
				row.SetupURL = c.channelSetupURL(l.IntegrationID)
			}
		}
		links = append(links, row)
	}

	linkable := make([]agentLinkableIntegration, 0, len(integrations))
	for _, in := range integrations {
		if _, ok := linked[in.ID]; ok {
			continue
		}
		linkable = append(linkable, agentLinkableIntegration{
			ID:            in.ID,
			Identifier:    in.Identifier,
			ProviderLabel: providerLabel(in.ProviderID),
			ChannelLabel:  channelLabel(in.Channel),
		})
	}
	return links, linkable, nil
}

// channelSetupURL returns the inbound webhook URL the operator registers with a
// chat provider (e.g. BotFather setWebhook). It mirrors the /v1 API's
// channelSetup contract exactly: <baseURL>/v1/inbound/<integrationID>, falling
// back to the "{base_url}" placeholder when the console has no public base URL
// configured (the acceptably-degraded path from Spec 08 Gap 2).
func (c *Console) channelSetupURL(integrationID string) string {
	base := strings.TrimRight(c.deps.PublicBaseURL, "/")
	if base == "" {
		base = "{base_url}"
	}
	return base + "/v1/inbound/" + integrationID
}

// buildAgentConvsView assembles the conversations-tab fragment for one agent.
func (c *Console) buildAgentConvsView(r *http.Request, scope domain.Scope, agent agentdomain.Agent, before string) agentConvsListView {
	convs, err := c.deps.Agents.Conversations().ListByAgent(r.Context(), scope, agent.ID, agentConvPageSize, before)
	if err != nil {
		c.deps.Logger.Error("console: list conversations", "identifier", agent.Identifier, "err", err)
	}
	rows := make([]agentConvRow, 0, len(convs))
	for _, conv := range convs {
		rows = append(rows, agentConvRow{
			ID:               conv.ID,
			Identifier:       agent.Identifier,
			Status:           conv.Status,
			LastActivityAt:   conv.LastActivityAt,
			SubscriberID:     conv.SubscriberID,
			PlatformThreadID: conv.PlatformThreadID,
		})
	}
	view := agentConvsListView{Identifier: agent.Identifier, Rows: rows, ReadOnly: c.deps.ReadOnly}
	if len(rows) == agentConvPageSize {
		view.NextCursor = rows[len(rows)-1].ID
	}
	if len(rows) == 0 {
		view.Empty = &emptyState{
			Icon:        "💬",
			Title:       "No conversations yet",
			Description: "Conversations appear here once the agent receives its first message.",
		}
	}
	return view
}

// serveConversationPage renders the conversation transcript + compose view.
func (c *Console) serveConversationPage(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveConversationPage: no scope in context")
	}
	identifier := chi.URLParam(r, "identifier")
	convID := chi.URLParam(r, "id")

	agent, err := c.deps.Agents.Agents().GetByIdentifier(r.Context(), scope, identifier)
	if err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: get agent for conversation", "identifier", identifier, "err", err)
		c.serveError(w, r)
		return
	}
	conv, err := c.deps.Agents.Conversations().Get(r.Context(), scope, convID)
	if err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: get conversation", "id", convID, "err", err)
		c.serveError(w, r)
		return
	}
	// Guard against cross-agent URL tampering: the conversation must belong
	// to the agent in the path.
	if conv.AgentID != agent.ID {
		c.serveNotFound(w, r)
		return
	}

	before := r.URL.Query().Get("before")
	activities, err := c.deps.Agents.Activities().Page(r.Context(), scope, convID, 50, before, "")
	if err != nil {
		c.deps.Logger.Error("console: page activities", "id", convID, "err", err)
		c.serveError(w, r)
		return
	}

	turns := make([]turnRow, 0, len(activities))
	for _, act := range activities {
		turns = append(turns, turnRow{
			ID:        act.ID,
			Role:      act.Role,
			Text:      extractActivityText(act.Content),
			RunURL:    c.runURL(act.RunID),
			CreatedAt: act.CreatedAt,
		})
	}
	var olderCursor string
	if len(activities) > 0 {
		olderCursor = activities[0].ID
	}

	s, _ := sessionFromContext(r.Context())
	pd := conversationPageView{
		Header:      pageHeader{Title: "Conversation"},
		Agent:       agent,
		Conv:        conv,
		Turns:       turns,
		OlderCursor: olderCursor,
		Signals:     convSignalsJSON(),
		ReadOnly:    c.deps.ReadOnly,
		CanCompose:  !c.deps.ReadOnly && c.deps.Composer != nil,
		CSRF:        s.CSRF,
	}
	c.renderPage(w, r, http.StatusOK, "agent-conversation", "Conversation", pd)
}

// extractActivityText decodes content JSON {parts:[{text:"..."},...]} and
// returns the concatenated text of all parts. Falls back to the raw JSON on
// decode error or when no parts carry text.
func extractActivityText(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	var body struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(content, &body); err != nil || len(body.Parts) == 0 {
		return string(content)
	}
	var sb strings.Builder
	for i, p := range body.Parts {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(p.Text)
	}
	return sb.String()
}

func agentTitle(a agentdomain.Agent) string {
	if a.Name != "" {
		return a.Name
	}
	return a.Identifier
}

// ─── fragment handlers ──────────────────────────────────────────────────────

func (c *Console) serveFragmentAgentsList(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentAgentsList: no scope in context")
	}
	params := mergeFragmentParams(r, agentsSignalKeys...)
	filter := first(params["q"])

	rows, err := c.buildAgentRows(r, scope, filter)
	if err != nil {
		c.deps.Logger.Error("console: fragment agents list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s, _ := sessionFromContext(r.Context())
	frag := agentsListView{
		Rows:          rows,
		Filter:        filter,
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: first(params["confirmDelete"]),
		CSRF:          s.CSRF,
	}
	if len(rows) == 0 {
		frag.Empty = emptyAgentsState(c.deps.ReadOnly, filter != "")
	}
	html, err := renderFragment(c.ts.base, "agents-tbody", frag)
	if err != nil {
		c.deps.Logger.Error("console: render agents-tbody", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

func (c *Console) serveFragmentAgentConversations(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentAgentConversations: no scope in context")
	}
	params := mergeFragmentParams(r, convSignalKeys...)
	identifier := first(params["identifier"])
	before := first(params["before"])

	agent, err := c.deps.Agents.Agents().GetByIdentifier(r.Context(), scope, identifier)
	if err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			http.Error(w, "agent not found", http.StatusNotFound)
			return
		}
		c.deps.Logger.Error("console: fragment conversations", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	frag := c.buildAgentConvsView(r, scope, agent, before)
	html, err := renderFragment(c.ts.base, "agent-conversations-tbody", frag)
	if err != nil {
		c.deps.Logger.Error("console: render agent-conversations-tbody", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// first returns the first value of a multi-value query slot, or "".
func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// ─── mutation handlers ──────────────────────────────────────────────────────

func (c *Console) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleAgentCreate: no scope in context")
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	identifier := strings.TrimSpace(r.FormValue("identifier"))
	if identifier == "" {
		redirectAgentNewErr(w, r, "Identifier is required.")
		return
	}
	runtime := strings.TrimSpace(r.FormValue("runtime"))
	// Reject runtimes without a registered executor — bridge/managed are
	// defined constants but unimplemented, so an agent saved with them would
	// dead-letter every turn (#103). Defense in depth behind the form, which
	// only offers domain.SupportedRuntimes.
	if !agentdomain.IsSupportedRuntime(runtime) {
		redirectAgentNewErr(w, r, "Unsupported runtime.")
		return
	}
	agent, err := c.deps.Agents.Agents().Create(r.Context(), scope, agentdomain.Agent{
		Identifier:     identifier,
		Name:           strings.TrimSpace(r.FormValue("name")),
		Description:    strings.TrimSpace(r.FormValue("description")),
		Runtime:        runtime,
		Active:         true,
		MeshSubject:    strings.TrimSpace(r.FormValue("meshSubject")),
		BridgeURL:      strings.TrimSpace(r.FormValue("bridgeUrl")),
		WelcomeMessage: strings.TrimSpace(r.FormValue("welcomeMessage")),
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			redirectAgentNewErr(w, r, "Identifier already exists.")
			return
		}
		c.deps.Logger.Error("console: create agent", "err", err)
		redirectAgentNewErr(w, r, "Create failed.")
		return
	}
	http.Redirect(w, r, "/console/agents/"+url.PathEscape(agent.Identifier), http.StatusSeeOther)
}

func redirectAgentNewErr(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/console/agents/new?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (c *Console) handleAgentSave(w http.ResponseWriter, r *http.Request) {
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleAgentSave: no scope in context")
	}
	identifier := chi.URLParam(r, "identifier")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	agent, err := c.deps.Agents.Agents().GetByIdentifier(r.Context(), scope, identifier)
	if err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.serveError(w, r)
		return
	}
	// Enforce supported-only-on-change: block switching TO an unimplemented
	// runtime, but leave a pre-existing unsupported agent (a row predating the
	// create guard, e.g. the seeded managed `ember`) editable on its other
	// fields. The edit form exposes no runtime selector, so a normal save sends
	// no "runtime" value and the stored runtime is left untouched; only a
	// crafted POST that changes runtime to an unsupported value is rejected
	// (#113 Part B relaxes #103's blanket stored-runtime guard).
	if rt := strings.TrimSpace(r.FormValue("runtime")); rt != "" && rt != agent.Runtime && !agentdomain.IsSupportedRuntime(rt) {
		http.Redirect(w, r, "/console/agents/"+url.PathEscape(identifier)+"?error="+url.QueryEscape("Unsupported runtime."), http.StatusSeeOther)
		return
	}
	agent.Name = strings.TrimSpace(r.FormValue("name"))
	agent.Description = strings.TrimSpace(r.FormValue("description"))
	agent.WelcomeMessage = strings.TrimSpace(r.FormValue("welcomeMessage"))
	agent.MeshSubject = strings.TrimSpace(r.FormValue("meshSubject"))
	agent.BridgeURL = strings.TrimSpace(r.FormValue("bridgeUrl"))
	if _, err := c.deps.Agents.Agents().Update(r.Context(), scope, agent); err != nil {
		c.deps.Logger.Error("console: save agent", "identifier", identifier, "err", err)
		http.Redirect(w, r, "/console/agents/"+url.PathEscape(identifier)+"?error="+url.QueryEscape("Save failed."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/console/agents/"+url.PathEscape(identifier), http.StatusSeeOther)
}

func (c *Console) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleAgentDelete: no scope in context")
	}
	identifier := chi.URLParam(r, "identifier")
	if err := c.deps.Agents.Agents().Delete(r.Context(), scope, identifier); err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: delete agent", "identifier", identifier, "err", err)
		c.serveError(w, r)
		return
	}
	http.Redirect(w, r, "/console/agents", http.StatusSeeOther)
}

func (c *Console) handleAgentToggleActive(w http.ResponseWriter, r *http.Request) {
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleAgentToggleActive: no scope in context")
	}
	identifier := chi.URLParam(r, "identifier")
	agent, err := c.deps.Agents.Agents().GetByIdentifier(r.Context(), scope, identifier)
	if err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.serveError(w, r)
		return
	}
	agent.Active = !agent.Active
	if _, err := c.deps.Agents.Agents().Update(r.Context(), scope, agent); err != nil {
		c.deps.Logger.Error("console: toggle agent active", "identifier", identifier, "err", err)
		c.serveError(w, r)
		return
	}
	http.Redirect(w, r, "/console/agents/"+url.PathEscape(identifier), http.StatusSeeOther)
}

// channelsTabURL is the 303 target both channel mutations redirect to. msg,
// when non-empty, is surfaced as a ?error= banner on the tab.
func channelsTabURL(identifier, msg string) string {
	u := "/console/agents/" + url.PathEscape(identifier) + "/tabs/channels"
	if msg != "" {
		u += "?error=" + url.QueryEscape(msg)
	}
	return u
}

// handleAgentChannelLink links the chosen channel integration to the agent and
// 303-redirects back to the channels tab. It is a thin wrapper over the same
// agents-store Links().Link the /v1 API uses: it resolves the agent in scope
// (404 on unknown), verifies the integration is visible in the same scope (so a
// foreign-tenant id cannot be linked), then upserts the link.
func (c *Console) handleAgentChannelLink(w http.ResponseWriter, r *http.Request) {
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleAgentChannelLink: no scope in context")
	}
	identifier := chi.URLParam(r, "identifier")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	agent, err := c.deps.Agents.Agents().GetByIdentifier(r.Context(), scope, identifier)
	if err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: link channel get agent", "identifier", identifier, "err", err)
		c.serveError(w, r)
		return
	}
	integrationID := strings.TrimSpace(r.FormValue("integrationId"))
	if integrationID == "" {
		http.Redirect(w, r, channelsTabURL(identifier, "Choose an integration to link."), http.StatusSeeOther)
		return
	}
	// Scope guard: only an integration visible in this tenant may be linked.
	// This rejects both unknown ids and cross-tenant ids without panicking.
	if _, err := c.deps.Integrations.GetByID(r.Context(), scope, integrationID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, channelsTabURL(identifier, "That integration was not found."), http.StatusSeeOther)
			return
		}
		c.deps.Logger.Error("console: link channel get integration", "id", integrationID, "err", err)
		c.serveError(w, r)
		return
	}
	if _, err := c.deps.Agents.Links().Link(r.Context(), scope, agentdomain.AgentLink{
		AgentID:       agent.ID,
		IntegrationID: integrationID,
	}); err != nil {
		c.deps.Logger.Error("console: link channel", "identifier", identifier, "integration", integrationID, "err", err)
		http.Redirect(w, r, channelsTabURL(identifier, "Link failed."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, channelsTabURL(identifier, ""), http.StatusSeeOther)
}

// handleAgentChannelUnlink removes the agent↔integration link and 303-redirects
// back to the channels tab. Thin wrapper over Links().Unlink; the store call is
// scope-guarded so a foreign-tenant link can never be removed here.
func (c *Console) handleAgentChannelUnlink(w http.ResponseWriter, r *http.Request) {
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleAgentChannelUnlink: no scope in context")
	}
	identifier := chi.URLParam(r, "identifier")
	integrationID := chi.URLParam(r, "integrationID")
	agent, err := c.deps.Agents.Agents().GetByIdentifier(r.Context(), scope, identifier)
	if err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: unlink channel get agent", "identifier", identifier, "err", err)
		c.serveError(w, r)
		return
	}
	if err := c.deps.Agents.Links().Unlink(r.Context(), scope, agent.ID, integrationID); err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			// Already gone (or never in scope) — converge on the tab quietly.
			http.Redirect(w, r, channelsTabURL(identifier, ""), http.StatusSeeOther)
			return
		}
		c.deps.Logger.Error("console: unlink channel", "identifier", identifier, "integration", integrationID, "err", err)
		http.Redirect(w, r, channelsTabURL(identifier, "Unlink failed."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, channelsTabURL(identifier, ""), http.StatusSeeOther)
}

// handleConversationCompose relays the conversation view's send-message form
// through the ingress compose port in-process, then 303-redirects back to the
// conversation (Wave 3: native POST → 303, no SSE).
func (c *Console) handleConversationCompose(w http.ResponseWriter, r *http.Request) {
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if c.deps.Composer == nil {
		http.Error(w, "compose service unavailable", http.StatusServiceUnavailable)
		return
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleConversationCompose: no scope in context")
	}
	identifier := chi.URLParam(r, "identifier")
	convID := chi.URLParam(r, "id")
	back := "/console/agents/" + url.PathEscape(identifier) + "/conversations/" + url.PathEscape(convID)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	conv, err := c.deps.Agents.Conversations().Get(r.Context(), scope, convID)
	if err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.serveError(w, r)
		return
	}
	if _, err := c.deps.Composer.ComposeMessage(r.Context(), scope, identifier, conv.PlatformThreadID, "operator", text); err != nil {
		c.deps.Logger.Error("console: compose message", "identifier", identifier, "conv", convID, "err", err)
		http.Redirect(w, r, back+"?error="+url.QueryEscape("Send failed."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
