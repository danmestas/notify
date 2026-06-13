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
	"time"

	"github.com/go-chi/chi/v5"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/contexts"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/envvars"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbound"
	"github.com/danmestas/notify/internal/inbounddomains"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/realtime"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/version"
	"github.com/danmestas/notify/internal/webhooksout"
)

// New builds the notify HTTP handler. All routes are grouped behind
// id.Middleware, so each handler resolves its Scope via identity.ScopeFrom.
// st may be nil; when non-nil it enables the console read surface routes
// (GET /v1/notifications, GET /v1/notifications/{id}).
// wh may be nil; when non-nil it enables webhook-endpoint management routes
// (GET/POST /v1/webhooks/endpoints, GET/PUT/DELETE /v1/webhooks/endpoints/{id}).
func New(id *identity.Service, aud *audience.Service, trg *trigger.Service, inbox *inboxapi.Service, rt *realtime.Gateway, inb *inbound.Service, an *analytics.Service, ag *agentsapi.Service, ing *ingress.Service, conn *connectivity.Service, st *store.Store, ev *envvars.Service, auth ...*authoring.Service) http.Handler {
	if len(auth) > 1 {
		panic("httpapi.New: at most one *authoring.Service")
	}
	var au *authoring.Service
	if len(auth) > 0 {
		au = auth[0]
	}
	// Wire a webhook management service when a store is present.
	var wh *webhooksout.ManagementService
	if st != nil {
		wh = webhooksout.NewManagement(st)
	}
	// Wire the contexts service when a store is present (#54).
	var ctxSvc *contexts.Service
	if st != nil {
		ctxSvc = contexts.New(st)
	}
	// Wire the inbound domains service when a store is present (#53).
	var inboundDomainSvc *inbounddomains.Service
	if st != nil {
		inboundDomainSvc = inbounddomains.New(st)
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
		// Wave-2 subscriber management (#34).
		pr.Get("/v1/subscribers", getSubscribers(aud))
		pr.Get("/v1/subscribers/{id}", getSubscriber(aud))
		pr.Patch("/v1/subscribers/{id}", patchSubscriber(aud))
		pr.Delete("/v1/subscribers/{id}", deleteSubscriber(aud))
		// Wave-2 topic management (#35).
		pr.Get("/v1/topics", getTopics(aud))
		pr.Get("/v1/topics/{key}", getTopic(aud))
		pr.Put("/v1/topics/{key}", putTopic(aud))
		pr.Delete("/v1/topics/{key}", deleteTopic(aud))
		pr.Delete("/v1/topics/{key}/subscribers/{subscriberId}", deleteTopicSubscriber(aud))
		if conn != nil {
			// Legacy channel-specific create endpoints (API back-compat).
			pr.Post("/v1/integrations/push", postPushIntegration(conn))
			pr.Post("/v1/integrations/email", postEmailIntegration(conn))
			// CRUD + unified create (Wave 2).
			pr.Get("/v1/integrations", getIntegrations(conn))
			pr.Post("/v1/integrations", postIntegration(conn))
			pr.Get("/v1/integrations/{id}", getIntegration(conn))
			pr.Put("/v1/integrations/{id}", putIntegration(conn))
			pr.Delete("/v1/integrations/{id}", deleteIntegration(conn))
			pr.Patch("/v1/integrations/{id}/active", patchIntegrationActive(conn))
		}
		if au != nil {
			pr.Post("/v1/workflows", postWorkflow(au))
			pr.Get("/v1/workflows", getWorkflows(au))
			pr.Get("/v1/workflows/{slug}", getWorkflow(au))
			pr.Put("/v1/workflows/{slug}", putWorkflow(au))
			pr.Delete("/v1/workflows/{slug}", deleteWorkflow(au))
			pr.Patch("/v1/workflows/{slug}/activate", patchWorkflowActivate(au))
			pr.Post("/v1/workflows/{slug}/duplicate", postWorkflowDuplicate(au))
		}
		if st != nil {
			pr.Get("/v1/notifications", getNotifications(st))
			pr.Get("/v1/notifications/{id}", getNotification(st))
			// Layouts management (W4-L1).
			layouts := st.Layouts()
			pr.Get("/v1/layouts", getLayouts(layouts))
			pr.Post("/v1/layouts", postLayout(layouts))
			pr.Get("/v1/layouts/{key}", getLayout(layouts))
			pr.Put("/v1/layouts/{key}", putLayout(layouts))
			pr.Delete("/v1/layouts/{key}", deleteLayout(layouts))
		}
		// Wave-2 webhook endpoint management (#36).
		if wh != nil {
			pr.Get("/v1/webhooks/endpoints", getWebhookEndpoints(wh))
			pr.Post("/v1/webhooks/endpoints", postWebhookEndpoint(wh))
			pr.Get("/v1/webhooks/endpoints/{id}", getWebhookEndpoint(wh))
			pr.Put("/v1/webhooks/endpoints/{id}", putWebhookEndpoint(wh))
			pr.Delete("/v1/webhooks/endpoints/{id}", deleteWebhookEndpoint(wh))
		}
		// W4-C1 contexts management (#54).
		if ctxSvc != nil {
			pr.Get("/v1/contexts", getContexts(ctxSvc))
			pr.Post("/v1/contexts", postContext(ctxSvc))
			pr.Get("/v1/contexts/{type}/{id}", getContext(ctxSvc))
			pr.Put("/v1/contexts/{type}/{id}", putContext(ctxSvc))
			pr.Delete("/v1/contexts/{type}/{id}", deleteContext(ctxSvc))
		}
		// W4-V1 environment variables management.
		if ev != nil {
			pr.Get("/v1/env-vars", getEnvVars(ev))
			pr.Post("/v1/env-vars", postEnvVar(ev))
			pr.Put("/v1/env-vars/{key}", putEnvVar(ev))
			pr.Delete("/v1/env-vars/{key}", deleteEnvVar(ev))
		}
		// W4-D1 inbound domain management (#53).
		if inboundDomainSvc != nil {
			pr.Get("/v1/inbound/domains", getInboundDomains(inboundDomainSvc))
			pr.Post("/v1/inbound/domains", postInboundDomain(inboundDomainSvc))
			pr.Get("/v1/inbound/domains/{name}", getInboundDomain(inboundDomainSvc))
			pr.Post("/v1/inbound/domains/{name}/verify", postInboundDomainVerify(inboundDomainSvc))
			pr.Delete("/v1/inbound/domains/{name}", deleteInboundDomain(inboundDomainSvc))
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
// Scope. An unknown topic key surfaces store.ErrNotFound as 404. The
// subscriberId must be non-empty and reference an existing subscriber in
// scope — memberships can never point at nothing (empty → 400, unknown →
// 422).
func postTopicSubscriber(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req topicSubscriberRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		subID := strings.TrimSpace(req.SubscriberID)
		if subID == "" {
			http.Error(w, "subscriberId is required", http.StatusBadRequest)
			return
		}
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())
		if _, err := aud.Get(r.Context(), scope, subID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "subscriber not found", http.StatusUnprocessableEntity)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := aud.AddTopicSubscriber(r.Context(), scope, key, subID); err != nil {
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

// emailIntegrationRequest is the POST /v1/integrations/email body.
// The optional "provider" field selects the backend (default: "smtp").
// For provider="smtp": host, port, username, password are used.
// For provider="cloudflare-email": accountId, apiToken are used.
// Password and apiToken are never echoed in the response DTO.
type emailIntegrationRequest struct {
	Identifier string `json:"identifier"`
	Provider   string `json:"provider"` // "smtp" (default) or "cloudflare-email"
	From       string `json:"from"`
	Primary    bool   `json:"primary"`
	// SMTP fields.
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	// Cloudflare Email fields.
	AccountID string `json:"accountId"`
	APIToken  string `json:"apiToken"`
}

// postEmailIntegration creates an email integration within the request Scope
// and returns a slim DTO (no credential bytes) with 201.
// The "provider" field selects between smtp (default) and cloudflare-email;
// the existing SMTP body is accepted unchanged when provider is absent.
func postEmailIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req emailIntegrationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.From == "" {
			http.Error(w, "from is required", http.StatusBadRequest)
			return
		}

		scope := identity.ScopeFrom(r.Context())

		var (
			in  domain.Integration
			err error
		)

		switch req.Provider {
		case "cloudflare-email":
			if req.AccountID == "" {
				http.Error(w, "accountId is required for cloudflare-email", http.StatusBadRequest)
				return
			}
			if req.APIToken == "" {
				http.Error(w, "apiToken is required for cloudflare-email", http.StatusBadRequest)
				return
			}
			in, err = conn.CreateCloudflareEmail(r.Context(), scope, req.Identifier, connectivity.CloudflareEmailConfig{
				AccountID: req.AccountID,
				APIToken:  req.APIToken,
				From:      req.From,
			}, req.Primary)
		case "smtp", "": // empty provider defaults to smtp for backward compatibility.
			if req.Host == "" {
				http.Error(w, "host is required", http.StatusBadRequest)
				return
			}
			port := req.Port
			if port == 0 {
				port = 587
			}
			addr := req.Host + ":" + itoa(port)
			in, err = conn.CreateSMTP(r.Context(), scope, req.Identifier, connectivity.SMTPConfig{
				Addr:     addr,
				Host:     req.Host,
				Username: req.Username,
				Password: req.Password,
				From:     req.From,
			}, req.Primary)
		default:
			// Fail closed on unknown providers rather than silently storing
			// them as SMTP rows with empty credentials.
			http.Error(w, "unknown provider "+req.Provider+"; allowed: smtp, cloudflare-email", http.StatusBadRequest)
			return
		}

		if err != nil {
			// Never surface the underlying error: wrapped marshal/encrypt/store
			// detail could carry credential-bearing config fields.
			http.Error(w, "failed to create email integration", http.StatusInternalServerError)
			return
		}
		// Sensitive fields (password, apiToken) are never echoed; return only
		// non-sensitive fields.
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

// workflowDetailDTO is the full representation returned by GET /v1/workflows/{slug}.
// Steps and PayloadSchema are forwarded as-is (raw JSON) so the console can
// render step controls without a round-trip parse on the server.
type workflowDetailDTO struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	TriggerIdentifier string          `json:"triggerIdentifier"`
	Active            bool            `json:"active"`
	Origin            string          `json:"origin"`
	Steps             json.RawMessage `json:"steps"`
	PayloadSchema     json.RawMessage `json:"payloadSchema,omitempty"`
}

// getWorkflow returns the full workflow definition for a single slug within
// the request Scope. The {slug} path parameter is the trigger identifier.
// Returns 404 when the workflow does not exist in this scope.
func getWorkflow(au *authoring.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		scope := identity.ScopeFrom(r.Context())
		wf, err := au.GetBySlug(r.Context(), scope, slug)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "workflow not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, workflowDetailDTO{
			ID:                wf.ID,
			Name:              wf.Name,
			TriggerIdentifier: wf.TriggerIdentifier,
			Active:            wf.Active,
			Origin:            wf.Origin,
			Steps:             wf.Steps,
			PayloadSchema:     wf.PayloadSchema,
		})
	}
}

// workflowUpdateRequest is the PUT /v1/workflows/{slug} body.
type workflowUpdateRequest struct {
	Name          string                `json:"name"`
	Steps         []workflowStepRequest `json:"steps"`
	PayloadSchema json.RawMessage       `json:"payloadSchema,omitempty"`
}

// putWorkflow updates an existing workflow's name, steps, and payload schema,
// recompiles the DAG, and re-registers it with the engine. Returns 404 when
// the workflow does not exist in this scope. Returns 422 when the steps fail
// compilation (e.g. a delay step with a non-positive amount).
func putWorkflow(au *authoring.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		var req workflowUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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
		wf, err := au.Update(r.Context(), scope, slug, authoring.UpdateInput{
			Name:          req.Name,
			Steps:         steps,
			PayloadSchema: req.PayloadSchema,
		})
		if err != nil {
			var compileErr *authoring.ErrCompileFailure
			if errors.As(err, &compileErr) {
				http.Error(w, compileErr.Error(), http.StatusUnprocessableEntity)
				return
			}
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "workflow not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, workflowDetailDTO{
			ID:                wf.ID,
			Name:              wf.Name,
			TriggerIdentifier: wf.TriggerIdentifier,
			Active:            wf.Active,
			Origin:            wf.Origin,
			Steps:             wf.Steps,
			PayloadSchema:     wf.PayloadSchema,
		})
	}
}

// deleteWorkflow removes a workflow and its engine DAG registration within the
// request Scope. Returns 404 when the workflow does not exist in this scope.
// Returns 204 on success (no body).
func deleteWorkflow(au *authoring.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		scope := identity.ScopeFrom(r.Context())
		if err := au.Delete(r.Context(), scope, slug); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "workflow not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// activateRequest is the PATCH /v1/workflows/{slug}/activate body.
type activateRequest struct {
	Active bool `json:"active"`
}

// patchWorkflowActivate toggles the active flag for the identified workflow.
// Body: {"active": bool}. Returns 404 when the workflow does not exist in
// this scope. Returns the updated full workflow DTO on success.
func patchWorkflowActivate(au *authoring.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		var req activateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		wf, err := au.SetActive(r.Context(), scope, slug, req.Active)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "workflow not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, workflowDetailDTO{
			ID:                wf.ID,
			Name:              wf.Name,
			TriggerIdentifier: wf.TriggerIdentifier,
			Active:            wf.Active,
			Origin:            wf.Origin,
			Steps:             wf.Steps,
			PayloadSchema:     wf.PayloadSchema,
		})
	}
}

// duplicateRequest is the POST /v1/workflows/{slug}/duplicate body.
type duplicateRequest struct {
	Name              string `json:"name"`
	TriggerIdentifier string `json:"triggerIdentifier"`
}

// postWorkflowDuplicate copies a workflow under a new name and identifier,
// inherits the source steps and payload schema, and publishes the new DAG.
// Returns 404 when the source workflow does not exist in this scope.
// Returns 409 when the desired TriggerIdentifier is already taken.
func postWorkflowDuplicate(au *authoring.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		srcSlug := chi.URLParam(r, "slug")
		var req duplicateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.TriggerIdentifier == "" {
			http.Error(w, "triggerIdentifier is required", http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		wf, err := au.Duplicate(r.Context(), scope, srcSlug, authoring.DuplicateInput{
			Name:              req.Name,
			TriggerIdentifier: req.TriggerIdentifier,
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "workflow not found", http.StatusNotFound)
				return
			}
			if errors.Is(err, authoring.ErrIdentifierConflict) {
				http.Error(w, "trigger identifier already exists", http.StatusConflict)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, workflowDetailDTO{
			ID:                wf.ID,
			Name:              wf.Name,
			TriggerIdentifier: wf.TriggerIdentifier,
			Active:            wf.Active,
			Origin:            wf.Origin,
			Steps:             wf.Steps,
			PayloadSchema:     wf.PayloadSchema,
		})
	}
}

// getNotifications handles GET /v1/notifications.
//
// Query parameters:
//   - workflowId    – filter to a specific workflow (optional)
//   - subscriberId  – filter to a specific subscriber (optional)
//   - transactionId – filter to a specific transaction (optional)
//   - since         – RFC3339 lower-bound on created_at (optional)
//   - limit         – page size, 1–200, default 50 (optional)
//   - cursor        – opaque keyset cursor from a prior response (optional)
//
// Response body:
//
//	{"items":[...], "next":"<cursor>", "totalCount":<n>}
//
// "next" is absent (omitempty) on the last page.
func getNotifications(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		scope := identity.ScopeFrom(r.Context())

		limit := 0
		if s := q.Get("limit"); s != "" {
			for _, c := range s {
				if c < '0' || c > '9' {
					http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
					return
				}
			}
			n := 0
			for _, c := range s {
				n = n*10 + int(c-'0')
			}
			limit = n
		}

		f := store.NotificationFilter{
			WorkflowID:    q.Get("workflowId"),
			SubscriberID:  q.Get("subscriberId"),
			TransactionID: q.Get("transactionId"),
		}
		if sinceStr := q.Get("since"); sinceStr != "" {
			t, err := time.Parse(time.RFC3339, sinceStr)
			if err != nil {
				http.Error(w, "since must be RFC3339 (e.g. 2006-01-02T15:04:05Z)", http.StatusBadRequest)
				return
			}
			f.Since = t
		}

		page, err := st.Notifications().List(r.Context(), scope, f, q.Get("cursor"), limit)
		if err != nil {
			if strings.Contains(err.Error(), "invalid cursor") {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		type response struct {
			Items      []domain.Notification `json:"items"`
			Next       string                `json:"next,omitempty"`
			TotalCount int                   `json:"totalCount"`
		}
		writeJSON(w, http.StatusOK, response{
			Items:      page.Items,
			Next:       page.Cursor,
			TotalCount: page.TotalCount,
		})
	}
}

// getNotification handles GET /v1/notifications/{id}.
// Returns 404 when the notification does not exist in this scope.
func getNotification(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		n, err := st.Notifications().Get(r.Context(), scope, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "notification not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, n)
	}
}

// maskCredentials replaces secret credential values with CredentialMaskSentinel
// so they are never surfaced in API responses. Non-secret fields (addr, from,
// base_url, host, username, account_id) are returned unchanged.
// The function operates on the raw JSON credential map; if decryption fails or
// the integration has no stored credentials an empty map is returned.
// integrationSecretFields enumerates, per provider, the credential fields that
// MUST be masked in API responses. Every provider accepted by the create switch
// must have an entry here — TestMaskCredentials_EveryProviderHasSecretFields
// enforces that, and maskCredentials fails CLOSED for any provider missing here.
var integrationSecretFields = map[string]map[string]bool{
	"smtp":             {"password": true},
	"cloudflare-email": {"api_token": true},
	"ntfy":             {"token": true},
}

// maskCredentials replaces secret credential fields with the mask sentinel.
// It fails CLOSED: for a provider with no secret-field map (unknown/future),
// EVERY field is masked rather than leaked — a new provider added to the create
// switch without a corresponding integrationSecretFields entry can never leak
// its secret on a read, only over-mask its non-secret fields.
func maskCredentials(providerID string, rawCreds map[string]any) map[string]any {
	fields, known := integrationSecretFields[providerID]
	out := make(map[string]any, len(rawCreds))
	for k, v := range rawCreds {
		if !known || fields[k] {
			out[k] = connectivity.CredentialMaskSentinel
		} else {
			out[k] = v
		}
	}
	return out
}

// integrationDTO is the JSON shape returned by list/get endpoints.
// enc_credentials is never included; credentialsMasked carries decrypted
// non-secret fields with secret fields replaced by CredentialMaskSentinel.
type integrationDTO struct {
	ID                string         `json:"id"`
	ProviderID        string         `json:"providerId"`
	Channel           string         `json:"channel"`
	Identifier        string         `json:"identifier"`
	Active            bool           `json:"active"`
	Primary           bool           `json:"primary"`
	Priority          int            `json:"priority"`
	CredentialsMasked map[string]any `json:"credentialsMasked"`
}

// toIntegrationDTO converts a domain.Integration (which may carry
// enc_credentials) into a safe DTO. If the service can decrypt, masked
// credentials are included; otherwise an empty map is used (fail safe).
func toIntegrationDTO(svc *connectivity.Service, in domain.Integration) integrationDTO {
	masked := map[string]any{}
	if len(in.EncCredentials) > 0 {
		if raw, err := svc.DecryptCredentials(in.EncCredentials); err == nil {
			var fields map[string]any
			if json.Unmarshal(raw, &fields) == nil {
				masked = maskCredentials(in.ProviderID, fields)
			}
		}
	}
	return integrationDTO{
		ID:                in.ID,
		ProviderID:        in.ProviderID,
		Channel:           in.Channel,
		Identifier:        in.Identifier,
		Active:            in.Active,
		Primary:           in.Primary,
		Priority:          in.Priority,
		CredentialsMasked: masked,
	}
}

// toIntegrationDTONoCredentials builds a DTO from a list-shape Integration
// that deliberately has no enc_credentials (List omits the column).
func toIntegrationDTONoCredentials(in domain.Integration) integrationDTO {
	return integrationDTO{
		ID:                in.ID,
		ProviderID:        in.ProviderID,
		Channel:           in.Channel,
		Identifier:        in.Identifier,
		Active:            in.Active,
		Primary:           in.Primary,
		Priority:          in.Priority,
		CredentialsMasked: map[string]any{},
	}
}

// getIntegrations handles GET /v1/integrations.
// Returns all integrations for the scope with credential fields absent
// (List does not select enc_credentials).
func getIntegrations(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		list, err := conn.Store().Integrations().List(r.Context(), scope)
		if err != nil {
			http.Error(w, "failed to list integrations", http.StatusInternalServerError)
			return
		}
		dtos := make([]integrationDTO, 0, len(list))
		for _, in := range list {
			dtos = append(dtos, toIntegrationDTONoCredentials(in))
		}
		writeJSON(w, http.StatusOK, dtos)
	}
}

// getIntegration handles GET /v1/integrations/{id}.
// Returns the integration with masked credentials (secrets replaced by ••••••).
func getIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		in, err := conn.Store().Integrations().GetByID(r.Context(), scope, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "integration not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to get integration", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toIntegrationDTO(conn, in))
	}
}

// unifiedCreateRequest is the body for POST /v1/integrations (unified create).
type unifiedCreateRequest struct {
	ProviderID  string            `json:"providerId"`
	Identifier  string            `json:"identifier"`
	Primary     bool              `json:"primary"`
	Credentials map[string]string `json:"credentials"`
}

// postIntegration handles POST /v1/integrations (unified create).
// Dispatches to the appropriate connectivity.Create* method based on providerId.
func postIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req unifiedCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())

		var (
			in  domain.Integration
			err error
		)
		switch req.ProviderID {
		case "smtp":
			in, err = conn.CreateSMTP(r.Context(), scope, req.Identifier, connectivity.SMTPConfig{
				Addr:     req.Credentials["addr"],
				Host:     req.Credentials["host"],
				Username: req.Credentials["username"],
				Password: req.Credentials["password"],
				From:     req.Credentials["from"],
			}, req.Primary)
		case "cloudflare-email":
			in, err = conn.CreateCloudflareEmail(r.Context(), scope, req.Identifier, connectivity.CloudflareEmailConfig{
				AccountID: req.Credentials["account_id"],
				APIToken:  req.Credentials["api_token"],
				From:      req.Credentials["from"],
			}, req.Primary)
		case "ntfy":
			in, err = conn.CreateNtfy(r.Context(), scope, req.Identifier, connectivity.NtfyConfig{
				BaseURL: req.Credentials["base_url"],
				Token:   req.Credentials["token"],
			}, req.Primary)
		default:
			http.Error(w, "unknown providerId "+req.ProviderID+"; allowed: smtp, cloudflare-email, ntfy", http.StatusBadRequest)
			return
		}
		if err != nil {
			// Never echo request detail into the error body.
			http.Error(w, "failed to create integration", http.StatusInternalServerError)
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

// updateIntegrationRequest is the body for PUT /v1/integrations/{id}.
type updateIntegrationRequest struct {
	Identifier  string            `json:"identifier"`
	Primary     bool              `json:"primary"`
	Priority    int               `json:"priority"`
	Credentials map[string]string `json:"credentials"`
}

// putIntegration handles PUT /v1/integrations/{id}.
// Delegates to connectivity.Service.Update which handles credential merging.
func putIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req updateIntegrationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		in, err := conn.Update(r.Context(), scope, id, connectivity.UpdateInput{
			Identifier:  req.Identifier,
			Primary:     req.Primary,
			Priority:    req.Priority,
			Credentials: req.Credentials,
		})
		if err != nil {
			var pc connectivity.ErrPrimaryConflict
			if errors.As(err, &pc) {
				writeJSON(w, http.StatusConflict, map[string]any{
					"code":              "primary_conflict",
					"existingPrimaryId": pc.ExistingID,
				})
				return
			}
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "integration not found", http.StatusNotFound)
				return
			}
			// Never echo request detail into the error body.
			http.Error(w, "failed to update integration", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":         in.ID,
			"providerId": in.ProviderID,
			"channel":    in.Channel,
			"identifier": in.Identifier,
			"active":     in.Active,
			"primary":    in.Primary,
		})
	}
}

// deleteIntegration handles DELETE /v1/integrations/{id}.
func deleteIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		if err := conn.Store().Integrations().Delete(r.Context(), scope, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "integration not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to delete integration", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// patchActiveRequest is the body for PATCH /v1/integrations/{id}/active.
type patchActiveRequest struct {
	Active bool `json:"active"`
}

// patchIntegrationActive handles PATCH /v1/integrations/{id}/active.
// Only flips the active flag; does not require a credential round-trip.
func patchIntegrationActive(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req patchActiveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		if err := conn.Store().Integrations().SetActive(r.Context(), scope, id, req.Active); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "integration not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to update integration", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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

// ---------------------------------------------------------------------------
// #34 — Subscriber management routes
// ---------------------------------------------------------------------------

// subscriberDTO is the JSON shape returned by subscriber list/get endpoints.
// Internal scope ids (org_id, environment_id) are excluded; data JSON is
// forwarded as-is.
type subscriberDTO struct {
	ID           string          `json:"id"`
	SubscriberID string          `json:"subscriberId"`
	Email        string          `json:"email"`
	Phone        string          `json:"phone"`
	FirstName    string          `json:"firstName"`
	LastName     string          `json:"lastName"`
	Locale       string          `json:"locale"`
	Timezone     string          `json:"timezone"`
	Data         json.RawMessage `json:"data,omitempty"`
}

func toSubscriberDTO(s domain.Subscriber) subscriberDTO {
	return subscriberDTO{
		ID:           s.ID,
		SubscriberID: s.SubscriberID,
		Email:        s.Email,
		Phone:        s.Phone,
		FirstName:    s.FirstName,
		LastName:     s.LastName,
		Locale:       s.Locale,
		Timezone:     s.Timezone,
		Data:         s.Data,
	}
}

// getSubscribers handles GET /v1/subscribers.
// Query params: ?after=<cursor> ?limit=<n>
// Response: {"data":[…],"next":"<cursor>","totalCount":<n>}
func getSubscribers(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		scope := identity.ScopeFrom(r.Context())

		limit := 0
		if lStr := q.Get("limit"); lStr != "" {
			for _, ch := range lStr {
				if ch < '0' || ch > '9' {
					http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
					return
				}
			}
			for _, ch := range lStr {
				limit = limit*10 + int(ch-'0')
			}
		}

		page, err := aud.ListSubscribers(r.Context(), scope, q.Get("after"), limit)
		if err != nil {
			if strings.Contains(err.Error(), "invalid cursor") {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "failed to list subscribers", http.StatusInternalServerError)
			return
		}

		dtos := make([]subscriberDTO, 0, len(page.Items))
		for _, s := range page.Items {
			dtos = append(dtos, toSubscriberDTO(s))
		}

		type response struct {
			Data       []subscriberDTO `json:"data"`
			Next       string          `json:"next,omitempty"`
			TotalCount int             `json:"totalCount"`
		}
		writeJSON(w, http.StatusOK, response{
			Data:       dtos,
			Next:       page.NextCursor,
			TotalCount: page.TotalCount,
		})
	}
}

// getSubscriber handles GET /v1/subscribers/{id} where {id} is the internal
// ULID id.
func getSubscriber(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		sub, err := aud.GetSubscriberByID(r.Context(), scope, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "subscriber not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to get subscriber", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toSubscriberDTO(sub))
	}
}

// patchSubscriberRequest is the body for PATCH /v1/subscribers/{id}.
type patchSubscriberRequest struct {
	Email     string          `json:"email"`
	FirstName string          `json:"firstName"`
	LastName  string          `json:"lastName"`
	Phone     string          `json:"phone"`
	Locale    string          `json:"locale"`
	Timezone  string          `json:"timezone"`
	Data      json.RawMessage `json:"data"`
}

// patchSubscriber handles PATCH /v1/subscribers/{id}.
// Only fields present and non-empty in the body are applied.
func patchSubscriber(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req patchSubscriberRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		sub, err := aud.PatchSubscriber(r.Context(), scope, id, audience.SubscriberInput{
			Email:     req.Email,
			FirstName: req.FirstName,
			LastName:  req.LastName,
			Phone:     req.Phone,
			Locale:    req.Locale,
			Timezone:  req.Timezone,
			Data:      req.Data,
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "subscriber not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to update subscriber", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toSubscriberDTO(sub))
	}
}

// deleteSubscriber handles DELETE /v1/subscribers/{id}.
// Soft-deletes the subscriber; returns 204 on success, 404 when not found.
func deleteSubscriber(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		if err := aud.DeleteSubscriber(r.Context(), scope, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "subscriber not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to delete subscriber", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------------------------------------------------------------------------
// #35 — Topic management routes
// ---------------------------------------------------------------------------

// topicDTO is the JSON shape returned by topic list/get endpoints.
type topicDTO struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	MemberCount int    `json:"memberCount"`
}

// getTopics handles GET /v1/topics.
// Response: {"data":[…]}
func getTopics(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		list, err := aud.ListTopics(r.Context(), scope)
		if err != nil {
			http.Error(w, "failed to list topics", http.StatusInternalServerError)
			return
		}
		dtos := make([]topicDTO, 0, len(list))
		for _, tc := range list {
			dtos = append(dtos, topicDTO{
				ID:          tc.ID,
				Key:         tc.Key,
				Name:        tc.Name,
				MemberCount: tc.MemberCount,
			})
		}
		type response struct {
			Data []topicDTO `json:"data"`
		}
		writeJSON(w, http.StatusOK, response{Data: dtos})
	}
}

// getTopic handles GET /v1/topics/{key}.
func getTopic(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())
		t, err := aud.GetTopic(r.Context(), scope, key)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "topic not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to get topic", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, topicDTO{ID: t.ID, Key: t.Key, Name: t.Name, MemberCount: t.MemberCount})
	}
}

// topicUpdateRequest is the body for PUT /v1/topics/{key}.
type topicUpdateRequest struct {
	Name string `json:"name"`
}

// putTopic handles PUT /v1/topics/{key}.
// Re-uses CreateTopic which upserts: a missing key is created, an existing key
// has its name updated. The key is immutable (taken from the path).
func putTopic(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		var req topicUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		t, err := aud.CreateTopic(r.Context(), scope, key, req.Name)
		if err != nil {
			http.Error(w, "failed to update topic", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, topicDTO{ID: t.ID, Key: t.Key, Name: t.Name})
	}
}

// deleteTopic handles DELETE /v1/topics/{key}.
// Permanently removes the topic and all its memberships. Returns 204.
func deleteTopic(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())
		if err := aud.DeleteTopic(r.Context(), scope, key); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "topic not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to delete topic", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// deleteTopicSubscriber handles DELETE /v1/topics/{key}/subscribers/{subscriberId}.
// Removes the subscriber's membership. Returns 204. The topic must exist
// (404 if not); removing a non-member is a no-op (still 204).
func deleteTopicSubscriber(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		subscriberID := chi.URLParam(r, "subscriberId")
		scope := identity.ScopeFrom(r.Context())
		if err := aud.RemoveTopicSubscriber(r.Context(), scope, key, subscriberID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "topic not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to remove subscriber from topic", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------------------------------------------------------------------------
// #36 — Webhook endpoint management routes
// ---------------------------------------------------------------------------

// webhookEndpointDTO is the JSON shape returned by webhook-endpoint endpoints.
type webhookEndpointDTO struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

func toWebhookEndpointDTO(e domain.WebhookEndpoint) webhookEndpointDTO {
	return webhookEndpointDTO{ID: e.ID, URL: e.URL, Active: e.Active}
}

// getWebhookEndpoints handles GET /v1/webhooks/endpoints.
// Returns all endpoints (active and inactive) for the scope.
func getWebhookEndpoints(wh *webhooksout.ManagementService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		list, err := wh.List(r.Context(), scope)
		if err != nil {
			http.Error(w, "failed to list webhook endpoints", http.StatusInternalServerError)
			return
		}
		dtos := make([]webhookEndpointDTO, 0, len(list))
		for _, e := range list {
			dtos = append(dtos, toWebhookEndpointDTO(e))
		}
		type response struct {
			Data []webhookEndpointDTO `json:"data"`
		}
		writeJSON(w, http.StatusOK, response{Data: dtos})
	}
}

// webhookEndpointCreateRequest is the body for POST /v1/webhooks/endpoints.
type webhookEndpointCreateRequest struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// postWebhookEndpoint handles POST /v1/webhooks/endpoints.
func postWebhookEndpoint(wh *webhooksout.ManagementService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req webhookEndpointCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.URL == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		ep, err := wh.Create(r.Context(), scope, req.URL, req.Active)
		if err != nil {
			http.Error(w, "failed to create webhook endpoint", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, toWebhookEndpointDTO(ep))
	}
}

// getWebhookEndpoint handles GET /v1/webhooks/endpoints/{id}.
func getWebhookEndpoint(wh *webhooksout.ManagementService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		ep, err := wh.GetByID(r.Context(), scope, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "webhook endpoint not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to get webhook endpoint", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toWebhookEndpointDTO(ep))
	}
}

// webhookEndpointUpdateRequest is the body for PUT /v1/webhooks/endpoints/{id}.
type webhookEndpointUpdateRequest struct {
	URL    string `json:"url"`
	Active *bool  `json:"active"`
}

// putWebhookEndpoint handles PUT /v1/webhooks/endpoints/{id}.
// Updates the URL; if active is present also flips the active flag.
func putWebhookEndpoint(wh *webhooksout.ManagementService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req webhookEndpointUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.URL == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		ep, err := wh.Update(r.Context(), scope, id, req.URL, req.Active)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "webhook endpoint not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to update webhook endpoint", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toWebhookEndpointDTO(ep))
	}
}

// deleteWebhookEndpoint handles DELETE /v1/webhooks/endpoints/{id}.
func deleteWebhookEndpoint(wh *webhooksout.ManagementService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		if err := wh.Delete(r.Context(), scope, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "webhook endpoint not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to delete webhook endpoint", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
