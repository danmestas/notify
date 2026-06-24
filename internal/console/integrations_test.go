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
		if !strings.Contains(body, "provider="+p.ID) {
			t.Errorf("provider picker missing select link for id=%s", p.ID)
		}
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
