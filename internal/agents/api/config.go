package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	"github.com/danmestas/notify/internal/identity"
)

// Configurer relays a config change to a mesh agent and returns the card the
// agent answers with. *mesh.Client implements it.
type Configurer interface {
	Configure(ctx context.Context, meshSubject string, body []byte) (json.RawMessage, error)
}

// WithConfigurer enables PATCH /v1/agents/{identifier}/config. Without one the
// route answers 503.
func (svc *Service) WithConfigurer(c Configurer) *Service {
	svc.configurer = c
	return svc
}

// WithLogger sets the logger for config audit lines; the default is slog's.
func (svc *Service) WithLogger(l *slog.Logger) *Service {
	svc.log = l
	return svc
}

var idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// configAgent relays the request body, opaque JSON, to a runtime-mesh agent.
// The body can carry a provider key, so it is never logged: the audit line
// names only the caller's scope, the agent and the outcome. A config change
// replaces fields rather than appending, so a repeated request (and with it
// a retry under the same Idempotency-Key) converges on the same state.
func (svc *Service) configAgent(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	agent, err := svc.store.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "identifier"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	outcome := "error"
	defer func() {
		svc.logger().Info("agent config", "org", scope.OrgID, "env", scope.EnvID, "agent", agent.Identifier, "outcome", outcome)
	}()
	if k := r.Header.Get("Idempotency-Key"); k != "" && !idempotencyKeyRE.MatchString(k) {
		outcome = "rejected"
		http.Error(w, "Idempotency-Key must be 1-64 characters of A-Z, a-z, 0-9, _ or -", http.StatusUnprocessableEntity)
		return
	}
	if agent.Runtime != agentdomain.RuntimeMesh || mesh.ConfigSubject(agent.MeshSubject) == "" {
		outcome = "not_configurable"
		http.Error(w, "only runtime mesh agents with a meshSubject under agents.prompt. accept config changes", http.StatusConflict)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mesh.MaxConfigBytes))
	if err != nil {
		outcome = "invalid"
		http.Error(w, "config body unreadable or over 64 KiB", http.StatusBadRequest)
		return
	}
	if t := strings.TrimSpace(string(body)); len(t) == 0 || t[0] != '{' || !json.Valid(body) {
		outcome = "invalid"
		http.Error(w, "config body must be a JSON object", http.StatusBadRequest)
		return
	}
	if svc.configurer == nil {
		outcome = "unavailable"
		http.Error(w, "agent config relay is not available", http.StatusServiceUnavailable)
		return
	}
	card, err := svc.configurer.Configure(r.Context(), agent.MeshSubject, body)
	var ce *mesh.ConfigError
	switch {
	case err == nil:
		outcome = "ok"
		if len(card) == 0 {
			card = json.RawMessage("null")
		}
		writeJSON(w, http.StatusOK, map[string]json.RawMessage{"card": card})
	case errors.As(err, &ce) && ce.Code == "forbidden":
		outcome = "forbidden"
		http.Error(w, ce.Message, http.StatusForbidden)
	case errors.As(err, &ce):
		outcome = "invalid"
		http.Error(w, ce.Message, http.StatusBadRequest)
	default:
		outcome = "unavailable"
		http.Error(w, "agent did not accept the config change: it is not reachable or did not answer", http.StatusServiceUnavailable)
	}
}

func (svc *Service) logger() *slog.Logger {
	if svc.log != nil {
		return svc.log
	}
	return slog.Default()
}
