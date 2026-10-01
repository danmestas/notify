// integrations_test.go proves the W2 integrations store page and fragment
// endpoints. Pattern follows analytics_test.go: production services over a
// temp SQLite store, full login/session stack, white-box template assertions.
//
// Test index:
//
//	TestIntegrations_PageRenderWithSeededIntegrations  — page renders rows
//	TestIntegrations_PageEmptyState                    — empty store empty-states
//	TestIntegrations_UnauthenticatedRedirectsToLogin   — auth gate
//	TestIntegrations_NavEntryIsLive                    — nav link present
//	TestIntegrations_CredentialsNeverRenderedRaw       — sentinel, not real secrets
//	TestFragment_IntegrationsGrid_SSEFraming           — SSE content-type
//	TestFragment_IntegrationsGrid_FilterByChannel      — channel filter
//	TestFragment_IntegrationsGrid_SeededRows           — row content present
//	TestFragment_IntegrationDetail_SSEFraming          — detail SSE
//	TestFragment_IntegrationDetail_CredentialsMasked   — credentials are masked
//	TestFragment_IntegrationDetail_EmptyOnMissingID    — missing id → empty panel
//	TestIntegrations_MutationBlockedWhenReadOnly       — ReadOnly gate on mutations
//	TestIntegrations_ToggleActive_ReadOnlyBlocked      — ReadOnly toggle gate
//	TestIntegrations_Delete_ReadOnlyBlocked            — ReadOnly delete gate
//	TestIntegrations_Update_ReadOnlyBlocked            — ReadOnly update gate
//	TestIntegrations_Create_ReadOnlyBlocked            — ReadOnly create gate
//	TestIntegrations_ScopeIsolation                    — alt-scope rows not visible
//	TestIntegrations_RouteAuth                         — all new routes behind session gate
package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
)

// ─── page render ─────────────────────────────────────────────────────────────

func TestIntegrations_PageRenderWithSeededIntegrations(t *testing.T) {
	tc := newTestConsole(t)

	tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Primary:        true,
		Identifier:     "smtp-primary",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations: status = %d, want 200 (body: %s)",
			rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	if !strings.Contains(body, "Integration Store") {
		t.Errorf("integrations page missing title")
	}
	if !strings.Contains(body, "smtp-primary") {
		t.Errorf("integrations page missing seeded identifier")
	}
	if !strings.Contains(body, `id="integrations-grid"`) {
		t.Errorf("integrations page missing #integrations-grid")
	}
	if !strings.Contains(body, `id="integration-detail"`) {
		t.Errorf("integrations page missing #integration-detail")
	}
	// Credentials must NEVER appear raw.
	if strings.Contains(body, "fake-ciphertext") {
		t.Errorf("integrations page leaked raw enc_credentials blob")
	}
}

// TestIntegrations_ActiveBadgeHasTooltip proves the "active" pill carries a
// title distinguishing enabled-vs-verified (L2): it can otherwise read as a
// live health check when it only means "enabled / not deactivated".
func TestIntegrations_ActiveBadgeHasTooltip(t *testing.T) {
	tc := newTestConsole(t)
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-primary",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, _ := tc.login(t)

	grid := tc.get(t, "/console/api/fragments/integrations-grid", cookie).Body.String()
	if !strings.Contains(grid, `title="Enabled`) {
		t.Errorf("integrations grid active badge missing enabled-vs-verified title; body:\n%s", grid)
	}

	detail := tc.get(t, "/console/api/fragments/integration-detail?id="+in.ID, cookie).Body.String()
	if !strings.Contains(detail, `title="Enabled`) {
		t.Errorf("integration detail active badge missing enabled-vs-verified title; body:\n%s", detail)
	}
}

// TestIntegrations_ToolbarUsesDesignSystem guards the integration toolbar
// against regressing to the unstyled raw controls it shipped with: the channel
// filters must render as a styled segmented group (.integration-channel-tabs >
// .integration-tab, with aria-pressed reflecting the active channel signal),
// and the create action must be the design-system primary button living in the
// toolbar's right-aligned action slot — matching the "+ New" CTA on every other
// list page — not a bare btn-outline wedged beneath the filters.
func TestIntegrations_ToolbarUsesDesignSystem(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations: status = %d, want 200 (body: %s)",
			rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Segmented filter group present, with the active state exposed to AT.
	if !strings.Contains(body, `class="integration-channel-tabs"`) {
		t.Error("integrations toolbar missing segmented filter group .integration-channel-tabs")
	}
	if !strings.Contains(body, `data-attr:aria-pressed`) {
		t.Error("channel filter tabs must bind aria-pressed to reflect the active channel")
	}
	// Connect Provider must be the primary button in the relocated action slot.
	if !strings.Contains(body, `class="btn btn-primary integration-toolbar-action"`) {
		t.Error("Connect Provider must use the design-system primary button in the toolbar action slot (btn btn-primary integration-toolbar-action)")
	}
}

// TestFragment_IntegrationProviderForm_ListsFullCatalog guards against the
// picker regressing to a hardcoded subset. The step-1 provider grid must be
// driven by providerCatalog, so every supported provider — including the ones
// that were previously missing from the UI (Expo, FCM, Discord, Telegram,
// Twilio, SignalWire) — renders a selectable tile.
func TestFragment_IntegrationProviderForm_ListsFullCatalog(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/integration-provider-form", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("provider form fragment: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	for _, p := range providerCatalog {
		if !strings.Contains(body, ">"+p.Label+"<") {
			t.Errorf("provider picker missing catalog entry %q (id=%s)", p.Label, p.ID)
		}
		// The pick rides the integrationProviderPick signal (Datastar drops a
		// literal ?provider=); the row sets it then refetches the fragment.
		if !strings.Contains(body, "$integrationProviderPick='"+p.ID+"'") {
			t.Errorf("provider picker missing signal-based select for id=%s", p.ID)
		}
	}
}

// TestFragment_IntegrationProviderForm_SMTPAddrHostClarified is a regression
// guard for L4: the Custom SMTP connect form required BOTH "Address
// (host:port)" and "Host" with no help text, reading as redundant. Addr is the
// real dial target (net.DialTimeout); Host is only a TLS SNI / EHLO override
// probeSMTP falls back to deriving from Addr when blank — so Host must not be
// required, and both fields need help text making the distinction legible.
func TestFragment_IntegrationProviderForm_SMTPAddrHostClarified(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/integration-provider-form",
		url.Values{"provider": {"smtp"}}.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("smtp form: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	addrIdx := strings.Index(body, `name="addr"`)
	hostIdx := strings.Index(body, `name="host"`)
	usernameIdx := strings.Index(body, `name="username"`)
	if addrIdx < 0 || hostIdx < 0 || usernameIdx < 0 {
		t.Fatalf("smtp form missing addr/host/username fields; body:\n%s", body)
	}
	// Each field's block runs from its <input name="..."> to the start of the
	// next field's, so it captures that field's own trailing help text.
	addrField := body[addrIdx:hostIdx]
	hostField := body[hostIdx:usernameIdx]

	if !strings.Contains(addrField, "form-hint") {
		t.Errorf("Address field missing help text distinguishing it from Host; field:\n%s", addrField)
	}
	if !strings.Contains(hostField, "form-hint") {
		t.Errorf("Host field missing help text distinguishing it from Address; field:\n%s", hostField)
	}
	if strings.Contains(hostField, "required") {
		t.Errorf("Host must not be required — it's an optional TLS/EHLO override derived from Address when blank; field:\n%s", hostField)
	}
}

// TestFragment_IntegrationProviderForm_ChatCredFields verifies the two chat
// providers that previously had no console form now render their credential
// inputs, so they can actually be connected from the UI.
func TestFragment_IntegrationProviderForm_ChatCredFields(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	for _, tt := range []struct{ provider, field string }{
		{"discord", "webhook_url"},
		{"telegram", "bot_token"},
		{"slack", "bot_token"},
		{"whatsapp", "access_token"},
		{"whatsapp", "phone_number_id"},
		{"resend", "api_key"},
	} {
		rr := tc.getWithQuery(t, "/console/api/fragments/integration-provider-form",
			url.Values{"provider": {tt.provider}}.Encode(), cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s form: status = %d, want 200", tt.provider, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), `name="`+tt.field+`"`) {
			t.Errorf("%s form missing credential field %q", tt.provider, tt.field)
		}
	}
}

// TestFragment_IntegrationProviderForm_SecretFieldsMasked verifies the new
// secret credential fields (api_key, bot_token, access_token) render as
// password inputs so the browser never shows the typed secret.
func TestFragment_IntegrationProviderForm_SecretFieldsMasked(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	for _, tt := range []struct{ provider, field string }{
		{"resend", "api_key"},
		{"slack", "bot_token"},
		{"whatsapp", "access_token"},
		{"mastodon", "access_token"},
		{"bluesky", "app_password"},
	} {
		rr := tc.getWithQuery(t, "/console/api/fragments/integration-provider-form",
			url.Values{"provider": {tt.provider}}.Encode(), cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s form: status = %d, want 200", tt.provider, rr.Code)
		}
		body := rr.Body.String()
		// The password input and its name attribute share the same <input> tag,
		// so require type="password" followed by the secret field's name.
		marker := `type="password"`
		idx := strings.Index(body, `name="`+tt.field+`"`)
		if idx < 0 {
			t.Fatalf("%s form missing credential field %q", tt.provider, tt.field)
		}
		// Find the <input ...> opening tag containing this name attribute.
		start := strings.LastIndex(body[:idx], "<input")
		if start < 0 || !strings.Contains(body[start:idx], marker) {
			t.Errorf("%s field %q is not rendered as a password input", tt.provider, tt.field)
		}
	}
}

// TestFragment_IntegrationProviderForm_AWSCredFields verifies the Amazon SES
// and SNS provider forms render their credential inputs and that
// secret_access_key is masked as a password input so the browser never shows
// the typed signing secret.
func TestFragment_IntegrationProviderForm_AWSCredFields(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	for _, tt := range []struct {
		provider string
		fields   []string
	}{
		{"ses", []string{"access_key_id", "secret_access_key", "region", "from"}},
		{"sns", []string{"access_key_id", "secret_access_key", "region"}},
	} {
		rr := tc.getWithQuery(t, "/console/api/fragments/integration-provider-form",
			url.Values{"provider": {tt.provider}}.Encode(), cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s form: status = %d, want 200", tt.provider, rr.Code)
		}
		body := rr.Body.String()
		for _, f := range tt.fields {
			if !strings.Contains(body, `name="`+f+`"`) {
				t.Errorf("%s form missing credential field %q", tt.provider, f)
			}
		}
		// secret_access_key must render as a password input.
		idx := strings.Index(body, `name="secret_access_key"`)
		if idx < 0 {
			t.Fatalf("%s form missing secret_access_key", tt.provider)
		}
		start := strings.LastIndex(body[:idx], "<input")
		if start < 0 || !strings.Contains(body[start:idx], `type="password"`) {
			t.Errorf("%s secret_access_key is not rendered as a password input", tt.provider)
		}
	}
}

// TestFragment_IntegrationProviderForm_WebhookCredFields verifies the three
// generic-webhook providers render their webhook_url + hmac_secret credential
// inputs so they can be connected from the UI.
func TestFragment_IntegrationProviderForm_WebhookCredFields(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	for _, provider := range []string{"email-webhook", "push-webhook", "chat-webhook"} {
		rr := tc.getWithQuery(t, "/console/api/fragments/integration-provider-form",
			url.Values{"provider": {provider}}.Encode(), cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s form: status = %d, want 200", provider, rr.Code)
		}
		body := rr.Body.String()
		for _, field := range []string{"webhook_url", "hmac_secret"} {
			if !strings.Contains(body, `name="`+field+`"`) {
				t.Errorf("%s form missing credential field %q", provider, field)
			}
		}
	}
}

// TestFragment_IntegrationProviderForm_ChannelTabs verifies the
// drawer renders the channel-tab bar that filters the provider list
// client-side: the four channel tab labels plus the $connectChannel signal
// that drives their active state and per-row visibility.
func TestFragment_IntegrationProviderForm_ChannelTabs(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/integration-provider-form", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("provider form fragment: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	for _, label := range []string{"E-Mail", "SMS", "Push", "Chat"} {
		if !strings.Contains(body, ">"+label+"<") {
			t.Errorf("channel-tab bar missing tab label %q", label)
		}
	}
	if !strings.Contains(body, "$connectChannel") {
		t.Error("channel-tab bar must drive the $connectChannel signal")
	}
}

// TestFragment_IntegrationProviderForm_RowsHaveIcons verifies the step-1 list
// renders every catalog provider as a row carrying both its label and an
// inlined brand SVG, gated by channel via the $connectChannel signal.
func TestFragment_IntegrationProviderForm_RowsHaveIcons(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/integration-provider-form", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("provider form fragment: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// Every catalog entry renders a row with a signal-based select + label.
	for _, p := range providerCatalog {
		if !strings.Contains(body, "$integrationProviderPick='"+p.ID+"'") {
			t.Errorf("provider list missing signal-based select for id=%s", p.ID)
		}
		if !strings.Contains(body, ">"+p.Label+"<") {
			t.Errorf("provider list missing label %q (id=%s)", p.Label, p.ID)
		}
	}

	// Rows are gated by channel client-side (no refetch on tab switch).
	if !strings.Contains(body, "$connectChannel === 'email'") {
		t.Error("provider rows must gate visibility on $connectChannel by channel")
	}

	// Each provider row inlines its brand SVG. Require at least one <svg per
	// row (one per catalog entry) and verify a sample provider's icon shows up.
	svgCount := strings.Count(body, "<svg")
	if svgCount < len(providerCatalog) {
		t.Errorf("provider list rendered %d <svg, want >= %d (one per provider)",
			svgCount, len(providerCatalog))
	}
}

// TestIntegrations_ConnectChannelSignalSeeded verifies the page seeds the
// connectChannel signal (default "email") so the drawer's channel tabs work
// before any interaction.
func TestIntegrations_ConnectChannelSignalSeeded(t *testing.T) {
	if !strings.Contains(integrationsSignalsJSON("", ""), `"connectChannel":"email"`) {
		t.Errorf("integrationsSignalsJSON must seed connectChannel=email; got %s",
			integrationsSignalsJSON("", ""))
	}

	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations: status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `connectChannel`) {
		t.Error("integrations page data-signals missing connectChannel")
	}
}

// TestIntegrations_DrawersUseSharedSlideover verifies both integration drawers
// (connect + detail) reuse the canonical .console-slideover chrome rather than
// any bespoke drawer classes, so every drawer in the app shares one pattern.
func TestIntegrations_DrawersUseSharedSlideover(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// Two drawers, both on the shared slide-over chrome.
	if got := strings.Count(body, "console-slideover "); got < 2 {
		t.Errorf("integrations page has %d .console-slideover panels, want >= 2 (connect + detail)", got)
	}
	// The connect drawer's close uses the canonical close control.
	if !strings.Contains(body, "console-slideover-close") {
		t.Error("connect drawer must use the canonical .console-slideover-close control")
	}
	// Connect drawer title is present.
	if !strings.Contains(body, ">Connect Integration<") {
		t.Error("connect drawer missing 'Connect Integration' title")
	}

	// The bespoke chrome classes must be fully retired (CSS rules removed).
	if strings.Contains(body, "integration-connect-backdrop") {
		t.Error("bespoke integration-connect-backdrop must be removed (no backdrop; consistency with shared drawers)")
	}
	if strings.Contains(body, "integration-connect-drawer-header") ||
		strings.Contains(body, "integration-connect-drawer-close") {
		t.Error("bespoke integration-connect-drawer chrome classes must be retired in favour of .console-slideover")
	}
}

// TestIntegrations_Create_WebhookProviders exercises the console create path for
// the three generic-webhook providers end-to-end. A successful create redirects
// (303).
func TestIntegrations_Create_WebhookProviders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	for _, tt := range []struct {
		provider string
		form     url.Values
	}{
		{"email-webhook", url.Values{
			"provider_id": {"email-webhook"}, "identifier": {"ewh-1"},
			"webhook_url": {"https://hooks.example.com/email"},
			"hmac_secret": {"s1"},
			"csrf_token":  {csrf},
		}},
		{"push-webhook", url.Values{
			"provider_id": {"push-webhook"}, "identifier": {"pwh-1"},
			"webhook_url": {"https://hooks.example.com/push"},
			"csrf_token":  {csrf},
		}},
		{"chat-webhook", url.Values{
			"provider_id": {"chat-webhook"}, "identifier": {"cwh-1"},
			"webhook_url": {"https://hooks.example.com/chat"},
			"hmac_secret": {"s3"},
			"csrf_token":  {csrf},
		}},
	} {
		rr := tc.postForm(t, "/console/integrations", tt.form, cookie)
		if rr.Code != http.StatusSeeOther {
			t.Errorf("create %s: status = %d, want 303 (body: %s)",
				tt.provider, rr.Code, rr.Body.String())
		}
	}
}

// TestIntegrations_Create_ChatProviders exercises the console create path for
// discord and telegram end-to-end — they used to fall through to the "unknown
// provider" default and 400. A successful create redirects (303).
func TestIntegrations_Create_ChatProviders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	for _, tt := range []struct {
		provider string
		form     url.Values
	}{
		{"discord", url.Values{
			"provider_id": {"discord"}, "identifier": {"disc-1"},
			"webhook_url": {"https://discord.com/api/webhooks/1/abc"},
			"csrf_token":  {csrf},
		}},
		{"telegram", url.Values{
			"provider_id": {"telegram"}, "identifier": {"tg-1"},
			"bot_token":  {"123456:ABC-DEF"},
			"csrf_token": {csrf},
		}},
		{"slack", url.Values{
			"provider_id": {"slack"}, "identifier": {"slack-1"},
			"bot_token":  {"xoxb-abc"},
			"csrf_token": {csrf},
		}},
		{"whatsapp", url.Values{
			"provider_id": {"whatsapp"}, "identifier": {"wa-1"},
			"access_token":    {"EAAG-abc"},
			"phone_number_id": {"109999"},
			"csrf_token":      {csrf},
		}},
	} {
		rr := tc.postForm(t, "/console/integrations", tt.form, cookie)
		if rr.Code != http.StatusSeeOther {
			t.Errorf("create %s: status = %d, want 303 (body: %s)",
				tt.provider, rr.Code, rr.Body.String())
		}
	}
}

// TestIntegrations_Create_Resend exercises the console create path for the
// resend email provider end-to-end. A successful create redirects (303).
func TestIntegrations_Create_Resend(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	form := url.Values{
		"provider_id": {"resend"}, "identifier": {"resend-1"},
		"api_key":    {"re_abc"},
		"from":       {"no-reply@example.com"},
		"csrf_token": {csrf},
	}
	rr := tc.postForm(t, "/console/integrations", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("create resend: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestIntegrations_Create_WebPush proves the console create path for the
// "webpush" catalog entry goes through the same EnsureWebPushKeys the API's
// GET /v1/integrations/webpush/key handler uses (console and API must agree
// on the singleton per-scope integration): no credential form fields are
// posted (there is no operator-entered form for auto-minted VAPID keys), the
// create still 303s, a "webpush" row appears in the scope, and the detail
// fragment surfaces the minted public key read-only.
func TestIntegrations_Create_WebPush(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	form := url.Values{
		"provider_id": {"webpush"},
		"identifier":  {"webpush"},
		"csrf_token":  {csrf},
	}
	rr := tc.postForm(t, "/console/integrations", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create webpush: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	integrations, err := tc.st.Integrations().List(testBackground(), tc.scope)
	if err != nil {
		t.Fatalf("list integrations: %v", err)
	}
	var in *domain.Integration
	for i := range integrations {
		if integrations[i].ProviderID == "webpush" {
			in = &integrations[i]
			break
		}
	}
	if in == nil {
		t.Fatalf("no webpush integration row after create; rows: %+v", integrations)
	}

	pub, _, err := tc.deps.Connectivity.EnsureWebPushKeys(testBackground(), tc.scope)
	if err != nil {
		t.Fatalf("EnsureWebPushKeys: %v", err)
	}
	if pub == "" {
		t.Fatal("EnsureWebPushKeys returned an empty public key")
	}

	detail := tc.get(t, "/console/api/fragments/integration-detail?id="+in.ID, cookie).Body.String()
	if !strings.Contains(detail, pub) {
		t.Errorf("integration detail missing the minted public key %q; body:\n%s", pub, detail)
	}
	if strings.Contains(detail, "••••••") {
		t.Errorf("integration detail rendered a masked-secret placeholder for webpush, which has no secret fields; body:\n%s", detail)
	}
}

// TestIntegrations_Detail_WebPush_NoTestSendButton proves the detail drawer
// does not offer Test send for a "webpush" integration: connectivity.TestSend
// routes to webpush.Client.Send, which always returns "not supported" (the
// provider only delivers per-device via SendToSubscription, which test-send
// has no subscribed device to target). CanTestSend must be false for webpush
// even though Connectivity is wired and the console is not read-only.
func TestIntegrations_Detail_WebPush_NoTestSendButton(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	if _, _, err := tc.deps.Connectivity.EnsureWebPushKeys(testBackground(), tc.scope); err != nil {
		t.Fatalf("EnsureWebPushKeys: %v", err)
	}
	integrations, err := tc.st.Integrations().List(testBackground(), tc.scope)
	if err != nil {
		t.Fatalf("list integrations: %v", err)
	}
	var in *domain.Integration
	for i := range integrations {
		if integrations[i].ProviderID == "webpush" {
			in = &integrations[i]
			break
		}
	}
	if in == nil {
		t.Fatalf("no webpush integration row; rows: %+v", integrations)
	}

	detail := tc.get(t, "/console/api/fragments/integration-detail?id="+in.ID, cookie).Body.String()
	if strings.Contains(detail, "/console/integrations/"+in.ID+"/test-send") {
		t.Errorf("webpush integration detail offers a test-send form; body:\n%s", detail)
	}
}

// TestIntegrations_Create_DuplicateIdentifierRejected guards H4: POSTing
// create with an identifier that already exists in scope must NOT 500 and
// must NOT overwrite the existing integration. The handler must 303 back to
// the integrations page with an inline ?createError= banner, matching the
// Agents duplicate-identifier pattern.
func TestIntegrations_Create_DuplicateIdentifierRejected(t *testing.T) {
	tc := newTestConsole(t)
	_, err := tc.deps.Connectivity.CreateSMTP(
		testBackground(), tc.scope, "dup-smtp",
		connectivity.SMTPConfig{
			Addr:     "smtp.example.com:587",
			Host:     "smtp.example.com",
			Username: "user@example.com",
			Password: "orig-pass",
			From:     "noreply@example.com",
		},
		false,
	)
	if err != nil {
		t.Fatalf("seed CreateSMTP: %v", err)
	}

	cookie, csrf := tc.login(t)
	form := url.Values{
		"provider_id": {"smtp"}, "identifier": {"dup-smtp"},
		"addr": {"evil.example.com:587"}, "host": {"evil.example.com"},
		"username": {"attacker"}, "password": {"clobber"},
		"from":       {"attacker@example.com"},
		"csrf_token": {csrf},
	}
	rr := tc.postForm(t, "/console/integrations", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("duplicate create: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/console/integrations") {
		t.Errorf("duplicate create: Location = %q, want redirect back to /console/integrations", loc)
	}
	if !strings.Contains(loc, "createError=") {
		t.Errorf("duplicate create: Location = %q, missing an error param", loc)
	}

	// The existing integration must not have been clobbered or duplicated.
	integrations, err := tc.st.Integrations().List(testBackground(), tc.scope)
	if err != nil {
		t.Fatalf("list integrations: %v", err)
	}
	count := 0
	for _, in := range integrations {
		if in.Identifier == "dup-smtp" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("duplicate create: identifier count = %d, want 1 (no clobber, no dupe row)", count)
	}

	// The redirect target must render the inline error banner, not a raw 500.
	rrGet := tc.get(t, loc, cookie)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", loc, rrGet.Code)
	}
	if !strings.Contains(rrGet.Body.String(), "already exists") {
		t.Errorf("error page missing 'already exists' message; body snippet: %s", rrGet.Body.String()[:min(500, rrGet.Body.Len())])
	}
}

// TestIntegrations_Create_RejectsInvalidIdentifier proves the same H2-class
// fix for integration identifiers: an identifier with URL-unsafe characters
// (space, '/', '<script>') is rejected inline instead of being persisted and
// permanently orphaned (every view/edit/delete route 404s on an un-encoded
// path segment).
func TestIntegrations_Create_RejectsInvalidIdentifier(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	for _, id := range []string{"q2adv5 bad key!! <script>", "foo/bar", "a b", "Foo", "<script>"} {
		form := url.Values{
			"provider_id": {"smtp"}, "identifier": {id},
			"addr": {"smtp.example.com:587"}, "host": {"smtp.example.com"},
			"username": {"user"}, "password": {"pass"},
			"from":       {"noreply@example.com"},
			"csrf_token": {csrf},
		}
		rr := tc.postForm(t, "/console/integrations", form, cookie)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("id=%q: create integration: status = %d, want 303 (body: %s)", id, rr.Code, rr.Body.String())
		}
		loc := rr.Header().Get("Location")
		if !strings.Contains(loc, "/console/integrations") {
			t.Errorf("id=%q: Location = %q, want redirect back to /console/integrations", id, loc)
		}
		if !strings.Contains(loc, "createError=") {
			t.Errorf("id=%q: Location = %q, missing an error param", id, loc)
		}

		integrations, err := tc.st.Integrations().List(testBackground(), tc.scope)
		if err != nil {
			t.Fatalf("id=%q: list integrations: %v", id, err)
		}
		for _, in := range integrations {
			if in.Identifier == id {
				t.Fatalf("id=%q: integration row was written despite invalid identifier; want no row", id)
			}
		}

		rrGet := tc.get(t, loc, cookie)
		if rrGet.Code != http.StatusOK {
			t.Fatalf("id=%q: GET %s: status = %d, want 200", id, loc, rrGet.Code)
		}
		if !strings.Contains(rrGet.Body.String(), "lowercase letters, numbers, hyphens, and underscores") {
			t.Errorf("id=%q: page missing validation error message; body snippet: %s", id, rrGet.Body.String()[:min(500, rrGet.Body.Len())])
		}
	}
}

// TestIntegrations_Create_AcceptsValidIdentifier proves a valid identifier is
// created (the inverse of TestIntegrations_Create_RejectsInvalidIdentifier).
func TestIntegrations_Create_AcceptsValidIdentifier(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	form := url.Values{
		"provider_id": {"smtp"}, "identifier": {"valid-smtp-id"},
		"addr": {"smtp.example.com:587"}, "host": {"smtp.example.com"},
		"username": {"user"}, "password": {"pass"},
		"from":       {"noreply@example.com"},
		"csrf_token": {csrf},
	}
	rr := tc.postForm(t, "/console/integrations", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create integration: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	integrations, err := tc.st.Integrations().List(testBackground(), tc.scope)
	if err != nil {
		t.Fatalf("list integrations: %v", err)
	}
	found := false
	for _, in := range integrations {
		if in.Identifier == "valid-smtp-id" {
			found = true
		}
	}
	if !found {
		t.Fatalf("valid identifier was not persisted")
	}
}

func TestIntegrations_PageEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No integrations yet") {
		t.Errorf("empty integrations page missing empty-state copy")
	}
}

func TestIntegrations_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/integrations", nil)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated /console/integrations: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("redirect location = %q, want /console/login", got)
	}
}

func TestIntegrations_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/integrations"`) {
		t.Errorf("integrations page missing nav link")
	}
	// Nav entry should be marked active.
	if !strings.Contains(body, `is-active`) {
		t.Errorf("integrations page nav link not marked as active")
	}
}

// TestIntegrations_CredentialsNeverRenderedRaw is the key security assertion:
// the page HTML must contain the masked sentinel but never the raw secret.
func TestIntegrations_CredentialsNeverRenderedRaw(t *testing.T) {
	tc := newTestConsole(t)

	// Seed via connectivity so real credentials are stored encrypted.
	_, err := tc.deps.Connectivity.CreateSMTP(
		testBackground(), tc.scope, "smtp-test",
		connectivity.SMTPConfig{
			Addr:     "smtp.example.com:587",
			Host:     "smtp.example.com",
			Username: "user@example.com",
			Password: "s3cr3t-p4ssword",
			From:     "noreply@example.com",
		},
		false,
	)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}

	integrations, err := tc.st.Integrations().List(testBackground(), tc.scope)
	if err != nil || len(integrations) == 0 {
		t.Fatalf("list integrations: err=%v, count=%d", err, len(integrations))
	}
	id := integrations[0].ID

	cookie, _ := tc.login(t)

	// Check page
	rr := tc.get(t, "/console/integrations", cookie)
	body := rr.Body.String()
	if strings.Contains(body, "s3cr3t-p4ssword") {
		t.Errorf("page leaked raw password in page HTML")
	}

	// Check detail fragment
	rr = tc.getWithQuery(t, "/console/api/fragments/integration-detail",
		url.Values{"id": {id}}.Encode(), cookie)
	body = rr.Body.String()
	if strings.Contains(body, "s3cr3t-p4ssword") {
		t.Errorf("detail fragment leaked raw password")
	}
	// Sentinel must be present.
	if !strings.Contains(body, connectivity.CredentialMaskSentinel) {
		t.Errorf("detail fragment missing credential mask sentinel %q", connectivity.CredentialMaskSentinel)
	}
}

// ─── grid fragment ────────────────────────────────────────────────────────────

func TestFragment_IntegrationsGrid_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-1",
		EncCredentials: []byte("fake"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/integrations-grid", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("integrations grid fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("grid fragment missing SSE event marker")
	}
	if !strings.Contains(body, `id="integrations-grid"`) {
		t.Errorf("grid fragment missing stable patch target id")
	}
}

func TestFragment_IntegrationsGrid_SeededRows(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-visible",
		EncCredentials: []byte("fake"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/integrations-grid", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "smtp-visible") {
		t.Errorf("grid fragment missing seeded identifier")
	}
}

func TestFragment_IntegrationsGrid_FilterByChannel(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-email",
		EncCredentials: []byte("fake"),
	})
	tc.seedIntegration(t, domain.Integration{
		ProviderID:     "ntfy",
		Channel:        "push",
		Active:         true,
		Identifier:     "ntfy-push",
		EncCredentials: []byte("fake"),
	})
	cookie, _ := tc.login(t)

	// Filter for email only — should see smtp but not ntfy.
	rr := tc.getWithQuery(t, "/console/api/fragments/integrations-grid",
		url.Values{"channel": {"email"}}.Encode(), cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "smtp-email") {
		t.Errorf("email-filtered grid missing smtp-email")
	}
	if strings.Contains(body, "ntfy-push") {
		t.Errorf("email-filtered grid leaked ntfy-push")
	}

	// Filter for push only.
	rr = tc.getWithQuery(t, "/console/api/fragments/integrations-grid",
		url.Values{"channel": {"push"}}.Encode(), cookie)
	body = rr.Body.String()
	if !strings.Contains(body, "ntfy-push") {
		t.Errorf("push-filtered grid missing ntfy-push")
	}
	if strings.Contains(body, "smtp-email") {
		t.Errorf("push-filtered grid leaked smtp-email")
	}
}

// ─── detail fragment ──────────────────────────────────────────────────────────

func TestFragment_IntegrationDetail_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	row := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-detail",
		EncCredentials: []byte("fake"),
	})
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/integration-detail",
		url.Values{"id": {row.ID}}.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("detail fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("detail fragment missing SSE event marker")
	}
	if !strings.Contains(body, `id="integration-detail"`) {
		t.Errorf("detail fragment missing stable patch target id")
	}
}

func TestFragment_IntegrationDetail_CredentialsMasked(t *testing.T) {
	tc := newTestConsole(t)

	_, err := tc.deps.Connectivity.CreateSMTP(
		testBackground(), tc.scope, "smtp-masked",
		connectivity.SMTPConfig{
			Addr:     "smtp.example.com:587",
			Host:     "smtp.example.com",
			Username: "u@example.com",
			Password: "top-secret",
			From:     "noreply@example.com",
		},
		false,
	)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}
	integrations, err := tc.st.Integrations().List(testBackground(), tc.scope)
	if err != nil || len(integrations) == 0 {
		t.Fatalf("list: err=%v count=%d", err, len(integrations))
	}

	cookie, _ := tc.login(t)
	rr := tc.getWithQuery(t, "/console/api/fragments/integration-detail",
		url.Values{"id": {integrations[0].ID}}.Encode(), cookie)
	body := rr.Body.String()

	if strings.Contains(body, "top-secret") {
		t.Errorf("detail fragment leaked raw password 'top-secret'")
	}
	if !strings.Contains(body, connectivity.CredentialMaskSentinel) {
		t.Errorf("detail fragment missing sentinel %q", connectivity.CredentialMaskSentinel)
	}
}

func TestFragment_IntegrationDetail_EmptyOnMissingID(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	// No id param → empty panel.
	rr := tc.get(t, "/console/api/fragments/integration-detail", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty detail fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `id="integration-detail"`) {
		t.Errorf("empty detail fragment missing target id")
	}

	// Non-existent id → empty panel (not 404).
	rr = tc.getWithQuery(t, "/console/api/fragments/integration-detail",
		url.Values{"id": {"nonexistent-id"}}.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("not-found detail fragment: status = %d", rr.Code)
	}
}

// ─── read-only gate ────────────────────────────────────────────────────────

// newReadOnlyTestConsole is newTestConsole but with ReadOnly=true.
func newReadOnlyTestConsole(t *testing.T) *testConsole {
	t.Helper()
	tc := newTestConsole(t)
	tc.deps.ReadOnly = true
	// Rebuild with ReadOnly set.
	c := New(tc.deps)
	tc.c = c
	tc.h = c.Routes()
	return tc
}

func TestIntegrations_MutationBlockedWhenReadOnly(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, _ := tc.login(t)

	// The page should render (GET is still allowed).
	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Errorf("read-only integrations page: status = %d, want 200", rr.Code)
	}
}

func TestIntegrations_Create_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/integrations",
		url.Values{"provider_id": {"smtp"}, "identifier": {"x"}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only create: status = %d, want 403", rr.Code)
	}
}

func TestIntegrations_Update_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	row := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "ro-update",
		EncCredentials: []byte("fake"),
	})
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/integrations/"+row.ID,
		url.Values{"_method": {"PUT"}, "identifier": {"changed"}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only update: status = %d, want 403", rr.Code)
	}
}

func TestIntegrations_Delete_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	row := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "ro-delete",
		EncCredentials: []byte("fake"),
	})
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/integrations/"+row.ID+"/delete",
		url.Values{},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only delete: status = %d, want 403", rr.Code)
	}
}

func TestIntegrations_ToggleActive_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	row := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "ro-toggle",
		EncCredentials: []byte("fake"),
	})
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/integrations/"+row.ID+"/active",
		url.Values{"active": {"false"}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only toggle: status = %d, want 403", rr.Code)
	}
}

// ─── scope isolation ─────────────────────────────────────────────────────────

func TestIntegrations_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	altScope := tc.altScope(t)

	// Seed one integration under the test scope and one under the alt scope.
	tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "mine",
		EncCredentials: []byte("fake"),
	})
	// Seed directly into altScope via store.
	_, err := tc.st.Integrations().Create(testBackground(), altScope, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "not-mine",
		EncCredentials: []byte("fake"),
	})
	if err != nil {
		t.Fatalf("seed alt integration: %v", err)
	}

	cookie, _ := tc.login(t)

	// Page must show "mine" but not "not-mine".
	rr := tc.get(t, "/console/integrations", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "mine") {
		t.Errorf("page missing own integration 'mine'")
	}
	if strings.Contains(body, "not-mine") {
		t.Errorf("page leaked alt-scope integration 'not-mine'")
	}
}

// ─── route auth ───────────────────────────────────────────────────────────────

// ─── W4-I2: Activity tab ─────────────────────────────────────────────────────

// TestFragment_IntegrationActivity_EmptyState verifies that the activity
// fragment renders an empty-state when the integration has no messages.
func TestFragment_IntegrationActivity_EmptyState(t *testing.T) {
	tc := newTestConsole(t)
	row := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-act-empty",
		EncCredentials: []byte("fake"),
	})
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/integration-activity",
		url.Values{"id": {row.ID}}.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("integration-activity fragment: status = %d body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("activity fragment missing SSE event marker")
	}
	if !strings.Contains(body, `id="integration-detail"`) {
		t.Errorf("activity fragment missing stable patch target id")
	}
	if !strings.Contains(body, "No messages yet") {
		t.Errorf("activity fragment missing empty-state text %q; body: %s", "No messages yet", body)
	}
}

// TestFragment_IntegrationActivity_RendersRows verifies that the activity
// fragment renders message rows when the integration has messages.
func TestFragment_IntegrationActivity_RendersRows(t *testing.T) {
	tc := newTestConsole(t)
	row := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-act-rows",
		EncCredentials: []byte("fake"),
	})

	// Seed a message attributed to this integration.
	tc.seedMessage(t, domain.Message{
		WorkflowID:     "wf1",
		NotificationID: "ntf1",
		RunID:          "run1",
		StepID:         "step1",
		SubscriberID:   "sub-activity",
		Channel:        "email",
		ProviderID:     "smtp",
		IntegrationID:  row.ID,
		Status:         "sent",
	})

	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/integration-activity",
		url.Values{"id": {row.ID}}.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("activity fragment with rows: status = %d body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sub-activity") {
		t.Errorf("activity fragment missing subscriber ID 'sub-activity'; body excerpt: %.500s", body)
	}
	if !strings.Contains(body, "sent") {
		t.Errorf("activity fragment missing status 'sent'; body excerpt: %.500s", body)
	}
	if strings.Contains(body, "No messages yet") {
		t.Errorf("activity fragment shows empty-state despite having a row")
	}
}

// TestFragment_IntegrationActivity_AuthGuard verifies the new route sits
// behind the session gate.
func TestFragment_IntegrationActivity_AuthGuard(t *testing.T) {
	tc := newTestConsole(t)

	rr := tc.get(t, "/console/api/fragments/integration-activity", nil)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("activity fragment without auth: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("activity fragment Location = %q, want /console/login", got)
	}
}

// TestFragment_IntegrationActivity_TabBarPresent verifies the Activity tab
// button appears in the drawer when the activity tab is active.
func TestFragment_IntegrationActivity_TabBarPresent(t *testing.T) {
	tc := newTestConsole(t)
	row := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-tab-check",
		EncCredentials: []byte("fake"),
	})
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/integration-activity",
		url.Values{"id": {row.ID}}.Encode(), cookie)
	body := rr.Body.String()

	// Both tab buttons must be present.
	if !strings.Contains(body, "Settings") {
		t.Errorf("activity tab missing Settings tab button")
	}
	if !strings.Contains(body, "Activity") {
		t.Errorf("activity tab missing Activity tab button")
	}
}

// TestIntegrations_RouteAuth verifies all new integrations routes redirect to
// login when called without a session. handler_test.go chi.Walk covers the
// full route table — this test is additional coverage for the specific paths.
func TestIntegrations_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/console/integrations"},
		{http.MethodGet, "/console/api/fragments/integrations-grid"},
		{http.MethodGet, "/console/api/fragments/integration-detail"},
		{http.MethodGet, "/console/api/fragments/integration-activity"},
		{http.MethodGet, "/console/api/fragments/integration-provider-form"},
		{http.MethodPost, "/console/integrations"},
		{http.MethodPost, "/console/integrations/x"},
		{http.MethodPost, "/console/integrations/x/delete"},
		{http.MethodPost, "/console/integrations/x/active"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			var rr *httptest.ResponseRecorder
			if rt.method == http.MethodGet {
				rr = tc.get(t, rt.path, nil)
			} else {
				rr = tc.postForm(t, rt.path, url.Values{}, nil)
			}
			if rr.Code != http.StatusSeeOther {
				t.Errorf("%s %s: status = %d, want 303", rt.method, rt.path, rr.Code)
			}
			if got := rr.Header().Get("Location"); got != "/console/login" {
				t.Errorf("%s %s: Location = %q, want /console/login", rt.method, rt.path, got)
			}
		})
	}
}

// ─── regression: connect-form CSRF (#bug-1) ──────────────────────────────────

// TestFragment_IntegrationProviderForm_CSRFPopulated guards the connect drawer's
// step-2 credential form against the regression where its hidden csrf_token
// rendered empty (value=""), so every create POST 403'd and no integration was
// created. The fragment must plumb the per-session CSRF token into the input.
func TestFragment_IntegrationProviderForm_CSRFPopulated(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/integration-provider-form",
		url.Values{"provider": {"smtp"}}.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("provider form fragment: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// The form must NOT carry an empty csrf_token.
	if strings.Contains(body, `name="csrf_token" value=""`) {
		t.Fatal("connect-form csrf_token is empty — create POST would 403 (#bug-1)")
	}
	// It must carry the live session token.
	if !strings.Contains(body, `name="csrf_token" value="`+csrf+`"`) {
		t.Errorf("connect-form missing populated csrf_token=%q; body excerpt:\n%s",
			csrf, body[strings.Index(body, "csrf_token"):min(len(body), strings.Index(body, "csrf_token")+80)])
	}
}

// ─── regression: provider pick via signal (#bug-2) ───────────────────────────

// TestFragment_IntegrationProviderForm_PickViaSignal proves the step-2 form
// loads when the chosen provider arrives via the integrationProviderPick signal
// (the ?datastar= envelope) rather than a literal ?provider= query param —
// because Datastar serialises signals to ?datastar= and drops literal params,
// so the old row link silently failed to load the chosen provider.
func TestFragment_IntegrationProviderForm_PickViaSignal(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	v := url.Values{}
	v.Set("datastar", `{"integrationProviderPick":"twilio"}`)
	rr := tc.getWithQuery(t, "/console/api/fragments/integration-provider-form",
		v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("provider form fragment: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// Step-2 loaded for Twilio: its title + a Twilio-specific credential field.
	if !strings.Contains(body, "Connect Twilio SMS") {
		t.Errorf("signal-routed provider pick did not load step-2 for twilio; body:\n%s", body)
	}
	if !strings.Contains(body, `name="account_sid"`) {
		t.Error("signal-routed twilio form missing account_sid credential field")
	}
}

// TestEnvelopeToQuery_IntegrationProviderPick locks the envelope→query mapping
// that mergeFragmentParams relies on to forward the signal as ?provider=.
func TestEnvelopeToQuery_IntegrationProviderPick(t *testing.T) {
	key, val := envelopeToQuery(fragmentEnvelope{IntegrationProviderPick: "slack"}, "integrationProviderPick")
	if key != "provider" || val != "slack" {
		t.Errorf("envelopeToQuery(integrationProviderPick) = (%q,%q), want (provider,slack)", key, val)
	}
}

// ─── regression: detail-drawer auto-fetch (#bug-3) ───────────────────────────

// TestIntegrations_DetailDrawerAutoFetchesWhenPreopened guards the wiring that
// makes a direct GET /console/integrations/{id} (and the verify/test-send 303
// redirects, which reload the page with integrationDetailId seeded) actually
// populate the detail body. Without a data-on-load on #integration-detail the
// drawer stays stuck on the "Select an integration…" placeholder and the
// verify/test-send banners never show.
func TestIntegrations_DetailDrawerAutoFetchesWhenPreopened(t *testing.T) {
	tc := newTestConsole(t)
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-1",
		EncCredentials: []byte("ct"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations/"+in.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations/{id}: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// The persistent #integration-detail element must auto-fetch the detail
	// fragment when integrationDetailId is set.
	idx := strings.Index(body, `id="integration-detail"`)
	if idx < 0 {
		t.Fatal("page missing #integration-detail element")
	}
	// Find the opening tag for that element.
	start := strings.LastIndex(body[:idx], "<div")
	end := strings.Index(body[idx:], ">")
	if start < 0 || end < 0 {
		t.Fatal("could not isolate #integration-detail opening tag")
	}
	tag := body[start : idx+end+1]
	if !strings.Contains(tag, "data-on-load") {
		t.Errorf("#integration-detail has no data-on-load auto-fetch (#bug-3); tag:\n%s", tag)
	}
	if !strings.Contains(tag, "/console/api/fragments/integration-detail") {
		t.Errorf("#integration-detail data-on-load does not fetch the detail fragment; tag:\n%s", tag)
	}
}

// ─── regression: verify/test-send banner surfaces at page level (#bug) ───────

// TestIntegrations_VerifyBannerRendersAtPageLevel proves that a verify 303
// redirect to /console/integrations/{id}?verifyRun=<run> surfaces the result
// banner in the PAGE HTML. The detail fragment @get cannot carry ?verifyRun
// (Datastar serializes signals to ?datastar= and drops literal query params),
// so the banner must render at the page level above #integration-detail.
func TestIntegrations_VerifyBannerRendersAtPageLevel(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.DagnatsConsoleURL = "http://dagnats.local"
	tc.rebuild()
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-1",
		EncCredentials: []byte("ct"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations/"+in.ID+"?verifyRun=run-verify-1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations/{id}?verifyRun: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "http://dagnats.local/console/runs/run-verify-1") {
		t.Errorf("page-level verify banner missing dagnats deep-link; body:\n%s",
			body[:min(len(body), 2000)])
	}
}

// TestIntegrations_VerifyErrorBannerRendersAtPageLevel proves the failure path:
// ?verifyError=<msg> surfaces the error banner in the page HTML.
func TestIntegrations_VerifyErrorBannerRendersAtPageLevel(t *testing.T) {
	tc := newTestConsole(t)
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-1",
		EncCredentials: []byte("ct"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations/"+in.ID+"?verifyError=Verification+failed+to+start", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations/{id}?verifyError: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Verification failed to start") {
		t.Errorf("page-level verify error banner missing message; body:\n%s",
			body[:min(len(body), 2000)])
	}
}

// TestIntegrations_TestSendBannerRendersAtPageLevel proves the test-send 303
// redirect (?testSendOK=1) surfaces the success banner in the page HTML.
func TestIntegrations_TestSendBannerRendersAtPageLevel(t *testing.T) {
	tc := newTestConsole(t)
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-1",
		EncCredentials: []byte("ct"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations/"+in.ID+"?testSendOK=1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations/{id}?testSendOK=1: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test message sent.") {
		t.Errorf("page-level test-send success banner missing; body:\n%s",
			body[:min(len(body), 2000)])
	}
}

// TestIntegrations_TestSendErrorBannerRendersAtPageLevel proves the test-send
// failure path (?testSendError=<msg>) surfaces the error banner in the page HTML.
func TestIntegrations_TestSendErrorBannerRendersAtPageLevel(t *testing.T) {
	tc := newTestConsole(t)
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Identifier:     "smtp-1",
		EncCredentials: []byte("ct"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations/"+in.ID+"?testSendError=SMTP+connection+refused", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations/{id}?testSendError: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "SMTP connection refused") {
		t.Errorf("page-level test-send error banner missing message; body:\n%s",
			body[:min(len(body), 2000)])
	}
}

// ─── regression: Twilio Auth Token masked (#bug-4) ───────────────────────────

// TestFragment_IntegrationProviderForm_TwilioAuthTokenMasked proves the Twilio
// Auth Token (a secret) renders as a password input, not plaintext type=text.
func TestFragment_IntegrationProviderForm_TwilioAuthTokenMasked(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/integration-provider-form",
		url.Values{"provider": {"twilio"}}.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("twilio form: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	idx := strings.Index(body, `name="auth_token"`)
	if idx < 0 {
		t.Fatal("twilio form missing auth_token field")
	}
	start := strings.LastIndex(body[:idx], "<input")
	if start < 0 || !strings.Contains(body[start:idx], `type="password"`) {
		t.Errorf("twilio auth_token is not a password input (renders plaintext) (#bug-4); input:\n%s",
			body[start:idx])
	}
}

// ─── regression: empty-state icon is SVG, not emoji (#bug-5) ──────────────────

// TestIntegrations_EmptyStateIconIsSVG proves the integrations empty-state
// renders its icon as a vendored ui-icon SVG (no-emoji rule) rather than the
// literal 🔌 glyph it shipped with.
func TestIntegrations_EmptyStateIconIsSVG(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	if strings.Contains(body, "🔌") {
		t.Error("integrations empty-state still renders the 🔌 emoji (#bug-5)")
	}
	// The empty-state icon span must carry the unplug ui-icon and an inlined SVG.
	idx := strings.Index(body, `data-kind="unplug"`)
	if idx < 0 {
		t.Fatal("integrations empty-state missing data-kind=\"unplug\" icon span")
	}
	// The span body should be an inlined SVG, not a text glyph.
	tail := body[idx:min(len(body), idx+200)]
	if !strings.Contains(tail, "<svg") {
		t.Errorf("empty-state unplug icon is not an inlined SVG; span:\n%s", tail)
	}
}

// ─── regression: missing Chat and SMS tabs (bug 1) ────────────────────────────

// TestIntegrations_ChannelTabsChatAndSMS is the regression test for bug 1:
// the integration store toolbar was missing Chat and SMS filter tabs even
// though chat (discord/telegram/slack/whatsapp/chat-webhook) and sms
// (twilio/signalwire/sns) integrations exist in the catalog and the grid
// filter logic already handles those channel values.
func TestIntegrations_ChannelTabsChatAndSMS(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// Each tab must set the channel signal to drive the grid filter on click.
	// The signal expression is the authoritative check: it proves the tab is
	// present and wired; the label check below is belt-and-suspenders.
	if !strings.Contains(body, "$integrationChannel='chat'") {
		t.Error("Chat tab must set $integrationChannel='chat' on click (bug 1)")
	}
	if !strings.Contains(body, "$integrationChannel='sms'") {
		t.Error("SMS tab must set $integrationChannel='sms' on click (bug 1)")
	}
	// Verify visible labels appear in the rendered toolbar.
	for _, label := range []string{"Chat", "SMS"} {
		if !strings.Contains(body, label) {
			t.Errorf("integration-channel-tabs missing %q tab label (bug 1)", label)
		}
	}
}

// ─── regression: active-tab reactive state broken (bug 2) ────────────────────

// TestIntegrations_ChannelTabActiveStateUsesColonForm is the regression test
// for bug 2.  data-class-is-active (the attribute-suffix form) is silently
// ignored in Datastar v1.0.2: the parser splits attribute names on the first
// colon to find the plugin, so "class-is-active" is treated as the full
// plugin name and never matched — the class plugin is registered as "class".
// The fix is the colon form: data-class:is-active="expr".
//
// Additionally, data-attr:aria-pressed with a boolean expression (true/false)
// is wrong: the Datastar v1.0.2 attr plugin maps boolean true → setAttribute
// to "" (empty string) and boolean false → removeAttribute, both incorrect
// for WAI-ARIA aria-pressed.  The expression must return the strings "true"
// or "false" so the attr plugin sets the attribute value correctly.
func TestIntegrations_ChannelTabActiveStateUsesColonForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/integrations: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// The broken suffix form must not appear anywhere in the channel tab nav.
	if strings.Contains(body, "data-class-is-active") {
		t.Error("channel filter tabs must not use data-class-is-active " +
			"(suffix form is silently broken in Datastar v1.0.2; bug 2)")
	}
	// The reactive colon form must be present for each tab.
	if !strings.Contains(body, "data-class:is-active") {
		t.Error("channel filter tabs must use data-class:is-active " +
			"(colon form) for reactive .is-active class binding (bug 2)")
	}
	// aria-pressed expression must return string "true"/"false", not a
	// boolean.  The ternary "? 'true' : 'false'" is the canonical form
	// used throughout this build.
	if !strings.Contains(body, "'true' : 'false'") {
		t.Error("aria-pressed expression must return string 'true'/'false', " +
			"not a boolean (Datastar v1.0.2 attr plugin maps bool true → " +
			"empty-string attribute; bug 2)")
	}
}

// ─── Bug 1: primary flag persists on Save / transfer ────────────────────────

// TestIntegrations_Update_PrimaryFlagPersistedOnSave verifies two paths of the
// "Primary provider" checkbox on the Configure form:
//
//  1. Sole-provider path: when no other provider of the same channel exists,
//     saving with primary=on persists Primary=true (no ErrPrimaryConflict).
//
//  2. Transfer path (the ACTUAL bug): when provider A is already primary,
//     saving provider B with primary=on must transfer the primary slot to B
//     (A.Primary=false, B.Primary=true) rather than returning ErrPrimaryConflict
//     and leaving B's flag unchanged.
func TestIntegrations_Update_PrimaryFlagPersistedOnSave(t *testing.T) {
	t.Run("sole provider becomes primary", func(t *testing.T) {
		tc := newTestConsole(t)

		// Seed a single SMTP integration that is NOT primary.
		created, err := tc.deps.Connectivity.CreateSMTP(
			testBackground(), tc.scope, "smtp-solo",
			connectivity.SMTPConfig{Addr: "smtp.example.com:587", Host: "smtp.example.com"},
			false,
		)
		if err != nil {
			t.Fatalf("CreateSMTP: %v", err)
		}
		if created.Primary {
			t.Fatal("precondition: created with primary=false but Primary=true")
		}

		cookie, csrf := tc.login(t)

		rr := tc.postForm(t, "/console/integrations/"+created.ID,
			url.Values{
				"_method":    {"PUT"},
				"identifier": {"smtp-solo"},
				"primary":    {"on"},
				"csrf_token": {csrf},
			},
			cookie)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("update with primary=on: status = %d, want 303; body: %s",
				rr.Code, rr.Body.String())
		}

		rows, err := tc.st.Integrations().List(testBackground(), tc.scope)
		if err != nil || len(rows) == 0 {
			t.Fatalf("list after update: err=%v count=%d", err, len(rows))
		}
		if !rows[0].Primary {
			t.Errorf("primary flag not persisted after Save: Primary=%v, want true", rows[0].Primary)
		}
	})

	t.Run("transfer from existing primary (ErrPrimaryConflict path)", func(t *testing.T) {
		tc := newTestConsole(t)

		// Seed two SMTP integrations: providerA is primary, providerB is not.
		providerA, err := tc.deps.Connectivity.CreateSMTP(
			testBackground(), tc.scope, "smtp-a",
			connectivity.SMTPConfig{Addr: "smtp-a.example.com:587", Host: "smtp-a.example.com"},
			true, // A is primary
		)
		if err != nil {
			t.Fatalf("CreateSMTP A: %v", err)
		}
		providerB, err := tc.deps.Connectivity.CreateSMTP(
			testBackground(), tc.scope, "smtp-b",
			connectivity.SMTPConfig{Addr: "smtp-b.example.com:587", Host: "smtp-b.example.com"},
			false,
		)
		if err != nil {
			t.Fatalf("CreateSMTP B: %v", err)
		}

		cookie, csrf := tc.login(t)

		// Save B with primary=on — must transfer (not 409 / not leave A primary).
		rr := tc.postForm(t, "/console/integrations/"+providerB.ID,
			url.Values{
				"_method":    {"PUT"},
				"identifier": {"smtp-b"},
				"primary":    {"on"},
				"csrf_token": {csrf},
			},
			cookie)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("transfer primary to B: status = %d, want 303; body: %s",
				rr.Code, rr.Body.String())
		}

		rows, err := tc.st.Integrations().List(testBackground(), tc.scope)
		if err != nil {
			t.Fatalf("list after transfer: %v", err)
		}
		byID := make(map[string]bool, len(rows))
		for _, row := range rows {
			byID[row.ID] = row.Primary
		}
		if !byID[providerB.ID] {
			t.Errorf("B.Primary = false after transfer; want true")
		}
		if byID[providerA.ID] {
			t.Errorf("A.Primary = true after transfer to B; want false (only one primary per channel)")
		}
	})
}

// TestIntegrations_Update_PrimaryTransfersToNewProvider verifies that saving
// with primary=on when another provider in the same channel already holds
// primary transfers the primary slot: the new provider becomes primary and
// the previous primary is unset. Only one provider per channel may be primary.
func TestIntegrations_Update_PrimaryTransfersToNewProvider(t *testing.T) {
	tc := newTestConsole(t)

	// Seed two SMTP integrations: r1 is primary, r2 is not.
	if _, err := tc.deps.Connectivity.CreateSMTP(
		testBackground(), tc.scope, "smtp-1",
		connectivity.SMTPConfig{Addr: "smtp.example.com:587", Host: "smtp.example.com"},
		true,
	); err != nil {
		t.Fatalf("CreateSMTP r1: %v", err)
	}
	r2, err := tc.deps.Connectivity.CreateSMTP(
		testBackground(), tc.scope, "smtp-2",
		connectivity.SMTPConfig{Addr: "smtp2.example.com:587", Host: "smtp2.example.com"},
		false,
	)
	if err != nil {
		t.Fatalf("CreateSMTP r2: %v", err)
	}

	cookie, csrf := tc.login(t)

	// Save r2 with primary=on — must transfer, not 409.
	rr := tc.postForm(t, "/console/integrations/"+r2.ID,
		url.Values{
			"_method":    {"PUT"},
			"identifier": {"smtp-2"},
			"primary":    {"on"},
			"csrf_token": {csrf},
		},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("update r2 as primary: status = %d, want 303 (primary transfer); body: %s",
			rr.Code, rr.Body.String())
	}

	rows, err := tc.st.Integrations().List(testBackground(), tc.scope)
	if err != nil {
		t.Fatalf("list after transfer: %v", err)
	}
	var primaries []string
	for _, row := range rows {
		if row.Primary {
			primaries = append(primaries, row.ID)
		}
	}
	if len(primaries) != 1 {
		t.Errorf("want exactly 1 primary per channel; got %d: %v", len(primaries), primaries)
	} else if primaries[0] != r2.ID {
		t.Errorf("want r2 (%s) to be primary after transfer; got %s", r2.ID, primaries[0])
	}
}

// ─── Bug 2: deactivating primary is blocked with explanatory error ─────────

// TestIntegrations_ToggleActive_BlocksDeactivatingPrimary verifies that
// deactivating the current primary integration returns an explanatory error
// (422) rather than silently leaving an inactive provider as the primary
// routing target. Routes that need a primary provider must either transfer
// the primary slot first or accept that no primary exists.
func TestIntegrations_ToggleActive_BlocksDeactivatingPrimary(t *testing.T) {
	tc := newTestConsole(t)

	row := tc.seedIntegration(t, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		Active:         true,
		Primary:        true,
		Identifier:     "smtp-primary",
		EncCredentials: []byte("fake"),
	})
	if !row.Primary {
		t.Fatal("precondition: seeded with Primary=true")
	}

	cookie, csrf := tc.login(t)

	// Attempt to deactivate the primary.
	rr := tc.postForm(t, "/console/integrations/"+row.ID+"/active",
		url.Values{"active": {"false"}, "csrf_token": {csrf}},
		cookie)

	// Must be blocked with an explanatory error, not silently succeed.
	if rr.Code == http.StatusSeeOther {
		// Deactivation succeeded — verify it didn't leave an inactive primary.
		rows, _ := tc.st.Integrations().List(testBackground(), tc.scope)
		for _, r := range rows {
			if r.ID == row.ID && !r.Active && r.Primary {
				t.Fatal("deactivating the primary silently left an inactive primary provider")
			}
		}
		// If deactivation succeeded AND primary was cleared, that's also acceptable;
		// however the spec says block with error.
		t.Fatalf("deactivating primary must be blocked (422); got 303 without error")
	}
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("deactivate primary: status = %d, want 422 with explanatory error", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "primary") {
		t.Errorf("deactivate primary error body must mention 'primary'; got: %s", body)
	}
}

// TestIntegrations_WebPushSubject_SetFromConsole is #358 in the console: the
// Web Push detail shows the sender contact, warns when there is none, saves
// a real one, and refuses localhost with a visible reason.
func TestIntegrations_WebPushSubject_SetFromConsole(t *testing.T) {
	tc := newTestConsole(t) // no public base URL: no default subject
	cookie, csrf := tc.login(t)
	if _, _, err := tc.deps.Connectivity.EnsureWebPushKeys(testBackground(), tc.scope); err != nil {
		t.Fatalf("EnsureWebPushKeys: %v", err)
	}
	in, err := tc.st.Integrations().GetByProviderID(testBackground(), tc.scope, "webpush")
	if err != nil {
		t.Fatalf("webpush row: %v", err)
	}
	detailURL := "/console/api/fragments/integration-detail?id=" + in.ID
	if body := tc.get(t, detailURL, cookie).Body.String(); !strings.Contains(body, "Web Push cannot send until a contact is set") {
		t.Fatalf("detail does not warn about a missing subject:\n%s", body)
	}

	action := "/console/integrations/" + in.ID + "/webpush-subject"
	rr := tc.postForm(t, action, url.Values{"subject": {"mailto:notify@localhost"}, "csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "subjectError=") {
		t.Fatalf("localhost subject: status %d Location %q, want 303 with subjectError", rr.Code, rr.Header().Get("Location"))
	}

	rr = tc.postForm(t, action, url.Values{"subject": {"mailto:ops@example.org"}, "csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther || strings.Contains(rr.Header().Get("Location"), "subjectError") {
		t.Fatalf("valid subject: status %d Location %q", rr.Code, rr.Header().Get("Location"))
	}
	if sub, isDefault, err := tc.deps.Connectivity.WebPushSubject(testBackground(), tc.scope); err != nil || sub != "mailto:ops@example.org" || isDefault {
		t.Fatalf("stored subject = %q default=%v err=%v", sub, isDefault, err)
	}
	if body := tc.get(t, detailURL, cookie).Body.String(); !strings.Contains(body, "mailto:ops@example.org") || strings.Contains(body, "cannot send") {
		t.Fatalf("detail after save:\n%s", body)
	}
}

func TestIntegrations_WebPushSubject_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.postForm(t, "/console/integrations/x/webpush-subject", url.Values{"subject": {"mailto:ops@example.org"}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only subject save: status = %d, want 403", rr.Code)
	}
}
