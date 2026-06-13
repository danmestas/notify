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
	Header     pageHeader
	Fragment   agentsListView
	Signals    template.JS
	ReadOnly   bool
	ShowCreate bool   // true on /console/agents/new (renders the create slide-over)
	FormError  string // surfaced from ?error= after a failed create
	CSRF       string
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
	Links         []agentLinkRow     // channels tab
	ConvsFragment agentConvsListView // conversations tab
	CSRF          string
}

type agentLinkRow struct {
	IntegrationID string
	ConnectedAt   string
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
func agentsSignalsJSON(filter string) template.JS {
	b, err := json.Marshal(map[string]string{"agentsFilter": filter, "agentsPage": ""})
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
		Signals:   agentsSignalsJSON(filter),
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
		Header:     pageHeader{Title: "Agents", Subtitle: "Conversational endpoints registered with this environment."},
		Fragment:   frag,
		Signals:    agentsSignalsJSON(""),
		ReadOnly:   c.deps.ReadOnly,
		ShowCreate: !c.deps.ReadOnly,
		FormError:  r.URL.Query().Get("error"),
		CSRF:       s.CSRF,
	}
	c.renderPage(w, r, http.StatusOK, "agents", "New agent", pd)
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
		es.PrimaryAction = &emptyStateAction{Label: "Create agent", Href: "/console/agents/new"}
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
	var convsFragment agentConvsListView

	switch activeTab {
	case "channels":
		rawLinks, err := c.deps.Agents.Links().ListByAgent(r.Context(), scope, agent.ID)
		if err != nil {
			c.deps.Logger.Error("console: list agent links", "identifier", identifier, "err", err)
			c.serveError(w, r)
			return
		}
		links = make([]agentLinkRow, 0, len(rawLinks))
		for _, l := range rawLinks {
			links = append(links, agentLinkRow{
				IntegrationID: l.IntegrationID,
				ConnectedAt:   l.ConnectedAt,
			})
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
		Signals:       agentsSignalsJSON(""),
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete") == "true",
		FormError:     r.URL.Query().Get("error"),
		Links:         links,
		ConvsFragment: convsFragment,
		CSRF:          s.CSRF,
	}
	c.renderPage(w, r, http.StatusOK, "agent-detail", agentTitle(agent), pd)
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
	if !agentdomain.ValidRuntime(runtime) {
		redirectAgentNewErr(w, r, "Invalid runtime.")
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
