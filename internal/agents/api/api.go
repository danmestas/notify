// Package api serves the agents management surface: CRUD on /v1/agents plus
// channel links on /v1/agents/{identifier}/integrations. Responses use the
// agent API's JSON format (package wire). It expects to run behind
// identity.Middleware so the request context carries a tenancy Scope.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/agents/wire"
	"github.com/danmestas/notify/internal/identity"
)

// runtimeErrMsg enumerates the runtimes a caller may actually create — those
// with a registered executor (agentdomain.SupportedRuntimes). bridge/managed
// are valid constants but unimplemented, so accepting them would mint an agent
// that dead-letters every turn (#113 tightens this from ValidRuntime). The list
// is derived from SupportedRuntimes so it never drifts; an empty enum list in a
// 422 is a misdiagnosis trap.
var runtimeErrMsg = "runtime must be one of: " + strings.Join(agentdomain.SupportedRuntimes, ", ")

// Service exposes agent management over HTTP.
type Service struct {
	store   *agentstore.Store
	baseURL string // optional; used by channelSetup to assemble inbound webhook URLs
}

// New builds a Service backed by the given agents store.
func New(s *agentstore.Store) *Service {
	return &Service{store: s}
}

// WithBaseURL sets the server's public base URL (e.g. "https://notify.example.com")
// so that channelSetup can return a ready-to-use inbound webhook URL. When
// omitted the endpoint returns a templated placeholder the operator fills in.
func (svc *Service) WithBaseURL(u string) *Service {
	svc.baseURL = strings.TrimRight(u, "/")
	return svc
}

// Routes registers the agent endpoints on r. It expects to run behind
// identity.Middleware so the request context carries a tenancy Scope.
func (svc *Service) Routes(r chi.Router) {
	r.Post("/v1/agents", svc.createAgent)
	r.Get("/v1/agents", svc.listAgents)
	r.Get("/v1/agents/{identifier}", svc.getAgent)
	r.Patch("/v1/agents/{identifier}", svc.patchAgent)
	r.Delete("/v1/agents/{identifier}", svc.deleteAgent)
	r.Post("/v1/agents/{identifier}/integrations", svc.linkIntegration)
	r.Get("/v1/agents/{identifier}/integrations", svc.listIntegrations)
	r.Delete("/v1/agents/{identifier}/integrations/{integrationID}", svc.unlinkIntegration)
	// Wave 3 additions.
	r.Get("/v1/agents/{identifier}/conversations", svc.listConversations)
	r.Get("/v1/agents/{identifier}/channels/{integrationID}/setup", svc.channelSetup)
	// Wave 4: global cross-agent conversations (GAP-AF-07).
	r.Get("/v1/inbox/conversations", svc.listGlobalConversations)
}

// agentRequest is the POST /v1/agents body.
type agentRequest struct {
	Identifier     string          `json:"identifier"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Runtime        string          `json:"runtime"`
	Active         *bool           `json:"active"`
	MeshSubject    string          `json:"meshSubject"`
	BridgeURL      string          `json:"bridgeUrl"`
	Managed        json.RawMessage `json:"managed"`
	WelcomeMessage string          `json:"welcomeMessage"`
}

// createAgent persists a new agent within the request Scope and returns the
// stored row as JSON with 201. An invalid runtime answers 422 with the
// allowed values enumerated.
func (svc *Service) createAgent(w http.ResponseWriter, r *http.Request) {
	var req agentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Identifier == "" {
		http.Error(w, "identifier is required", http.StatusUnprocessableEntity)
		return
	}
	// Reject identifiers outside the URL-safe slug allowlist — the identifier
	// is used verbatim as a console URL path segment (view/edit/delete/save
	// routes), so an unvalidated identifier would create an agent every
	// console route 404s on, permanently orphaning it (same class as H2).
	if !agentdomain.ValidIdentifier(req.Identifier) {
		http.Error(w, "identifier may only contain lowercase letters, numbers, hyphens, and underscores", http.StatusUnprocessableEntity)
		return
	}
	// Enforce supported-on-creation: only runtimes with a registered executor
	// may be minted, otherwise the API can create a dead agent (#113 Part A).
	if !agentdomain.IsSupportedRuntime(req.Runtime) {
		http.Error(w, runtimeErrMsg, http.StatusUnprocessableEntity)
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	scope := identity.ScopeFrom(r.Context())
	agent, err := svc.store.Agents().Create(r.Context(), scope, agentdomain.Agent{
		Identifier:     req.Identifier,
		Name:           req.Name,
		Description:    req.Description,
		Runtime:        req.Runtime,
		Active:         active,
		MeshSubject:    req.MeshSubject,
		BridgeURL:      req.BridgeURL,
		Managed:        req.Managed,
		WelcomeMessage: req.WelcomeMessage,
	})
	if err != nil {
		// The agents (environment_id, identifier) UNIQUE constraint makes a
		// duplicate create a caller conflict, not a server fault. (String
		// match mirrors the core store's "no such table" check.)
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			http.Error(w, "agent identifier already exists", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, wire.FromAgent(agent))
}

// listAgents returns every agent in the request Scope.
func (svc *Service) listAgents(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	agents, err := svc.store.Agents().List(r.Context(), scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, wire.Agents(agents))
}

// getAgent returns the agent addressed by {identifier} or 404.
func (svc *Service) getAgent(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	agent, err := svc.store.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "identifier"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wire.FromAgent(agent))
}

// agentPatch is the PATCH /v1/agents/{identifier} body. Pointer fields
// distinguish "absent" from "set to zero value" so a partial update only
// touches what the caller sent.
type agentPatch struct {
	Name           *string          `json:"name"`
	Description    *string          `json:"description"`
	Runtime        *string          `json:"runtime"`
	Active         *bool            `json:"active"`
	MeshSubject    *string          `json:"meshSubject"`
	BridgeURL      *string          `json:"bridgeUrl"`
	Managed        *json.RawMessage `json:"managed"`
	WelcomeMessage *string          `json:"welcomeMessage"`
}

// patchAgent applies a partial update to the agent addressed by {identifier}
// and returns the updated row. An invalid runtime answers 422 with the
// allowed values enumerated; an unknown identifier answers 404.
func (svc *Service) patchAgent(w http.ResponseWriter, r *http.Request) {
	var req agentPatch
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	scope := identity.ScopeFrom(r.Context())
	agent, err := svc.store.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "identifier"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Enforce supported-only-on-change: a runtime switch must target a runtime
	// with a registered executor, but a no-op runtime field (or one that equals
	// the stored value) leaves a legacy unsupported agent patchable on its other
	// fields (#113 — mirrors the relaxed console save guard).
	if req.Runtime != nil && *req.Runtime != agent.Runtime && !agentdomain.IsSupportedRuntime(*req.Runtime) {
		http.Error(w, runtimeErrMsg, http.StatusUnprocessableEntity)
		return
	}
	if req.Name != nil {
		agent.Name = *req.Name
	}
	if req.Description != nil {
		agent.Description = *req.Description
	}
	if req.Runtime != nil {
		agent.Runtime = *req.Runtime
	}
	if req.Active != nil {
		agent.Active = *req.Active
	}
	if req.MeshSubject != nil {
		agent.MeshSubject = *req.MeshSubject
	}
	if req.BridgeURL != nil {
		agent.BridgeURL = *req.BridgeURL
	}
	if req.Managed != nil {
		agent.Managed = *req.Managed
	}
	if req.WelcomeMessage != nil {
		agent.WelcomeMessage = *req.WelcomeMessage
	}
	updated, err := svc.store.Agents().Update(r.Context(), scope, agent)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wire.FromAgent(updated))
}

// deleteAgent removes the agent addressed by {identifier}, answering 204 on
// success and 404 when it does not exist.
func (svc *Service) deleteAgent(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	if err := svc.store.Agents().Delete(r.Context(), scope, chi.URLParam(r, "identifier")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linkRequest is the POST /v1/agents/{identifier}/integrations body.
type linkRequest struct {
	IntegrationID string          `json:"integrationId"`
	Config        json.RawMessage `json:"config"`
}

// linkIntegration connects the agent addressed by {identifier} to a channel
// integration and returns the stored link with 201. Re-linking the same
// integration updates its config in place.
func (svc *Service) linkIntegration(w http.ResponseWriter, r *http.Request) {
	var req linkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.IntegrationID == "" {
		http.Error(w, "integrationId is required", http.StatusUnprocessableEntity)
		return
	}
	scope := identity.ScopeFrom(r.Context())
	agent, err := svc.store.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "identifier"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	link, err := svc.store.Links().Link(r.Context(), scope, agentdomain.AgentLink{
		AgentID:       agent.ID,
		IntegrationID: req.IntegrationID,
		Config:        req.Config,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, wire.FromLink(link))
}

// listIntegrations returns the agent's channel links.
func (svc *Service) listIntegrations(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	agent, err := svc.store.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "identifier"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	links, err := svc.store.Links().ListByAgent(r.Context(), scope, agent.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, wire.Links(links))
}

// unlinkIntegration removes the link between the agent addressed by
// {identifier} and {integrationID}, answering 204 on success and 404 when
// either the agent or the link does not exist.
func (svc *Service) unlinkIntegration(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	agent, err := svc.store.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "identifier"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := svc.store.Links().Unlink(r.Context(), scope, agent.ID, chi.URLParam(r, "integrationID")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listConversations returns the agent's conversations ordered by
// last_activity_at DESC with keyset pagination.
//
// GET /v1/agents/{identifier}/conversations?limit=50&before=<ulid>
func (svc *Service) listConversations(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	agent, err := svc.store.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "identifier"))
	if err != nil {
		writeStoreError(w, err)
		return
	}

	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 0)
	before := q.Get("before")

	convs, err := svc.store.Conversations().ListByAgent(r.Context(), scope, agent.ID, limit, before)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, wire.Conversations(convs))
}

// parseLimit converts a raw query string to an int. Returns fallback on any
// parse error or when s is empty.
func parseLimit(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return fallback
		}
		v = v*10 + int(c-'0')
	}
	return v
}

// channelSetupResponse is the body of GET
// /v1/agents/{identifier}/channels/{integrationID}/setup.
type channelSetupResponse struct {
	WebhookURL string `json:"webhookUrl"`
}

// channelSetup returns the inbound webhook URL the operator pastes into
// BotFather (or any channel provider's webhook registration UI). The URL
// pattern is: <baseURL>/v1/inbound/<integrationID>. When the service has no
// baseURL configured (i.e. the operator hasn't set the public host), the
// response contains a templated placeholder they can fill in.
//
// GET /v1/agents/{identifier}/channels/{integrationID}/setup
func (svc *Service) channelSetup(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())

	// Resolve agent — yields 404 for unknowns or wrong scope.
	agent, err := svc.store.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "identifier"))
	if err != nil {
		writeStoreError(w, err)
		return
	}

	integrationID := chi.URLParam(r, "integrationID")

	// Verify the agent-channel link exists within scope before returning the URL.
	links, err := svc.store.Links().ListByAgent(r.Context(), scope, agent.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	found := false
	for _, l := range links {
		if l.IntegrationID == integrationID {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	base := svc.baseURL
	if base == "" {
		base = "{base_url}"
	}
	writeJSON(w, http.StatusOK, channelSetupResponse{
		WebhookURL: base + "/v1/inbound/" + integrationID,
	})
}

// listGlobalConversations returns the cross-agent conversation list for the
// current scope. No {agentIdentifier} path segment — this is the global inbox
// view required by Activity Feed §3.3.1 (GAP-AF-07).
//
// GET /v1/inbox/conversations?cursor=<ulid>&limit=20&agent=<identifier>
func (svc *Service) listGlobalConversations(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())

	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 0)
	cursor := q.Get("cursor")
	agentIdentifier := q.Get("agent")

	items, next, err := svc.store.Activities().ListConversations(r.Context(), scope, agentIdentifier, cursor, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": wire.ConversationSummaries(items),
		"next":  next,
	})
}

// writeStoreError maps a repository error onto its HTTP status: ErrNotFound
// answers 404, anything else 500.
func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, agentstore.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// writeJSON encodes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
