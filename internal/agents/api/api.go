// Package api serves the agents management surface: CRUD on /v1/agents plus
// channel links on /v1/agents/{identifier}/integrations. It returns store
// types directly (no response re-shaping) and expects to run behind
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
	"github.com/danmestas/notify/internal/identity"
)

// runtimeErrMsg deliberately enumerates the allowed runtime values: an empty
// enum list in a 422 is a misdiagnosis trap .
const runtimeErrMsg = "runtime must be one of: mesh, bridge, managed"

// Service exposes agent management over HTTP.
type Service struct {
	store *agentstore.Store
}

// New builds a Service backed by the given agents store.
func New(s *agentstore.Store) *Service {
	return &Service{store: s}
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
	if !agentdomain.ValidRuntime(req.Runtime) {
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
	writeJSON(w, http.StatusCreated, agent)
}

// listAgents returns every agent in the request Scope.
func (svc *Service) listAgents(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	agents, err := svc.store.Agents().List(r.Context(), scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if agents == nil {
		agents = []agentdomain.Agent{}
	}
	writeJSON(w, http.StatusOK, agents)
}

// getAgent returns the agent addressed by {identifier} or 404.
func (svc *Service) getAgent(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	agent, err := svc.store.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "identifier"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
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
	if req.Runtime != nil && !agentdomain.ValidRuntime(*req.Runtime) {
		http.Error(w, runtimeErrMsg, http.StatusUnprocessableEntity)
		return
	}
	scope := identity.ScopeFrom(r.Context())
	agent, err := svc.store.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "identifier"))
	if err != nil {
		writeStoreError(w, err)
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
	writeJSON(w, http.StatusOK, updated)
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
	writeJSON(w, http.StatusCreated, link)
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
	if links == nil {
		links = []agentdomain.AgentLink{}
	}
	writeJSON(w, http.StatusOK, links)
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
