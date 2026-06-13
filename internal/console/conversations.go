// conversations.go owns the W4-A5 Global Conversations console area:
// the cross-agent conversation list at /console/conversations and its
// Datastar fragment endpoint. It follows the established console idioms —
// full server-rendered pages, GET-only fragments, CSRF on any POST form,
// scope-first, in-process service calls via console.Deps.
package console

import (
	"html/template"
	"net/http"

	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
)

// globalConvPageSize is the default page size for the global conversations list.
const globalConvPageSize = 20

// globalConvSignalKeys lists the Datastar envelope signals the
// global-conversations fragment honours for cursor-based paging.
var globalConvSignalKeys = []string{"globalConvCursor", "globalConvAgent"}

// ─── view structs ─────────────────────────────────────────────────────────────

// conversationsPageView binds templates/conversations.html.
type conversationsPageView struct {
	Header   pageHeader
	Fragment globalConvsListView
	Signals  template.JS
	ReadOnly bool
}

// globalConvsListView binds the global-conversations-tbody fragment.
type globalConvsListView struct {
	Rows        []globalConvRow
	NextCursor  string
	AgentFilter string // current agent filter value (for re-render)
	Empty       *emptyState
	ReadOnly    bool
}

// globalConvRow is one row in the cross-agent conversation table.
type globalConvRow struct {
	ConversationID   string
	AgentIdentifier  string
	AgentName        string // display name; falls back to AgentIdentifier
	SubscriberID     string
	PlatformThreadID string
	Status           string
	LastMessageAt    string
}

func (r globalConvRow) AgentDisplay() string {
	if r.AgentName != "" {
		return r.AgentName
	}
	return r.AgentIdentifier
}

// globalConvSignalsJSON returns the Datastar signals JSON blob for the page wrapper.
func globalConvSignalsJSON() template.JS {
	return template.JS(`{"globalConvCursor":"","globalConvAgent":""}`)
}

// ─── page handler ──────────────────────────────────────────────────────────────

// serveConversationsPage renders the full /console/conversations page.
func (c *Console) serveConversationsPage(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveConversationsPage: no scope in context")
	}

	agentFilter := r.URL.Query().Get("agent")
	cursor := r.URL.Query().Get("cursor")

	frag := c.buildGlobalConvsView(r, scope, agentFilter, cursor)
	pd := conversationsPageView{
		Header:   pageHeader{Title: "Conversations", Subtitle: "All agent conversations across this environment."},
		Fragment: frag,
		Signals:  globalConvSignalsJSON(),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "conversations", "Conversations", pd)
}

// ─── fragment handler ──────────────────────────────────────────────────────────

// serveFragmentGlobalConversations serves the Datastar fragment for the
// global conversations tbody. Endpoint: GET /console/api/fragments/global-conversations
func (c *Console) serveFragmentGlobalConversations(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentGlobalConversations: no scope in context")
	}

	params := mergeFragmentParams(r, globalConvSignalKeys...)
	cursor := first(params["globalConvCursor"])
	agentFilter := first(params["globalConvAgent"])

	frag := c.buildGlobalConvsView(r, scope, agentFilter, cursor)
	html, err := renderFragment(c.ts.base, "global-conversations-tbody", frag)
	if err != nil {
		c.deps.Logger.Error("console: render global-conversations-tbody", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── builder ──────────────────────────────────────────────────────────────────

// buildGlobalConvsView assembles the global conversations fragment view.
func (c *Console) buildGlobalConvsView(r *http.Request, scope domain.Scope, agentFilter, cursor string) globalConvsListView {
	summaries, nextCursor, err := c.deps.Agents.Activities().ListConversations(
		r.Context(), scope, agentFilter, cursor, globalConvPageSize,
	)
	if err != nil {
		c.deps.Logger.Error("console: list global conversations", "err", err)
		// Return empty view rather than failing the entire page.
		summaries = nil
	}

	rows := make([]globalConvRow, 0, len(summaries))
	for _, s := range summaries {
		rows = append(rows, globalConvRow{
			ConversationID:   s.ConversationID,
			AgentIdentifier:  s.AgentIdentifier,
			AgentName:        s.AgentName,
			SubscriberID:     s.SubscriberID,
			PlatformThreadID: s.PlatformThreadID,
			Status:           s.Status,
			LastMessageAt:    s.LastMessageAt,
		})
	}

	view := globalConvsListView{
		Rows:        rows,
		NextCursor:  nextCursor,
		AgentFilter: agentFilter,
		ReadOnly:    c.deps.ReadOnly,
	}
	if len(rows) == 0 {
		view.Empty = &emptyState{
			Icon:        "conversations",
			Title:       "No conversations yet",
			Description: "Agent conversations appear here once an agent handles a message.",
		}
		// ErrNotFound when Agents store is not wired (shouldn't happen in prod).
		_ = agentstore.ErrNotFound
	}
	return view
}
