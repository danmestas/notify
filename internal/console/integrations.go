package console

// integrations.go implements the W2 integrations store page and its
// Datastar fragment endpoints. All data access goes through the in-process
// IntegrationRepo (for reads) and connectivity.Service.Update (for
// credential-merge writes). Credentials are NEVER rendered raw; every
// secret field is replaced with connectivity.CredentialMaskSentinel
// ("••••••") before the view struct is built.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// ─── signal keys ────────────────────────────────────────────────────────────

// integrationSignalKeys is the set of signal keys the integrations grid
// reads from the Datastar envelope.
var integrationSignalKeys = []string{"integrationChannel", "integrationDetailId"}

// ─── provider catalog ────────────────────────────────────────────────────────

// providerMeta holds static display metadata for a known provider.
type providerMeta struct {
	ID      string // matches domain.Integration.ProviderID
	Label   string // human name shown in the UI
	Channel string // "email" | "push" | "in_app" | "chat"
}

// providerCatalog is the static set of providers the console knows about.
// The ID must match the ProviderID stored in the integrations table.
var providerCatalog = []providerMeta{
	{ID: "smtp", Label: "SMTP Email", Channel: "email"},
	{ID: "cloudflare_email", Label: "Cloudflare Email", Channel: "email"},
	{ID: "ntfy", Label: "ntfy Push", Channel: "push"},
	{ID: "discord", Label: "Discord", Channel: "chat"},
	{ID: "telegram", Label: "Telegram", Channel: "chat"},
	{ID: "twilio", Label: "Twilio SMS", Channel: "sms"},
	{ID: "signalwire", Label: "SignalWire SMS", Channel: "sms"},
}

// providerLabel returns the human label for providerID from the catalog.
// Falls back to the raw providerID when not found.
func providerLabel(providerID string) string {
	for _, p := range providerCatalog {
		if p.ID == providerID {
			return p.Label
		}
	}
	return providerID
}

// channelLabel returns a human-readable channel label.
func channelLabel(ch string) string {
	switch ch {
	case "email":
		return "Email"
	case "push":
		return "Push"
	case "in_app":
		return "In-app"
	case "chat":
		return "Chat"
	case "sms":
		return "SMS"
	}
	return ch
}

// ─── view structs ────────────────────────────────────────────────────────────

// integrationRow is one row in the integrations table.
type integrationRow struct {
	ID            string
	ProviderID    string
	ProviderLabel string
	Channel       string
	ChannelLabel  string
	Identifier    string
	Active        bool
	Primary       bool
}

// integrationsListView is what integrations.html binds.
type integrationsListView struct {
	Header   pageHeader
	Rows     []integrationRow
	Channel  string // current channel filter ("" = all)
	Empty    *emptyState
	Signals  string // JSON for data-signals
	ReadOnly bool
	// PreopenID is set when the page is opened at /console/integrations/{id}:
	// the drawer opens immediately for that row.
	PreopenID string
}

// integrationsGridView is what the integrations-grid fragment binds.
type integrationsGridView struct {
	Rows     []integrationRow
	Channel  string
	Empty    *emptyState
	ReadOnly bool
}

// integrationCredentialField is one credential entry shown masked in the
// detail/edit form.
type integrationCredentialField struct {
	Key      string // JSON field name (e.g. "password", "token")
	Label    string // human label (e.g. "Password", "API Token")
	Value    string // always connectivity.CredentialMaskSentinel — never the real secret
	Required bool
}

// integrationDetailView is what the integration-detail fragment binds.
type integrationDetailView struct {
	Integration   integrationRow
	CredFields    []integrationCredentialField
	Empty         bool
	ConfirmDelete bool
	ReadOnly      bool
	// ActiveTab is "settings" (default) or "activity" (W4-I2).
	ActiveTab string
	// ActivityMessages is populated when ActiveTab == "activity" (W4-I2).
	ActivityMessages []integrationActivityRow
	// CSRF is the session token plumbed into the drawer's native POST forms
	// (delete / active toggle / verify). Required for the credential-verify
	// action's native form POST (W4 ADR-013, #55).
	CSRF string
	// CanVerify reports whether the durable credential-verify action is wired
	// (Deps.CredVerifier != nil). When false the Verify button renders disabled.
	CanVerify bool
	// ConfirmVerify renders the two-step inline confirm for Verify (chassis
	// idiom: no native confirm() since the probe performs a real handshake).
	ConfirmVerify bool
	// VerifyRunURL / VerifyError carry the post-verify banner: the deep-link to
	// the verification engine run, or the failure message. Mutually exclusive.
	VerifyRunURL string
	VerifyError  string
}

// integrationActivityRow is one row in the Activity tab of the detail drawer.
type integrationActivityRow struct {
	MessageID    string
	Status       string
	SubscriberID string
	CreatedAt    string
	// RunURL is the deep-link into the engine console for the run, or empty.
	RunURL string
	RunID  string
}

// integrationProviderFormView is what the integration-provider-form fragment
// binds (step 2 of the create flow: per-provider credential form).
type integrationProviderFormView struct {
	ProviderID string
	Label      string
	Channel    string
	CredFields []integrationCredentialField
	ReadOnly   bool
}

// ─── helpers ────────────────────────────────────────────────────────────────

// integrationToRow converts a domain.Integration to a display row.
func integrationToRow(in domain.Integration) integrationRow {
	return integrationRow{
		ID:            in.ID,
		ProviderID:    in.ProviderID,
		ProviderLabel: providerLabel(in.ProviderID),
		Channel:       in.Channel,
		ChannelLabel:  channelLabel(in.Channel),
		Identifier:    in.Identifier,
		Active:        in.Active,
		Primary:       in.Primary,
	}
}

// filterRows returns the subset of rows matching the channel filter.
// An empty channel means "all".
func filterRows(rows []integrationRow, channel string) []integrationRow {
	if channel == "" {
		return rows
	}
	out := make([]integrationRow, 0, len(rows))
	for _, r := range rows {
		if r.Channel == channel {
			out = append(out, r)
		}
	}
	return out
}

// integrationsEmptyState builds the appropriate empty-state for the grid.
func integrationsEmptyState(filtered bool, readOnly bool) *emptyState {
	if filtered {
		return &emptyState{
			Icon:        "🔌",
			Title:       "No integrations in this channel",
			Description: "Switch to All to see every integration, or connect a new provider.",
		}
	}
	es := &emptyState{
		Icon:        "🔌",
		Title:       "No integrations yet",
		Description: "Connect a provider to start delivering notifications.",
	}
	if !readOnly {
		es.PrimaryAction = &emptyStateAction{
			Label: "Connect provider",
			Href:  "#",
		}
	}
	return es
}

// credentialFieldsForProvider returns the masked credential fields for a
// known provider. All values are set to CredentialMaskSentinel — real
// secrets are never returned to the browser.
func credentialFieldsForProvider(providerID string) []integrationCredentialField {
	switch providerID {
	case "smtp":
		return []integrationCredentialField{
			{Key: "addr", Label: "Address (host:port)", Value: connectivity.CredentialMaskSentinel, Required: true},
			{Key: "host", Label: "Host", Value: connectivity.CredentialMaskSentinel, Required: true},
			{Key: "username", Label: "Username", Value: connectivity.CredentialMaskSentinel},
			{Key: "password", Label: "Password", Value: connectivity.CredentialMaskSentinel},
			{Key: "from", Label: "From address", Value: connectivity.CredentialMaskSentinel},
		}
	case "cloudflare_email":
		return []integrationCredentialField{
			{Key: "account_id", Label: "Account ID", Value: connectivity.CredentialMaskSentinel, Required: true},
			{Key: "api_token", Label: "API Token", Value: connectivity.CredentialMaskSentinel, Required: true},
			{Key: "from", Label: "From address", Value: connectivity.CredentialMaskSentinel},
		}
	case "ntfy":
		return []integrationCredentialField{
			{Key: "base_url", Label: "Server URL", Value: connectivity.CredentialMaskSentinel, Required: true},
			{Key: "token", Label: "Access Token", Value: connectivity.CredentialMaskSentinel},
		}
	case "twilio":
		return []integrationCredentialField{
			{Key: "account_sid", Label: "Account SID", Value: connectivity.CredentialMaskSentinel, Required: true},
			{Key: "auth_token", Label: "Auth Token", Value: connectivity.CredentialMaskSentinel, Required: true},
			{Key: "from", Label: "From number", Value: connectivity.CredentialMaskSentinel, Required: true},
		}
	case "signalwire":
		return []integrationCredentialField{
			{Key: "project_id", Label: "Project ID", Value: connectivity.CredentialMaskSentinel, Required: true},
			{Key: "token", Label: "API Token", Value: connectivity.CredentialMaskSentinel, Required: true},
			{Key: "space_url", Label: "Space URL", Value: connectivity.CredentialMaskSentinel, Required: true},
			{Key: "from", Label: "From number", Value: connectivity.CredentialMaskSentinel, Required: true},
		}
	}
	return nil
}

// integrationsSignalsJSON serialises the channel filter state into the JSON
// string Datastar's data-signals attribute expects.
func integrationsSignalsJSON(channel, detailID string) string {
	m := map[string]any{
		"integrationChannel":    channel,
		"integrationDetailId":   detailID,
		"integrationDetailOpen": detailID != "",
	}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"integrationChannel":"","integrationDetailId":"","integrationDetailOpen":false}`
	}
	return string(b)
}

// ─── list page handler ───────────────────────────────────────────────────────

// serveIntegrationsPage renders GET /console/integrations and
// GET /console/integrations/{id} (pre-opens the detail drawer).
func (c *Console) serveIntegrationsPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveIntegrationsPage: w is nil")
	}
	if r == nil {
		panic("serveIntegrationsPage: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveIntegrationsPage: no scope in context (route not behind sessionMiddleware)")
	}

	preopenID := chi.URLParam(r, "id")

	integrations, err := c.deps.Integrations.List(r.Context(), scope)
	if err != nil {
		c.deps.Logger.Error("console: integrations page list", "err", err)
		c.serveError(w, r)
		return
	}

	rows := make([]integrationRow, 0, len(integrations))
	for _, in := range integrations {
		rows = append(rows, integrationToRow(in))
	}

	header := pageHeader{
		Title:    "Integration Store",
		Subtitle: "Manage provider credentials for this environment.",
		Tiles: []tile{
			{Label: "integrations", Count: len(rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		c.deps.Logger.Error("console: integrations header", "err", err)
		header = pageHeader{Title: "Integration Store"}
	}

	var empty *emptyState
	if len(rows) == 0 {
		empty = integrationsEmptyState(false, c.deps.ReadOnly)
	}

	view := integrationsListView{
		Header:    header,
		Rows:      rows,
		Empty:     empty,
		Signals:   integrationsSignalsJSON("", preopenID),
		ReadOnly:  c.deps.ReadOnly,
		PreopenID: preopenID,
	}

	c.renderPage(w, r, http.StatusOK, "integrations", "Integration Store", view)
}

// ─── fragment: integrations grid ────────────────────────────────────────────

// serveFragmentIntegrationsGrid answers
// GET /console/api/fragments/integrations-grid.
// Reads integrationChannel signal + URL param and returns a PatchElements
// SSE event replacing #integrations-grid.
func (c *Console) serveFragmentIntegrationsGrid(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentIntegrationsGrid: w is nil")
	}
	if r == nil {
		panic("serveFragmentIntegrationsGrid: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentIntegrationsGrid: no scope in context")
	}

	q := mergeFragmentParams(r, integrationSignalKeys...)
	channel := ""
	if vs := q["channel"]; len(vs) > 0 {
		channel = vs[0]
	}

	integrations, err := c.deps.Integrations.List(r.Context(), scope)
	if err != nil {
		c.deps.Logger.Error("console: fragment integrations grid", "err", err)
		http.Error(w, "list integrations failed", http.StatusInternalServerError)
		return
	}

	allRows := make([]integrationRow, 0, len(integrations))
	for _, in := range integrations {
		allRows = append(allRows, integrationToRow(in))
	}

	rows := filterRows(allRows, channel)
	var empty *emptyState
	if len(rows) == 0 {
		empty = integrationsEmptyState(channel != "", c.deps.ReadOnly)
	}

	fv := integrationsGridView{
		Rows:     rows,
		Channel:  channel,
		Empty:    empty,
		ReadOnly: c.deps.ReadOnly,
	}
	html, err := renderFragment(c.ts.base, "integrations-grid", fv)
	if err != nil {
		c.deps.Logger.Error("console: render integrations-grid fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── fragment: integration detail ───────────────────────────────────────────

// serveFragmentIntegrationDetail answers
// GET /console/api/fragments/integration-detail.
// ?id= (or the integrationDetailId signal) loads one integration by ID and
// returns #integration-detail as a PatchElements SSE event. Credentials are
// always masked — the real secret is never included in the response.
func (c *Console) serveFragmentIntegrationDetail(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentIntegrationDetail: w is nil")
	}
	if r == nil {
		panic("serveFragmentIntegrationDetail: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentIntegrationDetail: no scope in context")
	}

	q := mergeFragmentParams(r, "integrationDetailId")
	id := ""
	if vs := q["id"]; len(vs) > 0 {
		id = vs[0]
	}

	confirmDelete := r.URL.Query().Get("confirmDelete") == "true"
	confirmVerify := r.URL.Query().Get("confirmVerify") == "true"

	renderEmpty := func() {
		html, err := renderFragment(c.ts.base, "integration-detail", integrationDetailView{Empty: true})
		if err != nil {
			c.deps.Logger.Error("console: render empty integration detail", "err", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
		c.patchHTML(w, r, html)
	}

	if id == "" {
		renderEmpty()
		return
	}

	in, err := c.deps.Integrations.GetByID(r.Context(), scope, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			renderEmpty()
			return
		}
		c.deps.Logger.Error("console: integration detail get", "id", id, "err", err)
		http.Error(w, "load integration failed", http.StatusInternalServerError)
		return
	}

	// Build masked credential fields — real secrets are never included.
	credFields := credentialFieldsForProvider(in.ProviderID)

	s, _ := sessionFromContext(r.Context())
	// verifyRunURL is the deep-link to the run a just-completed verify minted
	// (the handler 303-redirects here with ?verifyRun=<run_id>).
	verifyRunURL := ""
	if rid := r.URL.Query().Get("verifyRun"); rid != "" {
		verifyRunURL = c.runURL(rid)
	}

	html, err := renderFragment(c.ts.base, "integration-detail", integrationDetailView{
		Integration:   integrationToRow(in),
		CredFields:    credFields,
		ConfirmDelete: confirmDelete,
		ReadOnly:      c.deps.ReadOnly,
		ActiveTab:     "settings",
		CSRF:          s.CSRF,
		CanVerify:     c.deps.CredVerifier != nil,
		ConfirmVerify: confirmVerify,
		VerifyRunURL:  verifyRunURL,
		VerifyError:   r.URL.Query().Get("verifyError"),
	})
	if err != nil {
		c.deps.Logger.Error("console: render integration detail", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// handleIntegrationVerify handles POST /console/integrations/{id}/verify
// (W4 ADR-013, #55): the durable credential-verify action. It kicks an engine
// run that probes the integration's provider credentials (a real handshake) and
// 303-redirects back to the integrations page with the new run surfaced via
// ?verifyRun= (or ?verifyError= on failure). Native form POST → 303; the inline
// two-step confirm in the drawer guards the real probe.
func (c *Console) handleIntegrationVerify(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleIntegrationVerify: w is nil")
	}
	if r == nil {
		panic("handleIntegrationVerify: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleIntegrationVerify: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing integration id", http.StatusBadRequest)
		return
	}
	// The drawer re-opens this integration after the redirect.
	backURL := "/console/integrations/" + url.PathEscape(id)

	if c.deps.CredVerifier == nil {
		http.Redirect(w, r, backURL+"?verifyError="+
			url.QueryEscape("Credential verification is unavailable."), http.StatusSeeOther)
		return
	}

	runID, err := c.deps.CredVerifier.Start(r.Context(), scope, id)
	if err != nil {
		c.deps.Logger.Error("console: credential verify start", "id", id, "err", err)
		http.Redirect(w, r, backURL+"?verifyError="+
			url.QueryEscape("Verification failed to start: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, backURL+"?verifyRun="+url.QueryEscape(runID), http.StatusSeeOther)
}

// ─── fragment: integration activity tab ─────────────────────────────────────

// serveFragmentIntegrationActivity answers
// GET /console/api/fragments/integration-activity (W4-I2).
// Returns the integration-detail fragment with ActiveTab="activity" populated
// with up to 100 recent messages for that integration.
func (c *Console) serveFragmentIntegrationActivity(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentIntegrationActivity: w is nil")
	}
	if r == nil {
		panic("serveFragmentIntegrationActivity: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentIntegrationActivity: no scope in context")
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		html, err := renderFragment(c.ts.base, "integration-detail", integrationDetailView{Empty: true})
		if err != nil {
			c.deps.Logger.Error("console: render empty integration activity", "err", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
		c.patchHTML(w, r, html)
		return
	}

	in, err := c.deps.Integrations.GetByID(r.Context(), scope, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			html, rerr := renderFragment(c.ts.base, "integration-detail", integrationDetailView{Empty: true})
			if rerr != nil {
				c.deps.Logger.Error("console: render empty integration activity (not found)", "err", rerr)
				http.Error(w, "render failed", http.StatusInternalServerError)
				return
			}
			c.patchHTML(w, r, html)
			return
		}
		c.deps.Logger.Error("console: integration activity get", "id", id, "err", err)
		http.Error(w, "load integration failed", http.StatusInternalServerError)
		return
	}

	// Fetch messages for this integration (capped at 100, newest first).
	var rows []integrationActivityRow
	if c.deps.Messages != nil {
		msgs, merr := c.deps.Messages.ListByIntegrationID(r.Context(), scope, id, 100)
		if merr != nil {
			c.deps.Logger.Error("console: list messages by integration", "id", id, "err", merr)
			// Non-fatal: render empty activity rather than a 500.
		} else {
			rows = make([]integrationActivityRow, 0, len(msgs))
			for _, m := range msgs {
				row := integrationActivityRow{
					MessageID:    m.ID,
					Status:       m.Status,
					SubscriberID: m.SubscriberID,
					CreatedAt:    m.CreatedAt,
					RunID:        m.RunID,
				}
				if c.deps.DagnatsConsoleURL != "" && m.RunID != "" {
					row.RunURL = c.deps.DagnatsConsoleURL + "/runs/" + m.RunID
				}
				rows = append(rows, row)
			}
		}
	}

	html, err := renderFragment(c.ts.base, "integration-detail", integrationDetailView{
		Integration:      integrationToRow(in),
		ReadOnly:         c.deps.ReadOnly,
		ActiveTab:        "activity",
		ActivityMessages: rows,
	})
	if err != nil {
		c.deps.Logger.Error("console: render integration activity", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── fragment: provider form ─────────────────────────────────────────────────

// serveFragmentIntegrationProviderForm answers
// GET /console/api/fragments/integration-provider-form.
// ?provider= selects the provider; returns the step-2 credential form.
func (c *Console) serveFragmentIntegrationProviderForm(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentIntegrationProviderForm: w is nil")
	}
	if r == nil {
		panic("serveFragmentIntegrationProviderForm: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentIntegrationProviderForm: no scope in context")
	}

	providerID := r.URL.Query().Get("provider")
	var meta providerMeta
	for _, p := range providerCatalog {
		if p.ID == providerID {
			meta = p
			break
		}
	}

	fv := integrationProviderFormView{
		ProviderID: meta.ID,
		Label:      meta.Label,
		Channel:    meta.Channel,
		CredFields: credentialFieldsForProvider(meta.ID),
		ReadOnly:   c.deps.ReadOnly,
	}
	html, err := renderFragment(c.ts.base, "integration-provider-form", fv)
	if err != nil {
		c.deps.Logger.Error("console: render provider form", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── mutation handlers ───────────────────────────────────────────────────────

// handleIntegrationCreate handles POST /console/integrations.
// Gated by ReadOnly. Dispatches on provider_id to the appropriate
// connectivity.Create* method.
func (c *Console) handleIntegrationCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleIntegrationCreate: w is nil")
	}
	if r == nil {
		panic("handleIntegrationCreate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleIntegrationCreate: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	providerID := r.FormValue("provider_id")
	identifier := strings.TrimSpace(r.FormValue("identifier"))
	primary := r.FormValue("primary") == "true" || r.FormValue("primary") == "on"

	var createErr error
	switch providerID {
	case "smtp":
		cfg := connectivity.SMTPConfig{
			Addr:     r.FormValue("addr"),
			Host:     r.FormValue("host"),
			Username: r.FormValue("username"),
			Password: r.FormValue("password"),
			From:     r.FormValue("from"),
		}
		_, createErr = c.deps.Connectivity.CreateSMTP(r.Context(), scope, identifier, cfg, primary)
	case "ntfy":
		cfg := connectivity.NtfyConfig{
			BaseURL: r.FormValue("base_url"),
			Token:   r.FormValue("token"),
		}
		_, createErr = c.deps.Connectivity.CreateNtfy(r.Context(), scope, identifier, cfg, primary)
	case "cloudflare_email":
		cfg := connectivity.CloudflareEmailConfig{
			AccountID: r.FormValue("account_id"),
			APIToken:  r.FormValue("api_token"),
			From:      r.FormValue("from"),
		}
		_, createErr = c.deps.Connectivity.CreateCloudflareEmail(r.Context(), scope, identifier, cfg, primary)
	case "twilio":
		cfg := connectivity.TwilioConfig{
			AccountSID: r.FormValue("account_sid"),
			AuthToken:  r.FormValue("auth_token"),
			From:       r.FormValue("from"),
		}
		_, createErr = c.deps.Connectivity.CreateTwilio(r.Context(), scope, identifier, cfg, primary)
	case "signalwire":
		cfg := connectivity.SignalWireConfig{
			ProjectID: r.FormValue("project_id"),
			Token:     r.FormValue("token"),
			SpaceURL:  r.FormValue("space_url"),
			From:      r.FormValue("from"),
		}
		_, createErr = c.deps.Connectivity.CreateSignalWire(r.Context(), scope, identifier, cfg, primary)
	default:
		http.Error(w, fmt.Sprintf("unknown provider: %s", providerID), http.StatusBadRequest)
		return
	}

	if createErr != nil {
		c.deps.Logger.Error("console: create integration", "provider", providerID, "err", createErr)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/integrations", http.StatusSeeOther)
}

// handleIntegrationUpdate handles POST /console/integrations/{id}
// (_method=PUT). Gated by ReadOnly. Applies a credential-merge update via
// connectivity.Service.Update; masked-sentinel fields keep their stored
// values unchanged.
func (c *Console) handleIntegrationUpdate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleIntegrationUpdate: w is nil")
	}
	if r == nil {
		panic("handleIntegrationUpdate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleIntegrationUpdate: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	// Only handle _method=PUT; anything else is a 405.
	if method := r.FormValue("_method"); method != "PUT" && method != "" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Collect all credential fields submitted (masked sentinels pass through).
	creds := map[string]string{}
	for _, key := range []string{
		"addr", "host", "username", "password", "from",
		"account_id", "api_token", "base_url", "token",
	} {
		if v := r.FormValue(key); v != "" {
			creds[key] = v
		}
	}

	inp := connectivity.UpdateInput{
		Identifier:  strings.TrimSpace(r.FormValue("identifier")),
		Primary:     r.FormValue("primary") == "true" || r.FormValue("primary") == "on",
		Credentials: creds,
	}

	_, err := c.deps.Connectivity.Update(r.Context(), scope, id, inp)
	if err != nil {
		var conflict connectivity.ErrPrimaryConflict
		if errors.As(err, &conflict) {
			http.Error(w, "primary conflict: "+conflict.ExistingID, http.StatusConflict)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: update integration", "id", id, "err", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/integrations/"+id, http.StatusSeeOther)
}

// handleIntegrationDelete handles POST /console/integrations/{id}/delete
// (_method=DELETE). Gated by ReadOnly. Permanently removes the integration.
func (c *Console) handleIntegrationDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleIntegrationDelete: w is nil")
	}
	if r == nil {
		panic("handleIntegrationDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleIntegrationDelete: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := c.deps.Integrations.Delete(r.Context(), scope, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: delete integration", "id", id, "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/integrations", http.StatusSeeOther)
}

// handleIntegrationToggleActive handles POST /console/integrations/{id}/active
// (_method=PATCH). Gated by ReadOnly. Atomically flips the active flag.
func (c *Console) handleIntegrationToggleActive(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleIntegrationToggleActive: w is nil")
	}
	if r == nil {
		panic("handleIntegrationToggleActive: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleIntegrationToggleActive: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	// "active" form field: "true" | "false" | "on" (checkbox).
	active := r.FormValue("active") == "true" || r.FormValue("active") == "on"

	if err := c.deps.Integrations.SetActive(r.Context(), scope, id, active); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: toggle active", "id", id, "active", active, "err", err)
		http.Error(w, "toggle failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/integrations/"+id, http.StatusSeeOther)
}
