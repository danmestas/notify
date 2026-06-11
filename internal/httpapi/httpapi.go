// Package httpapi assembles notify's public HTTP surface: subscriber upsert,
// event trigger, and the in-app message feed, all mounted behind
// identity.Middleware so every handler runs with an authenticated tenancy
// Scope on its request context.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbound"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/realtime"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/version"
)

// New builds the notify HTTP handler. All routes are grouped behind
// id.Middleware, so each handler resolves its Scope via identity.ScopeFrom.
func New(id *identity.Service, aud *audience.Service, trg *trigger.Service, inbox *inboxapi.Service, rt *realtime.Gateway, inb *inbound.Service, an *analytics.Service, ag *agentsapi.Service, ing *ingress.Service, conn *connectivity.Service, auth ...*authoring.Service) http.Handler {
	if len(auth) > 1 {
		panic("httpapi.New: at most one *authoring.Service")
	}
	var au *authoring.Service
	if len(auth) > 0 {
		au = auth[0]
	}
	r := chi.NewRouter()
	// Liveness probe, mounted outside the auth group so container/orchestrator
	// health checks need no API key.
	r.Get("/healthz", healthz)
	r.Group(func(pr chi.Router) {
		pr.Use(id.Middleware)
		pr.Post("/v1/subscribers", postSubscriber(aud))
		pr.Put("/v1/subscribers/{subscriberId}/credentials", putSubscriberCredentials(aud))
		pr.Post("/v1/events/trigger", postTrigger(trg))
		pr.Post("/v1/topics", postTopic(aud))
		pr.Post("/v1/topics/{key}/subscribers", postTopicSubscriber(aud))
		if conn != nil {
			pr.Post("/v1/integrations/push", postPushIntegration(conn))
			pr.Post("/v1/integrations/email", postEmailIntegration(conn))
		}
		if au != nil {
			pr.Post("/v1/workflows", postWorkflow(au))
			pr.Get("/v1/workflows", getWorkflows(au))
		}
		inbox.Routes(pr)
		if inb != nil {
			inb.Routes(pr)
		}
		if rt != nil {
			rt.Routes(pr)
		}
		if an != nil {
			an.Routes(pr)
		}
		if ag != nil {
			ag.Routes(pr)
		}
		if ing != nil {
			ing.Routes(pr)
		}
	})
	return r
}

// healthz is the unauthenticated liveness endpoint for container and
// orchestrator health checks: it reports 200 with the build version as JSON.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version.Version,
	})
}

// subscriberRequest is the POST /v1/subscribers body.
type subscriberRequest struct {
	SubscriberID string `json:"subscriberId"`
	Email        string `json:"email"`
	FirstName    string `json:"firstName"`
	LastName     string `json:"lastName"`
	Phone        string `json:"phone"`
	Locale       string `json:"locale"`
}

// postSubscriber upserts a subscriber within the request Scope and returns the
// persisted row as JSON with 201.
func postSubscriber(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req subscriberRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		sub, err := aud.Upsert(r.Context(), scope, audience.SubscriberInput{
			SubscriberID: req.SubscriberID,
			Email:        req.Email,
			FirstName:    req.FirstName,
			LastName:     req.LastName,
			Phone:        req.Phone,
			Locale:       req.Locale,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, sub)
	}
}

// subscriberCredentialsRequest is the PUT /v1/subscribers/{subscriberId}/credentials
// body: per-provider channel addressing, e.g.
// {"providerId":"ntfy","channel":"push","credentials":{"topic":"agent-alerts"}}.
type subscriberCredentialsRequest struct {
	ProviderID  string          `json:"providerId"`
	Channel     string          `json:"channel"`
	Credentials json.RawMessage `json:"credentials"`
}

// putSubscriberCredentials upserts the subscriber's channel credentials for a
// provider within the request Scope and returns 204.
func putSubscriberCredentials(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req subscriberCredentialsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ProviderID == "" {
			http.Error(w, "providerId is required", http.StatusBadRequest)
			return
		}
		subscriberID := chi.URLParam(r, "subscriberId")
		scope := identity.ScopeFrom(r.Context())
		if _, err := aud.SetCredentials(r.Context(), scope, subscriberID, req.ProviderID, req.Channel, req.Credentials); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// pushIntegrationRequest is the POST /v1/integrations/push body: the ntfy
// server credentials sealed into the integration row.
type pushIntegrationRequest struct {
	Identifier string `json:"identifier"`
	BaseURL    string `json:"baseUrl"`
	Token      string `json:"token"`
	Primary    bool   `json:"primary"`
}

// postPushIntegration creates an ntfy push integration within the request
// Scope and returns a slim DTO (no credential bytes) with 201.
func postPushIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req pushIntegrationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.BaseURL == "" {
			http.Error(w, "baseUrl is required", http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		in, err := conn.CreateNtfy(r.Context(), scope, req.Identifier, connectivity.NtfyConfig{
			BaseURL: req.BaseURL,
			Token:   req.Token,
		}, req.Primary)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id":         in.ID,
			"providerId": in.ProviderID,
			"channel":    in.Channel,
			"identifier": in.Identifier,
			"active":     in.Active,
			"primary":    in.Primary,
		})
	}
}

// postTrigger ingests a trigger within the request Scope and returns the
// TriggerResponse as JSON with 201.
func postTrigger(trg *trigger.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req trigger.TriggerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		resp, err := trg.Ingest(r.Context(), scope, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

// topicRequest is the POST /v1/topics body.
type topicRequest struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// postTopic creates (or upserts) a topic within the request Scope and returns
// the persisted row as JSON with 201.
func postTopic(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req topicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		topic, err := aud.CreateTopic(r.Context(), scope, req.Key, req.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, topic)
	}
}

// topicSubscriberRequest is the POST /v1/topics/{key}/subscribers body.
type topicSubscriberRequest struct {
	SubscriberID string `json:"subscriberId"`
}

// postTopicSubscriber adds a subscriber to the named topic within the request
// Scope. An unknown topic key surfaces store.ErrNotFound as 404.
func postTopicSubscriber(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req topicSubscriberRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())
		if err := aud.AddTopicSubscriber(r.Context(), scope, key, req.SubscriberID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// emailIntegrationRequest is the POST /v1/integrations/email body: SMTP
// credentials sealed into the integration row. Password is never echoed in the
// response DTO.
type emailIntegrationRequest struct {
	Identifier string `json:"identifier"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	From       string `json:"from"`
	Primary    bool   `json:"primary"`
}

// postEmailIntegration creates an SMTP email integration within the request
// Scope and returns a slim DTO (no credential bytes) with 201.
func postEmailIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req emailIntegrationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Host == "" {
			http.Error(w, "host is required", http.StatusBadRequest)
			return
		}
		if req.From == "" {
			http.Error(w, "from is required", http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		port := req.Port
		if port == 0 {
			port = 587
		}
		addr := req.Host + ":" + itoa(port)
		in, err := conn.CreateSMTP(r.Context(), scope, req.Identifier, connectivity.SMTPConfig{
			Addr:     addr,
			Host:     req.Host,
			Username: req.Username,
			Password: req.Password,
			From:     req.From,
		}, req.Primary)
		if err != nil {
			// Never surface the underlying error: wrapped marshal/encrypt/store
			// detail could carry credential-bearing config fields.
			http.Error(w, "failed to create email integration", http.StatusInternalServerError)
			return
		}
		// password is never echoed; return only non-sensitive fields.
		writeJSON(w, http.StatusCreated, map[string]any{
			"id":         in.ID,
			"providerId": in.ProviderID,
			"channel":    in.Channel,
			"identifier": in.Identifier,
			"active":     in.Active,
			"primary":    in.Primary,
		})
	}
}

// allowedStepTypes enumerates the step types the authoring layer understands.
var allowedStepTypes = []string{"in_app", "email", "push", "delay", "digest"}

// workflowStepRequest is one element of the POST /v1/workflows steps array.
type workflowStepRequest struct {
	StepID        string          `json:"stepId"`
	Type          string          `json:"type"`
	ControlValues json.RawMessage `json:"controlValues"`
}

// workflowRequest is the POST /v1/workflows body.
type workflowRequest struct {
	Name              string                `json:"name"`
	TriggerIdentifier string                `json:"triggerIdentifier"`
	Steps             []workflowStepRequest `json:"steps"`
}

// postWorkflow creates and publishes a workflow within the request Scope,
// returning the workflow id and trigger identifier with 201.
// Unknown step types produce a 422 that enumerates the allowed values.
func postWorkflow(au *authoring.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req workflowRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.TriggerIdentifier == "" {
			http.Error(w, "triggerIdentifier is required", http.StatusBadRequest)
			return
		}
		// Validate step types before touching the store.
		allowed := make(map[string]bool, len(allowedStepTypes))
		for _, t := range allowedStepTypes {
			allowed[t] = true
		}
		steps := make([]domain.Step, 0, len(req.Steps))
		for i, s := range req.Steps {
			if !allowed[s.Type] {
				msg := "step[" + itoa(i) + "].type " + s.Type +
					" is not valid; allowed: " + strings.Join(allowedStepTypes, ", ")
				http.Error(w, msg, http.StatusUnprocessableEntity)
				return
			}
			id := s.StepID
			if id == "" {
				id = s.Type + "-" + itoa(i)
			}
			steps = append(steps, domain.Step{
				StepID:        id,
				Type:          s.Type,
				ControlValues: s.ControlValues,
			})
		}
		scope := identity.ScopeFrom(r.Context())
		wf, err := au.Create(r.Context(), scope, authoring.WorkflowInput{
			Name:              req.Name,
			TriggerIdentifier: req.TriggerIdentifier,
			Steps:             steps,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := au.Publish(r.Context(), scope, req.TriggerIdentifier); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id":                wf.ID,
			"triggerIdentifier": wf.TriggerIdentifier,
			"name":              wf.Name,
			"active":            wf.Active,
		})
	}
}

// workflowDTO is the slim list representation returned by GET /v1/workflows.
type workflowDTO struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	TriggerIdentifier string `json:"triggerIdentifier"`
	Active            bool   `json:"active"`
}

// getWorkflows lists all workflows for the request Scope.
func getWorkflows(au *authoring.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		wfs, err := au.List(r.Context(), scope)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dtos := make([]workflowDTO, 0, len(wfs))
		for _, wf := range wfs {
			dtos = append(dtos, workflowDTO{
				ID:                wf.ID,
				Name:              wf.Name,
				TriggerIdentifier: wf.TriggerIdentifier,
				Active:            wf.Active,
			})
		}
		writeJSON(w, http.StatusOK, dtos)
	}
}

// itoa converts an int to a decimal string without importing strconv throughout
// handler code.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

// writeJSON encodes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
