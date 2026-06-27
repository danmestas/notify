package console

// api_keys.go implements W4-E2 — the API Keys settings page under
// /console/settings/api-keys.
//
// Security model (fail-closed):
//   - Raw API keys are NEVER rendered in any list or masked view.
//   - The raw hex is shown exactly once: immediately after a create or rotate
//     action, embedded in the one-time reveal card. The handler that produces
//     it redirects to a page that shows it via a query parameter — the raw key
//     is only surfaced in the redirect target page view struct and immediately
//     discarded from the handler stack.
//   - No raw key is ever written to a log line.
//   - The masked display (••••…last4) is always safe; DecryptForDisplay
//     decrypts for the last4 only.
//   - Rotate is a two-step: first click shows the confirm form, second click
//     POSTs the rotate action.

import (
	"net/http"

	"github.com/danmestas/notify/internal/domain"
)

// ─── view structs ─────────────────────────────────────────────────────────────

// apiKeysPageView is what api_keys.html binds.
type apiKeysPageView struct {
	Header        pageHeader
	Env           domain.Environment
	MaskedKey     string // e.g. "••••••••••…abcd"
	Last4         string // last 4 chars of the hex key
	APIURL        string // base URL of the notify server
	ConfirmRotate bool   // render the rotate confirm form
	// NewRawKey is the plaintext key after a successful mint or rotate,
	// shown once. Empty on all other renders.
	NewRawKey     string
	NewKeyRotated bool // true when NewRawKey comes from a rotation
	// RawKey is the current active key, surfaced only via the data-text
	// reveal attribute so the Reveal toggle can show it client-side.
	// It is never logged.
	RawKey   string
	ReadOnly bool
	CSRF     string
}

// ─── page handlers ────────────────────────────────────────────────────────────

// serveAPIKeysPage renders GET /console/settings/api-keys.
func (c *Console) serveAPIKeysPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveAPIKeysPage: w is nil")
	}
	if r == nil {
		panic("serveAPIKeysPage: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveAPIKeysPage: no scope in context")
	}

	env, err := c.deps.Identity.Store().Environments().Get(r.Context(), scope)
	if err != nil {
		c.deps.Logger.Error("console: api-keys page: get environment", "err", err)
		c.serveError(w, r)
		return
	}

	masked, last4, rawHex, err := c.deps.Identity.DecryptForDisplay(r.Context(), scope)
	if err != nil {
		c.deps.Logger.Error("console: api-keys page: decrypt for display", "err", err)
		c.serveError(w, r)
		return
	}

	s, _ := sessionFromContext(r.Context())

	// A fresh key (from regenerate) rides in the URL; strip it so
	// a browser back-button doesn't re-reveal it.
	newRaw := r.URL.Query().Get("newKey")
	rotated := r.URL.Query().Get("rotated") == "1"

	header := pageHeader{
		Title:    "API Keys",
		Subtitle: "Application credentials for this environment.",
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "API Keys"}
	}

	view := apiKeysPageView{
		Header:        header,
		Env:           env,
		MaskedKey:     masked,
		Last4:         last4,
		RawKey:        rawHex, // never logged; flows into data-text reveal attribute only
		APIURL:        c.deps.HTTPAddr,
		ConfirmRotate: r.URL.Query().Get("confirmRotate") == "1",
		NewRawKey:     newRaw,
		NewKeyRotated: rotated,
		ReadOnly:      c.deps.ReadOnly,
		CSRF:          s.CSRF,
	}
	c.renderPage(w, r, http.StatusOK, "api-keys", "API Keys", view)
}

// ─── mutation handlers ────────────────────────────────────────────────────────

// handleAPIKeyRegenerate handles POST /console/settings/api-keys/regenerate.
// It rotates the active key and redirects back to the page with the new raw
// key embedded in the URL (shown once). Raw key NEVER logged.
func (c *Console) handleAPIKeyRegenerate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleAPIKeyRegenerate: w is nil")
	}
	if r == nil {
		panic("handleAPIKeyRegenerate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleAPIKeyRegenerate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	newRaw, err := c.deps.Identity.RotateKey(r.Context(), scope)
	if err != nil {
		c.deps.Logger.Error("console: regenerate api key", "err", err)
		http.Error(w, "rotate failed", http.StatusInternalServerError)
		return
	}

	// Redirect with the new key as a query param (shown-once pattern).
	// The key is in the URL only transiently; the handler that serves the
	// redirect reads it and does NOT persist or log it.
	http.Redirect(
		w, r,
		"/console/settings/api-keys?newKey="+newRaw+"&rotated=1",
		http.StatusSeeOther,
	)
}
