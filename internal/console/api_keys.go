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
//   - The raw hex is also shown exactly once immediately after a rotate
//     action, embedded in the one-time reveal card. The handler that
//     produces it stashes the raw key server-side (reveal.go) and redirects
//     with an opaque one-shot TOKEN — never the raw key — in the URL. The
//     redirect target consumes the token exactly once and discards it, so a
//     reload of the same URL renders nothing.
//   - No raw key is ever written to a log line.
//   - Rotate is a two-step: first click shows the confirm form, second click
//     POSTs the rotate action.

import (
	"net/http"
	"time"

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

	// A fresh key (from regenerate) rides behind a one-shot reveal TOKEN —
	// never the raw key itself — so the redirect Location, browser history,
	// and any reverse-proxy/CDN access log fronting the console never see
	// the plaintext. The token is consumed on this first GET; a reload
	// (token already consumed) renders nothing.
	newRaw := ""
	if token := r.URL.Query().Get("revealed"); token != "" {
		newRaw, _ = c.takeReveal(token)
	}
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
// It rotates the active key and redirects back to the page carrying a
// one-shot reveal token — never the raw key — in the URL. Raw key NEVER
// logged.
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

	// Rotating the operator's OWN active key deletes the old key row, so
	// the current session cookie (which carries the old key) would fail
	// re-authentication on the very next request. Re-seal a fresh cookie
	// carrying the new key in this same response — same CSRF token, fresh
	// expiry — so the redirect target stays authenticated and the reveal
	// card actually renders instead of bouncing to login.
	s, ok := sessionFromContext(r.Context())
	if !ok {
		panic("handleAPIKeyRegenerate: no session in context")
	}
	sealed, err := c.sealSession(session{
		Key:  newRaw,
		CSRF: s.CSRF,
		Exp:  time.Now().Add(sessionTTL).Unix(),
	})
	if err != nil {
		c.deps.Logger.Error("console: reseal session after regenerate", "err", err)
		http.Error(w, "rotate failed", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, r, sealed)

	// Stash the raw key server-side and redirect with an opaque one-shot
	// token — never the key — so the secret never transits the URL,
	// browser history, or a reverse-proxy/CDN access log.
	token, err := c.stashReveal(newRaw)
	if err != nil {
		c.deps.Logger.Error("console: stash revealed api key", "err", err)
		http.Error(w, "rotate failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(
		w, r,
		"/console/settings/api-keys?revealed="+token+"&rotated=1",
		http.StatusSeeOther,
	)
}
