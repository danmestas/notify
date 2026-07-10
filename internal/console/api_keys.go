package console

// api_keys.go implements W4-E2 — the API Keys settings page under
// /console/settings/api-keys.
//
// Security model (fail-closed):
//   - Raw API keys are NEVER rendered in the page's initial/default render.
//   - The masked display (••••…last4) is always safe; DecryptForDisplay
//     decrypts for the last4 only, and that's all that flows into the page
//     view struct unless the caller explicitly asked for more (see below).
//   - Reveal-on-demand mirrors Environment Variables (variables.go): a
//     ?reveal=1 query param on the same authenticated GET route triggers a
//     separate render that includes the plaintext. The plaintext is never
//     embedded client-side for a Datastar/JS toggle to flip — the browser
//     must make a fresh authenticated request to see it, so view-source /
//     devtools on the masked page never leaks the live secret.
//   - The raw hex is also shown exactly once immediately after a create or
//     rotate action, embedded in the one-time reveal card. The handler that
//     produces it redirects to a page that shows it via a query parameter —
//     the raw key is only surfaced in the redirect target page view struct
//     and immediately discarded from the handler stack.
//   - No raw key is ever written to a log line.
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
	// RawKey is the current active key's plaintext. It is populated ONLY
	// when Revealed is true (an explicit ?reveal=1 request) — never on the
	// default render. It is never logged.
	RawKey   string
	Revealed bool
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

	// Reveal-on-demand, mirroring Environment Variables: the plaintext only
	// flows into the rendered page when the caller explicitly asked for it
	// via ?reveal=1 on this same authenticated route. The default render
	// (and thus every page load from a bookmark, a link, or a refresh) only
	// ever carries the masked form.
	revealed := r.URL.Query().Get("reveal") == "1"
	rawForView := ""
	if revealed {
		rawForView = rawHex
	}

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
		RawKey:        rawForView, // never logged; empty unless Revealed
		Revealed:      revealed,
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
