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
	"github.com/danmestas/notify/internal/emailloop"
	"github.com/danmestas/notify/internal/envvars"
	"github.com/danmestas/notify/internal/feedapi"
	"github.com/danmestas/notify/internal/feedingest"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbound"
	"github.com/danmestas/notify/internal/inbounddomains"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/realtime"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/version"
	"github.com/danmestas/notify/internal/webhooksout"
)

// Deps carries everything New needs to assemble the notify HTTP handler,
// mirroring console.Deps: named fields make each dependency's optionality
// explicit and remove the transposition hazard of a long positional list.
// All services are called in-process. Identity, Trigger and Inbox are
// required (their routes are always mounted); the remaining fields are
// nil-tolerant and gate their routes as documented below.
type Deps struct {
	// Identity authenticates API keys and resolves each request's Scope; its
	// Middleware wraps the whole authenticated route group. Required.
	Identity *identity.Service
	// Audience backs subscriber and topic management. Nil leaves those
	// handlers wired but non-functional in wiring that never exercises them.
	Audience *audience.Service
	// Trigger backs POST /v1/events/trigger. Required.
	Trigger *trigger.Service
	// Inbox mounts the in-app message feed routes unconditionally. Required.
	Inbox *inboxapi.Service
	// Realtime is the SSE gateway. Nil disables its routes.
	Realtime *realtime.Gateway
	// Feed is the durable stream-channel feed gateway. Nil disables the feed
	// SSE route (GET /v1/feed/stream).
	Feed *feedapi.Gateway
	// Inbound mounts the inbound message routes. Nil disables them.
	Inbound *inbound.Service
	// Analytics mounts the analytics routes. Nil disables them.
	Analytics *analytics.Service
	// Agents mounts the agents API routes. Nil disables them.
	Agents *agentsapi.Service
	// Ingress mounts the agent ingress routes. Nil disables them.
	Ingress *ingress.Service
	// Connectivity backs the integration management routes. Nil disables them.
	Connectivity *connectivity.Service
	// Store is the persistence layer. Nil disables the console read surface
	// (GET /v1/notifications, GET /v1/notifications/{id}), webhook-endpoint
	// management (GET/POST/PUT/DELETE /v1/webhooks/endpoints), layouts,
	// translations, contexts, and inbound-domain routes.
	Store *store.Store
	// EnvVars backs environment-variable management routes. Nil disables them.
	EnvVars *envvars.Service
	// Secrets seals webhook endpoint signing secrets (#346). Nil means
	// endpoints are created unsigned and rotate answers 503 -- acceptable in
	// tests, a wiring fault in production.
	Secrets *secretbox.Box
	// EmailLoop handles the inbound email parse webhook (POST /v1/inbound/email),
	// mounted OUTSIDE the API-key group with its own X-Inbound-Secret auth. Nil
	// leaves the route unmounted.
	EmailLoop *emailloop.Service
	// SourceIngest enables the inbound webhook-push ingest route
	// (POST /v1/sources/{sourceID}/events), mounted OUTSIDE the identity
	// Middleware group with its own per-source SourceKey auth. Nil leaves the
	// route unmounted.
	SourceIngest *feedingest.Gateway
	// Authoring backs the workflow-authoring routes. Nil disables them.
	Authoring *authoring.Service
	// Preferences backs GET/PUT /v1/subscribers/{id}/preferences. Nil disables
	// them. Requires Audience and Authoring to also be set (translating
	// trigger identifiers to workflow ids and back); a nil Preferences with
	// either of those set leaves the route unmounted, same as any other
	// optional dependency.
	Preferences *preferences.Service
	// InboundMXHost is the expected MX host stamped on domains created via the
	// inbound-domain routes (NOTIFY_INBOUND_MX_HOST, resolved once in
	// cmd/notify). Empty falls back to the package default inside
	// inbounddomains.New.
	InboundMXHost string
}

// New builds the notify HTTP handler from d. All authenticated routes are
// grouped behind d.Identity.Middleware, so each handler resolves its Scope via
// identity.ScopeFrom. Optional dependencies gate their routes as documented on
// Deps.
func New(d Deps) http.Handler {
	id, aud, trg, inbox, rt, feed, inb, an, ag, ing, conn, st, ev, eml, src, au, prf := d.Identity, d.Audience, d.Trigger, d.Inbox, d.Realtime, d.Feed, d.Inbound, d.Analytics, d.Agents, d.Ingress, d.Connectivity, d.Store, d.EnvVars, d.EmailLoop, d.SourceIngest, d.Authoring, d.Preferences
	// Wire a webhook management service when a store is present.
	var wh *webhooksout.ManagementService
	if st != nil {
		wh = webhooksout.NewManagement(st, d.Secrets)
	}
	// Wire the contexts service when a store is present (#54).
	var ctxSvc *contexts.Service
	if st != nil {
		ctxSvc = contexts.New(st)
	}
	// Wire the inbound domains service when a store is present (#53).
	var inboundDomainSvc *inbounddomains.Service
	if st != nil {
		inboundDomainSvc = inbounddomains.New(st, d.InboundMXHost)
	}
	r := chi.NewRouter()
	// Liveness probe, mounted outside the auth group so container/orchestrator
	// health checks need no API key.
	r.Get("/healthz", healthz)
	// Bare root → the console. notify serves no landing page at "/", so a plain
	// visit to the host (via the Cloudflare tunnel or the Tailscale name) would
	// otherwise 404; send it to the console, which handles login itself.
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/console/", http.StatusFound)
	})
	// Inbound email parse webhook (the email two-way loop). Mounted OUTSIDE the
	// API-key group: a mail forwarder (Cloudflare Email Worker / Resend inbound)
	// has no API key, so the handler authenticates the X-Inbound-Secret shared
	// secret itself. A nil service (no master key / store) leaves the route
	// unmounted.
	if eml != nil {
		r.Post("/v1/inbound/email", eml.HandleInbound)
	}
	// Inbound webhook-push ingest. Mounted OUTSIDE the API-key group: an external
	// producer holds a per-source SourceKey, not a console ApiKey, and the
	// handler authenticates that scheme itself. A nil gateway leaves the route
	// unmounted.
	if src != nil {
		src.Routes(r)
	}
	// The inbox is the one route group reachable with a subscriber session, so
	// it gets its own middleware. Everything else stays behind the tenant-wide
	// Middleware above, which means a session token is simply not a valid
	// credential anywhere else -- structurally, rather than by every handler
	// remembering to refuse it (#332).
	r.Group(func(sr chi.Router) {
		sr.Use(id.SubscriberMiddleware)
		inbox.Routes(sr)
	})
	r.Group(func(pr chi.Router) {
		pr.Use(id.Middleware)
		pr.Post("/v1/subscribers", postSubscriber(aud))
		// Mint a subscriber session (#332). Deliberately an API-key route:
		// minting is a backend act, performed for a user the consumer has
		// already authenticated. The token it returns is the only credential
		// a browser may hold.
		pr.Post("/v1/subscribers/{subscriberId}/session", id.PostSubscriberSession)
		pr.Put("/v1/subscribers/{subscriberId}/credentials", putSubscriberCredentials(aud))
		// Per-device Web Push delete (#305 stage 3): a browser unsubscribe
		// removes just that endpoint's row, leaving the subscriber's other
		// devices (and every other provider's credentials) untouched.
		pr.Delete("/v1/subscribers/{subscriberId}/credentials/webpush", deleteSubscriberWebPushCredentials(aud))
		pr.Post("/v1/events/trigger", postTrigger(trg))
		pr.Post("/v1/topics", postTopic(aud))
		pr.Post("/v1/topics/{key}/subscribers", postTopicSubscriber(aud))
		// Wave-2 subscriber management (#34).
		pr.Get("/v1/subscribers", getSubscribers(aud))
		pr.Get("/v1/subscribers/{id}", getSubscriber(aud))
		pr.Patch("/v1/subscribers/{id}", patchSubscriber(aud))
		pr.Delete("/v1/subscribers/{id}", deleteSubscriber(aud))
		// Subscriber preferences (#305 stage 2). Requires both Audience
		// (subscriber lookup/scoping) and Authoring (trigger identifier
		// <-> workflow id translation) alongside Preferences itself.
		if prf != nil {
			pr.Get("/v1/subscribers/{id}/preferences", getSubscriberPreferences(aud, au, prf))
			pr.Put("/v1/subscribers/{id}/preferences", putSubscriberPreferences(aud, au, prf))
		}
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
			pr.Post("/v1/integrations/chat", postChatIntegration(conn))
			pr.Post("/v1/integrations/sms", postSMSIntegration(conn))
			// GET /v1/integrations/webpush/key mints the org's VAPID keypair on
			// first use and returns the public half; rotation is delete-the-row.
			pr.Get("/v1/integrations/webpush/key", getWebPushKey(conn))
			pr.Put("/v1/integrations/webpush/subject", putWebPushSubject(conn))
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
			// Wave-4 #43: messages per notification.
			pr.Get("/v1/notifications/{id}/messages", getNotificationMessages(st))
			// Wave-4 #42: GET /v1/messages?integrationId= is handled by inboxapi.Service.listMessages
			// (extended in Wave-4 to support the integrationId filter param).
			// Layouts management (W4-L1).
			layouts := st.Layouts()
			pr.Get("/v1/layouts", getLayouts(layouts))
			pr.Post("/v1/layouts", postLayout(layouts))
			pr.Get("/v1/layouts/{key}", getLayout(layouts))
			pr.Put("/v1/layouts/{key}", putLayout(layouts))
			pr.Delete("/v1/layouts/{key}", deleteLayout(layouts))
			// Translations management (W4-L2).
			translations := st.Translations()
			pr.Get("/v1/translations", getTranslations(translations))
			pr.Post("/v1/translations", postTranslation(translations))
			pr.Get("/v1/translations/{locale}", getTranslation(translations))
			pr.Put("/v1/translations/{locale}", putTranslationLocale(translations))
			pr.Put("/v1/translations/{locale}/{key}", putTranslationKey(translations))
			pr.Delete("/v1/translations/{locale}/{key}", deleteTranslationKey(translations))
		}
		// Wave-2 webhook endpoint management (#36).
		if wh != nil {
			pr.Get("/v1/webhooks/endpoints", getWebhookEndpoints(wh))
			pr.Post("/v1/webhooks/endpoints", postWebhookEndpoint(wh))
			pr.Get("/v1/webhooks/endpoints/{id}", getWebhookEndpoint(wh))
			pr.Put("/v1/webhooks/endpoints/{id}", putWebhookEndpoint(wh))
			pr.Delete("/v1/webhooks/endpoints/{id}", deleteWebhookEndpoint(wh))
			pr.Post("/v1/webhooks/endpoints/{id}/secret", postWebhookEndpointSecret(wh))
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
		if inb != nil {
			inb.Routes(pr)
		}
		if rt != nil {
			rt.Routes(pr)
		}
		if feed != nil {
			feed.Routes(pr)
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
		// Email-origin agent conversations (#317): the explicit caller that
		// composes the first turn AND arms the email two-way loop, closing the
		// gap ReplyHook alone cannot (it only replies once a binding already
		// exists). Requires all three collaborators.
		if ing != nil && eml != nil && aud != nil {
			pr.Post("/v1/agents/{identifier}/conversations/email", postAgentEmailConversation(ing, eml, aud))
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
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
			// A *audience.ValidationError is a caller error (malformed
			// email, oversized field, too many/invalid custom fields) — the
			// same validation the console form applies, now enforced here
			// too so the API can't be used to bypass it. See W5.
			var verr *audience.ValidationError
			if errors.As(err, &verr) {
				writeJSONError(w, http.StatusBadRequest, "bad_request", verr.Msg)
				return
			}
			writeInternalError(w, r, err)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.ProviderID == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "providerId is required")
			return
		}
		subscriberID := chi.URLParam(r, "subscriberId")
		scope := identity.ScopeFrom(r.Context())
		// webpush upserts a per-device row in webpush_subscriptions instead of
		// the one-row-per-provider subscriber_channels table: a subscriber can
		// hold many live browser PushSubscriptions at once. Every other
		// provider's path below is unchanged.
		if req.ProviderID == "webpush" {
			if _, err := aud.SetWebPushCredentials(r.Context(), scope, subscriberID, req.Credentials); err != nil {
				var verr *audience.ValidationError
				if errors.As(err, &verr) {
					writeJSONError(w, http.StatusBadRequest, "bad_request", verr.Msg)
					return
				}
				writeInternalError(w, r, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if _, err := aud.SetCredentials(r.Context(), scope, subscriberID, req.ProviderID, req.Channel, req.Credentials); err != nil {
			writeInternalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// deleteSubscriberWebPushCredentials removes one device's Web Push
// subscription (?endpoint=<urlencoded>) for the subscriber and returns 204.
// A missing/empty endpoint is 400; no matching subscription in this scope is
// 404 — the generic (non-webpush) credentials DELETE, if any exists, is
// untouched by this handler.
func deleteSubscriberWebPushCredentials(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subscriberID := chi.URLParam(r, "subscriberId")
		endpoint := r.URL.Query().Get("endpoint")
		if endpoint == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "endpoint is required")
			return
		}
		scope := identity.ScopeFrom(r.Context())
		if err := aud.DeleteWebPushCredentials(r.Context(), scope, subscriberID, endpoint); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "webpush subscription not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// webPushKeyResponse is the GET /v1/integrations/webpush/key response body.
type webPushKeyResponse struct {
	PublicKey string `json:"publicKey"`
	// Subject is the VAPID contact pushes are signed with; SubjectDefault is
	// true when it is the server default rather than one set for this
	// environment. An empty subject means Web Push cannot send until one is
	// set (PUT /v1/integrations/webpush/subject).
	Subject        string `json:"subject"`
	SubjectDefault bool   `json:"subjectDefault"`
}

// webPushSubjectRequest is the PUT /v1/integrations/webpush/subject body. An
// empty subject clears the override so the server default applies.
type webPushSubjectRequest struct {
	Subject string `json:"subject"`
}

// getWebPushKey returns the scope's Web Push VAPID public key, minting a
// fresh keypair on first call. Rotation has no endpoint: deleting the
// "webpush" integration row makes the next call mint a new one.
func getWebPushKey(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		pub, _, err := conn.EnsureWebPushKeys(r.Context(), scope)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		subject, isDefault, err := conn.WebPushSubject(r.Context(), scope)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, webPushKeyResponse{PublicKey: pub, Subject: subject, SubjectDefault: isDefault})
	}
}

// putWebPushSubject sets the scope's VAPID subject, the contact Apple and
// other push services require in the signed JWT (#358). A subject that names
// no reachable contact is refused with 400 here rather than failing every
// push to an Apple device later.
func putWebPushSubject(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		var req webPushSubjectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		if err := conn.SetWebPushSubject(r.Context(), scope, req.Subject); err != nil {
			if errors.Is(err, connectivity.ErrInvalidVAPIDSubject) {
				writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
			writeInternalError(w, r, err)
			return
		}
		getWebPushKey(conn)(w, r)
	}
}

// pushIntegrationRequest is the POST /v1/integrations/push body. The "provider"
// field selects the backend: "ntfy" (default) uses baseUrl/token; "expo" uses
// the optional accessToken; "fcm" uses serviceAccount/projectId. token,
// accessToken, and serviceAccount are secrets and are never echoed back. An
// empty provider defaults to "ntfy" for backward compatibility.
type pushIntegrationRequest struct {
	Identifier string `json:"identifier"`
	Provider   string `json:"provider"` // "ntfy" (default), "expo", or "fcm"
	Primary    bool   `json:"primary"`
	// ntfy fields.
	BaseURL string `json:"baseUrl"`
	Token   string `json:"token"`
	// expo field (optional).
	AccessToken string `json:"accessToken"`
	// fcm fields.
	ServiceAccount string `json:"serviceAccount"`
	ProjectID      string `json:"projectId"`
}

// postPushIntegration creates a push integration within the request Scope and
// returns a slim DTO (no credential bytes) with 201. The "provider" field
// selects ntfy (default), expo, or fcm; an unknown provider fails closed with
// 400.
func postPushIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req pushIntegrationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		scope := identity.ScopeFrom(r.Context())

		provider := req.Provider
		if provider == "" {
			provider = "ntfy"
		}

		var (
			in  domain.Integration
			err error
		)
		switch provider {
		case "ntfy":
			if req.BaseURL == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "baseUrl is required for ntfy")
				return
			}
			in, err = conn.CreateNtfy(r.Context(), scope, req.Identifier, connectivity.NtfyConfig{
				BaseURL: req.BaseURL,
				Token:   req.Token,
			}, req.Primary)
		case "expo":
			// Expo's access token is optional; no required field beyond the
			// per-subscriber device token, which lives in subscriber_channels.
			in, err = conn.CreateExpo(r.Context(), scope, req.Identifier, connectivity.ExpoConfig{
				AccessToken: req.AccessToken,
			}, req.Primary)
		case "fcm":
			if req.ServiceAccount == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "serviceAccount is required for fcm")
				return
			}
			if req.ProjectID == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "projectId is required for fcm")
				return
			}
			in, err = conn.CreateFCM(r.Context(), scope, req.Identifier, connectivity.FCMConfig{
				ServiceAccount: req.ServiceAccount,
				ProjectID:      req.ProjectID,
			}, req.Primary)
		default:
			writeJSONError(w, http.StatusBadRequest, "bad_request", "unknown provider "+req.Provider+"; allowed: ntfy, expo, fcm")
			return
		}
		if err != nil {
			if errors.Is(err, connectivity.ErrInvalidIdentifier) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
			// Never surface the underlying error: wrapped marshal/encrypt/store
			// detail could carry credential-bearing config fields.
			writeInternalError(w, r, err)
			return
		}
		// Sensitive fields (token, accessToken, serviceAccount) are never echoed.
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		scope := identity.ScopeFrom(r.Context())
		resp, err := trg.Ingest(r.Context(), scope, req)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		// A refused trigger (unknown/inactive/empty workflow, unknown topic)
		// used to answer 201 with acknowledged:false, so callers checking only
		// the status believed the event was accepted (#310).
		status := http.StatusCreated
		if !resp.Acknowledged {
			status = http.StatusUnprocessableEntity
			for _, e := range resp.Error {
				if strings.HasSuffix(e, "not found") {
					status = http.StatusNotFound
				}
			}
		}
		writeJSON(w, status, resp)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		scope := identity.ScopeFrom(r.Context())
		topic, err := aud.CreateTopic(r.Context(), scope, req.Key, req.Name)
		if err != nil {
			if errors.Is(err, audience.ErrInvalidTopicKey) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
			writeInternalError(w, r, err)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		subID := strings.TrimSpace(req.SubscriberID)
		if subID == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "subscriberId is required")
			return
		}
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())
		if _, err := aud.Get(r.Context(), scope, subID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "subscriber not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		if err := aud.AddTopicSubscriber(r.Context(), scope, key, subID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			writeInternalError(w, r, err)
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
	Provider   string `json:"provider"` // "smtp" (default), "cloudflare-email", "resend", "ses", "sendmail"
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
	// Resend field.
	APIKey string `json:"apiKey"`
	// SES fields. secretAccessKey is a secret and is never echoed back.
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	Region          string `json:"region"`
}

// postEmailIntegration creates an email integration within the request Scope
// and returns a slim DTO (no credential bytes) with 201.
// The "provider" field selects between smtp (default) and cloudflare-email;
// the existing SMTP body is accepted unchanged when provider is absent.
func postEmailIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req emailIntegrationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.From == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "from is required")
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
				writeJSONError(w, http.StatusBadRequest, "bad_request", "accountId is required for cloudflare-email")
				return
			}
			if req.APIToken == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "apiToken is required for cloudflare-email")
				return
			}
			in, err = conn.CreateCloudflareEmail(r.Context(), scope, req.Identifier, connectivity.CloudflareEmailConfig{
				AccountID: req.AccountID,
				APIToken:  req.APIToken,
				From:      req.From,
			}, req.Primary)
		case "resend":
			if req.APIKey == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "apiKey is required for resend")
				return
			}
			in, err = conn.CreateResend(r.Context(), scope, req.Identifier, connectivity.ResendConfig{
				APIKey: req.APIKey,
				From:   req.From,
			}, req.Primary)
		case "ses":
			if req.AccessKeyID == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "accessKeyId is required for ses")
				return
			}
			if req.SecretAccessKey == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "secretAccessKey is required for ses")
				return
			}
			if req.Region == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "region is required for ses")
				return
			}
			in, err = conn.CreateSES(r.Context(), scope, req.Identifier, connectivity.SESConfig{
				AccessKeyID:     req.AccessKeyID,
				SecretAccessKey: req.SecretAccessKey,
				Region:          req.Region,
				From:            req.From,
			}, req.Primary)
		case "sendmail":
			// Only the sender is tenant data. The pipe command is operator
			// configuration (-sendmail-command) and is deliberately not
			// accepted here: an API-key holder must not be able to choose
			// which binary the notify process executes.
			in, err = conn.CreateSendmail(r.Context(), scope, req.Identifier, connectivity.SendmailConfig{
				From: req.From,
			}, req.Primary)
		case "smtp", "": // empty provider defaults to smtp for backward compatibility.
			if req.Host == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "host is required")
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", "unknown provider "+req.Provider+"; allowed: smtp, cloudflare-email, resend, ses, sendmail")
			return
		}

		if err != nil {
			if errors.Is(err, connectivity.ErrInvalidIdentifier) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
			// Never surface the underlying error: wrapped marshal/encrypt/store
			// detail could carry credential-bearing config fields.
			writeInternalError(w, r, err)
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

// chatIntegrationRequest is the POST /v1/integrations/chat body. The "provider"
// field selects the backend: "discord" uses webhookUrl; "telegram" uses
// botToken. webhookUrl and botToken are secrets and are never echoed back.
type chatIntegrationRequest struct {
	Identifier string `json:"identifier"`
	Provider   string `json:"provider"` // "discord" or "telegram"
	Primary    bool   `json:"primary"`
	// Discord field.
	WebhookURL string `json:"webhookUrl"`
	// Telegram and Slack field.
	BotToken string `json:"botToken"`
	// WhatsApp fields.
	AccessToken   string `json:"accessToken"`
	PhoneNumberID string `json:"phoneNumberId"`
	// Mastodon fields. AccessToken (above) doubles as the Mastodon Bearer
	// token.
	BaseURL    string `json:"baseUrl"`
	Visibility string `json:"visibility"`
	// Bluesky fields.
	PDSURL            string `json:"pdsUrl"`
	AccountIdentifier string `json:"accountIdentifier"`
	AppPassword       string `json:"appPassword"`
}

// postChatIntegration creates a chat integration within the request Scope and
// returns a slim DTO (no credential bytes) with 201. Mirrors
// postPushIntegration; the "provider" field selects discord vs telegram and an
// unknown provider fails closed with 400.
func postChatIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req chatIntegrationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		scope := identity.ScopeFrom(r.Context())

		var (
			in  domain.Integration
			err error
		)
		switch req.Provider {
		case "discord":
			if req.WebhookURL == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "webhookUrl is required for discord")
				return
			}
			in, err = conn.CreateDiscord(r.Context(), scope, req.Identifier, connectivity.DiscordConfig{
				WebhookURL: req.WebhookURL,
			}, req.Primary)
		case "telegram":
			if req.BotToken == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "botToken is required for telegram")
				return
			}
			in, err = conn.CreateTelegram(r.Context(), scope, req.Identifier, connectivity.TelegramConfig{
				BotToken: req.BotToken,
			}, req.Primary)
		case "slack":
			if req.BotToken == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "botToken is required for slack")
				return
			}
			in, err = conn.CreateSlack(r.Context(), scope, req.Identifier, connectivity.SlackConfig{
				BotToken: req.BotToken,
			}, req.Primary)
		case "whatsapp":
			if req.AccessToken == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "accessToken is required for whatsapp")
				return
			}
			if req.PhoneNumberID == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "phoneNumberId is required for whatsapp")
				return
			}
			in, err = conn.CreateWhatsApp(r.Context(), scope, req.Identifier, connectivity.WhatsAppConfig{
				AccessToken:   req.AccessToken,
				PhoneNumberID: req.PhoneNumberID,
			}, req.Primary)
		case "mastodon":
			if req.BaseURL == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "baseUrl is required for mastodon")
				return
			}
			if req.AccessToken == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "accessToken is required for mastodon")
				return
			}
			in, err = conn.CreateMastodon(r.Context(), scope, req.Identifier, connectivity.MastodonConfig{
				BaseURL:     req.BaseURL,
				AccessToken: req.AccessToken,
				Visibility:  req.Visibility,
			}, req.Primary)
		case "bluesky":
			if req.PDSURL == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "pdsUrl is required for bluesky")
				return
			}
			if req.AccountIdentifier == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "accountIdentifier is required for bluesky")
				return
			}
			if req.AppPassword == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "appPassword is required for bluesky")
				return
			}
			in, err = conn.CreateBluesky(r.Context(), scope, req.Identifier, connectivity.BlueskyConfig{
				PDSURL:      req.PDSURL,
				Identifier:  req.AccountIdentifier,
				AppPassword: req.AppPassword,
			}, req.Primary)
		default:
			writeJSONError(w, http.StatusBadRequest, "bad_request", "unknown provider "+req.Provider+"; allowed: discord, telegram, slack, whatsapp, mastodon, bluesky")
			return
		}
		if err != nil {
			if errors.Is(err, connectivity.ErrInvalidIdentifier) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
			// Never surface the underlying error: wrapped marshal/encrypt/store
			// detail could carry credential-bearing config fields.
			writeInternalError(w, r, err)
			return
		}
		// Sensitive fields (webhookUrl, botToken) are never echoed.
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

// smsIntegrationRequest is the POST /v1/integrations/sms body. The "provider"
// field selects the backend: "twilio" uses accountSid/authToken; "signalwire"
// uses projectId/token/spaceUrl. Both use "from" as the sender number.
// authToken and token are secrets and are never echoed back.
type smsIntegrationRequest struct {
	Identifier string `json:"identifier"`
	Provider   string `json:"provider"` // "twilio" or "signalwire"
	From       string `json:"from"`
	Primary    bool   `json:"primary"`
	// Twilio fields.
	AccountSID string `json:"accountSid"`
	AuthToken  string `json:"authToken"`
	// SignalWire fields.
	ProjectID string `json:"projectId"`
	Token     string `json:"token"`
	SpaceURL  string `json:"spaceUrl"`
	// SNS fields. secretAccessKey is a secret and is never echoed back. SNS
	// publishes directly to the recipient number and has no "from".
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	Region          string `json:"region"`
}

// postSMSIntegration creates an SMS integration within the request Scope and
// returns a slim DTO (no credential bytes) with 201. Mirrors
// postChatIntegration; the "provider" field selects twilio vs signalwire and an
// unknown provider fails closed with 400.
func postSMSIntegration(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req smsIntegrationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		// SNS publishes directly to the recipient number and carries no sender,
		// so it is exempt from the "from" requirement the LaML providers need.
		if req.From == "" && req.Provider != "sns" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "from is required")
			return
		}
		scope := identity.ScopeFrom(r.Context())

		var (
			in  domain.Integration
			err error
		)
		switch req.Provider {
		case "twilio":
			if req.AccountSID == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "accountSid is required for twilio")
				return
			}
			if req.AuthToken == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "authToken is required for twilio")
				return
			}
			in, err = conn.CreateTwilio(r.Context(), scope, req.Identifier, connectivity.TwilioConfig{
				AccountSID: req.AccountSID,
				AuthToken:  req.AuthToken,
				From:       req.From,
			}, req.Primary)
		case "signalwire":
			if req.ProjectID == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "projectId is required for signalwire")
				return
			}
			if req.Token == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "token is required for signalwire")
				return
			}
			if req.SpaceURL == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "spaceUrl is required for signalwire")
				return
			}
			in, err = conn.CreateSignalWire(r.Context(), scope, req.Identifier, connectivity.SignalWireConfig{
				ProjectID: req.ProjectID,
				Token:     req.Token,
				SpaceURL:  req.SpaceURL,
				From:      req.From,
			}, req.Primary)
		case "sns":
			if req.AccessKeyID == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "accessKeyId is required for sns")
				return
			}
			if req.SecretAccessKey == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "secretAccessKey is required for sns")
				return
			}
			if req.Region == "" {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "region is required for sns")
				return
			}
			in, err = conn.CreateSNS(r.Context(), scope, req.Identifier, connectivity.SNSConfig{
				AccessKeyID:     req.AccessKeyID,
				SecretAccessKey: req.SecretAccessKey,
				Region:          req.Region,
			}, req.Primary)
		default:
			writeJSONError(w, http.StatusBadRequest, "bad_request", "unknown provider "+req.Provider+"; allowed: twilio, signalwire, sns")
			return
		}
		if err != nil {
			if errors.Is(err, connectivity.ErrInvalidIdentifier) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
			// Never surface the underlying error: wrapped marshal/encrypt/store
			// detail could carry credential-bearing config fields.
			writeInternalError(w, r, err)
			return
		}
		// Sensitive fields (authToken, token) are never echoed.
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
// It is derived from the step-type registry (the ONE source of truth) so it can
// never drift from the console's offered set the way two hand-kept lists did.
var allowedStepTypes = steptype.Default().Names()

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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.TriggerIdentifier == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "triggerIdentifier is required")
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
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", msg)
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
			if errors.Is(err, store.ErrConflict) {
				writeJSONError(w, http.StatusConflict, "conflict", "triggerIdentifier already exists")
				return
			}
			if errors.Is(err, authoring.ErrInvalidTriggerIdentifier) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
			var compileErr *authoring.ErrCompileFailure
			if errors.As(err, &compileErr) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", compileErr.Error())
				return
			}
			writeInternalError(w, r, err)
			return
		}
		if err := au.Publish(r.Context(), scope, req.TriggerIdentifier); err != nil {
			// Publish compiles the step DAG; the workflow row was just stored
			// successfully, so any failure here is a step-config (compile)
			// error — a client mistake, not infra. Answer 422.
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
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
	Critical          bool   `json:"critical"`
}

// getWorkflows lists all workflows for the request Scope.
func getWorkflows(au *authoring.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		wfs, err := au.List(r.Context(), scope)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		dtos := make([]workflowDTO, 0, len(wfs))
		for _, wf := range wfs {
			dtos = append(dtos, workflowDTO{
				ID:                wf.ID,
				Name:              wf.Name,
				TriggerIdentifier: wf.TriggerIdentifier,
				Active:            wf.Active,
				Critical:          wf.Critical,
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
	Critical          bool            `json:"critical"`
	Steps             json.RawMessage `json:"steps"`
	PayloadSchema     json.RawMessage `json:"payloadSchema,omitempty"`
	// Outcome reports what a PUT /v1/workflows/{slug} upsert actually did:
	// "created", "unchanged", or "updated". Left unset (omitted from the
	// JSON body) by every other handler that returns this DTO.
	Outcome string `json:"outcome,omitempty"`
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
				writeJSONError(w, http.StatusNotFound, "not_found", "workflow not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, workflowDetailDTO{
			ID:                wf.ID,
			Name:              wf.Name,
			TriggerIdentifier: wf.TriggerIdentifier,
			Active:            wf.Active,
			Origin:            wf.Origin,
			Critical:          wf.Critical,
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

// putWorkflow ensures a workflow exists matching the submitted definition --
// PUT /v1/workflows/{slug} is an idempotent upsert, not an unconditional
// overwrite:
//
//   - absent -> created, 201.
//   - present and the submitted definition (name, steps, payload schema)
//     canonically equals the stored one -> 200, unchanged: no write, no new
//     workflow_version row.
//   - present and different -> the ?onConflict= query param decides.
//     "update" (the default, preserving the historical PUT behavior of this
//     endpoint) overwrites and returns 200 with the updated workflow.
//     "keep" leaves the stored row untouched and returns 409 with the
//     stored definition so the client can diff against what it sent.
//
// Returns 422 when the steps fail compilation (e.g. a delay step with a
// non-positive amount).
func putWorkflow(au *authoring.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		var req workflowUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		onConflict := authoring.OnConflictUpdate
		if v := r.URL.Query().Get("onConflict"); v != "" {
			switch v {
			case string(authoring.OnConflictUpdate):
				onConflict = authoring.OnConflictUpdate
			case string(authoring.OnConflictKeep):
				onConflict = authoring.OnConflictKeep
			default:
				writeJSONError(w, http.StatusBadRequest, "bad_request", "onConflict must be update or keep")
				return
			}
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
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", msg)
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
		wf, outcome, err := au.Ensure(r.Context(), scope, slug, authoring.EnsureInput{
			Name:              req.Name,
			TriggerIdentifier: slug,
			Steps:             steps,
			PayloadSchema:     req.PayloadSchema,
		}, onConflict)
		if err != nil {
			var conflictErr *authoring.ErrDefinitionConflict
			if errors.As(err, &conflictErr) {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":   "conflict",
					"message": "workflow exists with a different definition",
					"existing": workflowDetailDTO{
						ID:                conflictErr.Existing.ID,
						Name:              conflictErr.Existing.Name,
						TriggerIdentifier: conflictErr.Existing.TriggerIdentifier,
						Active:            conflictErr.Existing.Active,
						Origin:            conflictErr.Existing.Origin,
						Critical:          conflictErr.Existing.Critical,
						Steps:             conflictErr.Existing.Steps,
						PayloadSchema:     conflictErr.Existing.PayloadSchema,
					},
				})
				return
			}
			var compileErr *authoring.ErrCompileFailure
			if errors.As(err, &compileErr) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", compileErr.Error())
				return
			}
			if errors.Is(err, authoring.ErrInvalidTriggerIdentifier) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "workflow not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		if outcome == authoring.EnsureCreated {
			// Mirrors postWorkflow: Ensure only performs the store write for
			// a fresh row, Publish registers the compiled DAG with the
			// engine. A publish failure here is a step-config (compile)
			// error, not infra -- the row is already persisted, same as POST.
			if err := au.Publish(r.Context(), scope, slug); err != nil {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
		}
		status := http.StatusOK
		outcomeStr := "updated"
		switch outcome {
		case authoring.EnsureCreated:
			status = http.StatusCreated
			outcomeStr = "created"
		case authoring.EnsureUnchanged:
			outcomeStr = "unchanged"
		}
		writeJSON(w, status, workflowDetailDTO{
			ID:                wf.ID,
			Name:              wf.Name,
			TriggerIdentifier: wf.TriggerIdentifier,
			Active:            wf.Active,
			Origin:            wf.Origin,
			Critical:          wf.Critical,
			Steps:             wf.Steps,
			PayloadSchema:     wf.PayloadSchema,
			Outcome:           outcomeStr,
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
				writeJSONError(w, http.StatusNotFound, "not_found", "workflow not found")
				return
			}
			writeInternalError(w, r, err)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		scope := identity.ScopeFrom(r.Context())
		wf, err := au.SetActive(r.Context(), scope, slug, req.Active)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "workflow not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, workflowDetailDTO{
			ID:                wf.ID,
			Name:              wf.Name,
			TriggerIdentifier: wf.TriggerIdentifier,
			Active:            wf.Active,
			Origin:            wf.Origin,
			Critical:          wf.Critical,
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.TriggerIdentifier == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "triggerIdentifier is required")
			return
		}
		scope := identity.ScopeFrom(r.Context())
		wf, err := au.Duplicate(r.Context(), scope, srcSlug, authoring.DuplicateInput{
			Name:              req.Name,
			TriggerIdentifier: req.TriggerIdentifier,
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "workflow not found")
				return
			}
			if errors.Is(err, authoring.ErrIdentifierConflict) {
				writeJSONError(w, http.StatusConflict, "conflict", "trigger identifier already exists")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, workflowDetailDTO{
			ID:                wf.ID,
			Name:              wf.Name,
			TriggerIdentifier: wf.TriggerIdentifier,
			Active:            wf.Active,
			Origin:            wf.Origin,
			Critical:          wf.Critical,
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
					writeJSONError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
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
				writeJSONError(w, http.StatusBadRequest, "bad_request", "since must be RFC3339 (e.g. 2006-01-02T15:04:05Z)")
				return
			}
			f.Since = t
		}

		page, err := st.Notifications().List(r.Context(), scope, f, q.Get("cursor"), limit)
		if err != nil {
			if strings.Contains(err.Error(), "invalid cursor") {
				writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
			writeInternalError(w, r, err)
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
				writeJSONError(w, http.StatusNotFound, "not_found", "notification not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, n)
	}
}

// getNotificationMessages handles GET /v1/notifications/{id}/messages (#43).
// Returns all messages for the notification in scope order (newest first).
func getNotificationMessages(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		msgs, err := st.Messages().ListByNotificationID(r.Context(), scope, id)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		if msgs == nil {
			msgs = []domain.Message{}
		}
		writeJSON(w, http.StatusOK, msgs)
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
	"sendmail":         {}, // only "from" is stored; nothing to mask
	"cloudflare-email": {"api_token": true},
	"ntfy":             {"token": true},
	"expo":             {"access_token": true},
	"fcm":              {"service_account": true},
	"discord":          {"webhook_url": true},
	"telegram":         {"bot_token": true},
	"twilio":           {"auth_token": true},
	"signalwire":       {"token": true},
	"resend":           {"api_key": true},
	"ses":              {"secret_access_key": true},
	"slack":            {"bot_token": true},
	"whatsapp":         {"access_token": true},
	"sns":              {"secret_access_key": true},
	"email-webhook":    {"hmac_secret": true},
	"push-webhook":     {"hmac_secret": true},
	"chat-webhook":     {"hmac_secret": true},
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
			writeInternalError(w, r, err)
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
				writeJSONError(w, http.StatusNotFound, "not_found", "integration not found")
				return
			}
			writeInternalError(w, r, err)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
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
		case "expo":
			in, err = conn.CreateExpo(r.Context(), scope, req.Identifier, connectivity.ExpoConfig{
				AccessToken: req.Credentials["access_token"],
			}, req.Primary)
		case "fcm":
			in, err = conn.CreateFCM(r.Context(), scope, req.Identifier, connectivity.FCMConfig{
				ServiceAccount: req.Credentials["service_account"],
				ProjectID:      req.Credentials["project_id"],
			}, req.Primary)
		case "discord":
			in, err = conn.CreateDiscord(r.Context(), scope, req.Identifier, connectivity.DiscordConfig{
				WebhookURL: req.Credentials["webhook_url"],
			}, req.Primary)
		case "telegram":
			in, err = conn.CreateTelegram(r.Context(), scope, req.Identifier, connectivity.TelegramConfig{
				BotToken: req.Credentials["bot_token"],
			}, req.Primary)
		case "twilio":
			in, err = conn.CreateTwilio(r.Context(), scope, req.Identifier, connectivity.TwilioConfig{
				AccountSID: req.Credentials["account_sid"],
				AuthToken:  req.Credentials["auth_token"],
				From:       req.Credentials["from"],
			}, req.Primary)
		case "signalwire":
			in, err = conn.CreateSignalWire(r.Context(), scope, req.Identifier, connectivity.SignalWireConfig{
				ProjectID: req.Credentials["project_id"],
				Token:     req.Credentials["token"],
				SpaceURL:  req.Credentials["space_url"],
				From:      req.Credentials["from"],
			}, req.Primary)
		case "sns":
			in, err = conn.CreateSNS(r.Context(), scope, req.Identifier, connectivity.SNSConfig{
				AccessKeyID:     req.Credentials["access_key_id"],
				SecretAccessKey: req.Credentials["secret_access_key"],
				Region:          req.Credentials["region"],
			}, req.Primary)
		case "email-webhook":
			in, err = conn.CreateEmailWebhook(r.Context(), scope, req.Identifier, connectivity.EmailWebhookConfig{
				WebhookURL: req.Credentials["webhook_url"],
				HMACSecret: req.Credentials["hmac_secret"],
			}, req.Primary)
		case "push-webhook":
			in, err = conn.CreatePushWebhook(r.Context(), scope, req.Identifier, connectivity.PushWebhookConfig{
				WebhookURL: req.Credentials["webhook_url"],
				HMACSecret: req.Credentials["hmac_secret"],
			}, req.Primary)
		case "chat-webhook":
			in, err = conn.CreateChatWebhook(r.Context(), scope, req.Identifier, connectivity.ChatWebhookConfig{
				WebhookURL: req.Credentials["webhook_url"],
				HMACSecret: req.Credentials["hmac_secret"],
			}, req.Primary)
		case "resend":
			in, err = conn.CreateResend(r.Context(), scope, req.Identifier, connectivity.ResendConfig{
				APIKey: req.Credentials["api_key"],
				From:   req.Credentials["from"],
			}, req.Primary)
		case "sendmail":
			// See postEmailIntegration: the pipe command is never tenant data.
			in, err = conn.CreateSendmail(r.Context(), scope, req.Identifier, connectivity.SendmailConfig{
				From: req.Credentials["from"],
			}, req.Primary)
		case "ses":
			in, err = conn.CreateSES(r.Context(), scope, req.Identifier, connectivity.SESConfig{
				AccessKeyID:     req.Credentials["access_key_id"],
				SecretAccessKey: req.Credentials["secret_access_key"],
				Region:          req.Credentials["region"],
				From:            req.Credentials["from"],
			}, req.Primary)
		case "slack":
			in, err = conn.CreateSlack(r.Context(), scope, req.Identifier, connectivity.SlackConfig{
				BotToken: req.Credentials["bot_token"],
			}, req.Primary)
		case "whatsapp":
			in, err = conn.CreateWhatsApp(r.Context(), scope, req.Identifier, connectivity.WhatsAppConfig{
				AccessToken:   req.Credentials["access_token"],
				PhoneNumberID: req.Credentials["phone_number_id"],
			}, req.Primary)
		case "mastodon":
			in, err = conn.CreateMastodon(r.Context(), scope, req.Identifier, connectivity.MastodonConfig{
				BaseURL:     req.Credentials["base_url"],
				AccessToken: req.Credentials["access_token"],
				Visibility:  req.Credentials["visibility"],
			}, req.Primary)
		case "bluesky":
			in, err = conn.CreateBluesky(r.Context(), scope, req.Identifier, connectivity.BlueskyConfig{
				PDSURL:      req.Credentials["pds_url"],
				Identifier:  req.Credentials["identifier"],
				AppPassword: req.Credentials["app_password"],
			}, req.Primary)
		default:
			writeJSONError(w, http.StatusBadRequest, "bad_request", "unknown providerId "+req.ProviderID+"; allowed: smtp, cloudflare-email, resend, ses, ntfy, expo, fcm, discord, telegram, slack, whatsapp, mastodon, bluesky, twilio, signalwire, sns, email-webhook, push-webhook, chat-webhook")
			return
		}
		if err != nil {
			if errors.Is(err, connectivity.ErrInvalidIdentifier) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
			// Never echo request detail into the error body.
			writeInternalError(w, r, err)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
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
				writeJSONError(w, http.StatusNotFound, "not_found", "integration not found")
				return
			}
			// Never echo request detail into the error body.
			writeInternalError(w, r, err)
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
				writeJSONError(w, http.StatusNotFound, "not_found", "integration not found")
				return
			}
			writeInternalError(w, r, err)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		scope := identity.ScopeFrom(r.Context())
		if err := conn.Store().Integrations().SetActive(r.Context(), scope, id, req.Active); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "integration not found")
				return
			}
			writeInternalError(w, r, err)
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
					writeJSONError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
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
				writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
			writeInternalError(w, r, err)
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
				writeJSONError(w, http.StatusNotFound, "not_found", "subscriber not found")
				return
			}
			writeInternalError(w, r, err)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
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
				writeJSONError(w, http.StatusNotFound, "not_found", "subscriber not found")
				return
			}
			// A *audience.ValidationError is a caller error (malformed
			// email, oversized field, too many/invalid custom fields) — see
			// the matching comment in postSubscriber (W5).
			var verr *audience.ValidationError
			if errors.As(err, &verr) {
				writeJSONError(w, http.StatusBadRequest, "bad_request", verr.Msg)
				return
			}
			writeInternalError(w, r, err)
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
				writeJSONError(w, http.StatusNotFound, "not_found", "subscriber not found")
				return
			}
			writeInternalError(w, r, err)
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
			writeInternalError(w, r, err)
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
				writeJSONError(w, http.StatusNotFound, "not_found", "topic not found")
				return
			}
			writeInternalError(w, r, err)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "name is required")
			return
		}
		scope := identity.ScopeFrom(r.Context())
		t, err := aud.CreateTopic(r.Context(), scope, key, req.Name)
		if err != nil {
			writeInternalError(w, r, err)
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
				writeJSONError(w, http.StatusNotFound, "not_found", "topic not found")
				return
			}
			writeInternalError(w, r, err)
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
				writeJSONError(w, http.StatusNotFound, "not_found", "topic not found")
				return
			}
			writeInternalError(w, r, err)
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
	// Signed reports whether events to this endpoint carry X-Notify-Signature.
	// The secret itself is never in this DTO; it is returned once, on create
	// and on rotate, as webhookEndpointSecretDTO.
	Signed bool `json:"signed"`
}

func toWebhookEndpointDTO(e domain.WebhookEndpoint) webhookEndpointDTO {
	return webhookEndpointDTO{ID: e.ID, URL: e.URL, Active: e.Active, Signed: e.Secret != ""}
}

// webhookEndpointSecretDTO is the create/rotate response: the endpoint plus
// its signing secret in the clear, shown this once. Events are signed
// X-Notify-Signature: sha256=hex(HMAC-SHA256(secret, X-Notify-Timestamp + "." + body)).
type webhookEndpointSecretDTO struct {
	webhookEndpointDTO
	Secret string `json:"secret,omitempty"`
}

func toWebhookEndpointSecretDTO(c webhooksout.Created) webhookEndpointSecretDTO {
	return webhookEndpointSecretDTO{webhookEndpointDTO: toWebhookEndpointDTO(c.Endpoint), Secret: c.Secret}
}

// postWebhookEndpointSecret handles POST /v1/webhooks/endpoints/{id}/secret:
// rotate the signing secret and return the new one once. Existing endpoints
// that predate signing get their first secret this way.
func postWebhookEndpointSecret(wh *webhooksout.ManagementService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		c, err := wh.RotateSecret(r.Context(), scope, id)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "not_found", "webhook endpoint not found")
			case errors.Is(err, webhooksout.ErrSigningUnavailable):
				writeJSONError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
			default:
				writeInternalError(w, r, err)
			}
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, toWebhookEndpointSecretDTO(c))
	}
}

// getWebhookEndpoints handles GET /v1/webhooks/endpoints.
// Returns all endpoints (active and inactive) for the scope.
func getWebhookEndpoints(wh *webhooksout.ManagementService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		list, err := wh.List(r.Context(), scope)
		if err != nil {
			writeInternalError(w, r, err)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.URL == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "url is required")
			return
		}
		scope := identity.ScopeFrom(r.Context())
		created, err := wh.Create(r.Context(), scope, req.URL, req.Active)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		// The secret is in this response and nowhere else.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, toWebhookEndpointSecretDTO(created))
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
				writeJSONError(w, http.StatusNotFound, "not_found", "webhook endpoint not found")
				return
			}
			writeInternalError(w, r, err)
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
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.URL == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "url is required")
			return
		}
		scope := identity.ScopeFrom(r.Context())
		ep, err := wh.Update(r.Context(), scope, id, req.URL, req.Active)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "webhook endpoint not found")
				return
			}
			writeInternalError(w, r, err)
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
				writeJSONError(w, http.StatusNotFound, "not_found", "webhook endpoint not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
