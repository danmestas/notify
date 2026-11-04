package console

// agent_receiver.go adds the live-receiver surface to the agent detail
// overview: "answered by" (the configured runtime, or a connected live
// receiver) and the agent's receiver keys -- mint (the secret is shown once,
// behind the same one-shot reveal token as client keys), list and revoke.

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/a2areceiver"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// receiverKeyRow is one receiver key on the agent overview.
type receiverKeyRow struct {
	ID         string
	Name       string
	CreatedAt  string
	LastUsedAt string
	Revoked    bool
}

// agentReceiverView is what the overview's "answered by" row and receiver key
// card bind. Enabled is false when the console has no receiver service.
type agentReceiverView struct {
	Enabled    bool
	AnsweredBy string // "live receiver (since …)" or "<runtime> runtime"
	Live       bool
	Keys       []receiverKeyRow
	NewSecret  string
	Error      string
}

func (c *Console) buildAgentReceiverView(r *http.Request, scope domain.Scope, agent agentdomain.Agent) agentReceiverView {
	v := agentReceiverView{AnsweredBy: agent.Runtime + " runtime"}
	if c.deps.Receiver == nil {
		return v
	}
	v.Enabled = true
	if since, ok := c.deps.Receiver.Live(agent.ID); ok {
		v.Live = true
		v.AnsweredBy = "live receiver (since " + timestamp.Format(since) + ")"
	}
	keys, err := c.deps.Receiver.List(r.Context(), scope, agent.Identifier)
	if err != nil {
		c.deps.Logger.Error("console: list receiver keys", "agent", agent.Identifier, "err", err)
	}
	for _, k := range keys {
		v.Keys = append(v.Keys, receiverKeyRow{
			ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, Revoked: k.RevokedAt != "",
		})
	}
	if token := r.URL.Query().Get("revealed"); token != "" {
		v.NewSecret, _ = c.takeReveal(token)
	}
	v.Error = r.URL.Query().Get("keyError")
	return v
}

func overviewURL(identifier string) string {
	return "/console/agents/" + url.PathEscape(identifier) + "/tabs/overview"
}

// handleReceiverKeyCreate handles POST /console/agents/{identifier}/receiver-keys.
func (c *Console) handleReceiverKeyCreate(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.mutationScope(w, r, "handleReceiverKeyCreate")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	identifier := chi.URLParam(r, "identifier")
	_, token, err := c.deps.Receiver.Mint(r.Context(), scope, identifier, r.FormValue("name"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.serveNotFound(w, r)
		return
	case errors.Is(err, a2areceiver.ErrInvalidName):
		http.Redirect(w, r, overviewURL(identifier)+"?keyError="+url.QueryEscape("Name is required (max 64 characters)."), http.StatusSeeOther)
		return
	case err != nil:
		c.deps.Logger.Error("console: mint receiver key", "err", err)
		http.Error(w, "mint failed", http.StatusInternalServerError)
		return
	}
	reveal, err := c.stashReveal(token)
	if err != nil {
		c.deps.Logger.Error("console: stash receiver key", "err", err)
		http.Error(w, "mint failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, overviewURL(identifier)+"?revealed="+reveal, http.StatusSeeOther)
}

// handleReceiverKeyRevoke handles POST /console/agents/{identifier}/receiver-keys/{id}/revoke.
func (c *Console) handleReceiverKeyRevoke(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.mutationScope(w, r, "handleReceiverKeyRevoke")
	if !ok {
		return
	}
	identifier := chi.URLParam(r, "identifier")
	if err := c.deps.Receiver.Revoke(r.Context(), scope, identifier, chi.URLParam(r, "id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: revoke receiver key", "err", err)
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, overviewURL(identifier), http.StatusSeeOther)
}
