package console

// a2a_client_keys.go implements the "A2A Client Keys" console page under
// /console/a2a-keys: list, mint (the secret is shown once, with a copy
// button) and revoke. It follows the stream-sources page: native POST→303
// mutations, and the freshly minted secret rides behind a one-shot reveal
// token (reveal.go) so it never appears in a redirect Location or history.

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/a2aauth"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// a2aKeyRow is one row of the client keys table.
type a2aKeyRow struct {
	ID           string
	Name         string
	SubscriberID string
	CreatedAt    string
	LastUsedAt   string
	Revoked      bool
	RevokedAt    string
}

// a2aKeysPageView is what a2a_client_keys.html binds.
type a2aKeysPageView struct {
	Header pageHeader
	Rows   []a2aKeyRow
	Empty  *emptyState
	// NewSecret is the raw token shown exactly once after a mint.
	NewSecret     string
	ConfirmRevoke string
	CreateError   string
	ReadOnly      bool
	CSRF          string
}

// serveA2AKeysPage renders GET /console/a2a-keys.
func (c *Console) serveA2AKeysPage(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveA2AKeysPage: no scope in context")
	}
	keys, err := c.deps.A2AAuth.List(r.Context(), scope)
	if err != nil {
		c.deps.Logger.Error("console: list a2a client keys", "err", err)
		c.serveError(w, r)
		return
	}
	rows := make([]a2aKeyRow, 0, len(keys))
	active := 0
	for _, k := range keys {
		rows = append(rows, a2aKeyRow{
			ID: k.ID, Name: k.Name, SubscriberID: k.SubscriberID,
			CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt,
			Revoked: k.RevokedAt != "", RevokedAt: k.RevokedAt,
		})
		if k.RevokedAt == "" {
			active++
		}
	}
	var empty *emptyState
	if len(rows) == 0 {
		empty = &emptyState{
			Icon:        "unplug",
			Title:       "No client keys yet",
			Description: "Mint a key to let a machine caller (another agent or a service) call this environment's A2A channel.",
		}
	}
	header := pageHeader{
		Title:    "A2A Client Keys",
		Subtitle: "Credentials for machine callers of the A2A channel. A key works only in this environment.",
		Tiles:    []tile{{Label: "active keys", Count: active, Tone: toneDefault}},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "A2A Client Keys"}
	}
	newSecret := ""
	if token := r.URL.Query().Get("revealed"); token != "" {
		newSecret, _ = c.takeReveal(token)
	}
	s, _ := sessionFromContext(r.Context())
	c.renderPage(w, r, http.StatusOK, "a2a-keys", "A2A Client Keys", a2aKeysPageView{
		Header:        header,
		Rows:          rows,
		Empty:         empty,
		NewSecret:     newSecret,
		ConfirmRevoke: r.URL.Query().Get("confirmRevoke"),
		CreateError:   r.URL.Query().Get("createError"),
		ReadOnly:      c.deps.ReadOnly,
		CSRF:          s.CSRF,
	})
}

// handleA2AKeyCreate handles POST /console/a2a-keys.
func (c *Console) handleA2AKeyCreate(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.mutationScope(w, r, "handleA2AKeyCreate")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	_, token, err := c.deps.A2AAuth.Mint(r.Context(), scope, r.FormValue("name"), r.FormValue("subscriber_id"))
	if err != nil {
		msg := "Mint failed."
		switch {
		case errors.Is(err, a2aauth.ErrInvalidName):
			msg = "Name is required (max 64 characters)."
		case errors.Is(err, a2aauth.ErrSubscriberNotFound):
			msg = "That subscriber does not exist in this environment."
		default:
			c.deps.Logger.Error("console: mint a2a client key", "err", err)
		}
		c.redirect(w, r, "/console/a2a-keys?createError="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	reveal, err := c.stashReveal(token)
	if err != nil {
		c.deps.Logger.Error("console: stash a2a client key", "err", err)
		http.Error(w, "mint failed", http.StatusInternalServerError)
		return
	}
	c.redirect(w, r, "/console/a2a-keys?revealed="+reveal, http.StatusSeeOther)
}

// handleA2AKeyRevoke handles POST /console/a2a-keys/{id}/revoke.
func (c *Console) handleA2AKeyRevoke(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.mutationScope(w, r, "handleA2AKeyRevoke")
	if !ok {
		return
	}
	if err := c.deps.A2AAuth.Revoke(r.Context(), scope, chi.URLParam(r, "id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: revoke a2a client key", "err", err)
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	c.redirect(w, r, "/console/a2a-keys", http.StatusSeeOther)
}
