// integration_health_test.go proves the delivery-health surfaces (#101): the
// badge and last error on the integrations page, the drawer's health block,
// and the environment-wide banner while an integration is failing.
package console

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// withHealth wires the health repo onto the harness.
func (tc *testConsole) withHealth() {
	tc.deps.IntegrationHealth = tc.st.IntegrationHealth()
	tc.rebuild()
}

func (tc *testConsole) putHealth(t *testing.T, h domain.IntegrationHealth) {
	t.Helper()
	if err := tc.st.IntegrationHealth().Put(context.Background(), tc.scope, h); err != nil {
		t.Fatalf("put health: %v", err)
	}
}

func seedSMTP(tc *testConsole, t *testing.T, identifier string, active bool) domain.Integration {
	t.Helper()
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: active,
		Identifier: identifier, EncCredentials: []byte("fake-ciphertext"),
	})
	if !active {
		if err := tc.st.Integrations().SetActive(context.Background(), tc.scope, in.ID, false); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
	}
	return in
}

func TestIntegrationHealth_BadgesAndLastErrorOnPage(t *testing.T) {
	tc := newTestConsole(t)
	tc.withHealth()
	bad := seedSMTP(tc, t, "smtp-bad", true)
	good := seedSMTP(tc, t, "smtp-good", true)
	seedSMTP(tc, t, "smtp-new", true) // no health row: unverified
	tc.putHealth(t, domain.IntegrationHealth{
		IntegrationID: bad.ID, State: "failing", Since: "2026-11-07T06:00:00Z",
		LastErrorClass: "auth", LastErrorMessage: "smtp: credentials rejected (status 535)",
	})
	tc.putHealth(t, domain.IntegrationHealth{IntegrationID: good.ID, State: "healthy", Since: "2026-11-07T05:00:00Z"})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/integrations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`data-health-state="failing"`, `data-health-state="healthy"`, `data-health-state="unverified"`,
		"auth: smtp: credentials rejected (status 535)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("integrations page missing %q", want)
		}
	}
	if strings.Contains(body, "fake-ciphertext") {
		t.Error("page leaks credentials")
	}
}

func TestIntegrationHealth_DrawerShowsStateAndError(t *testing.T) {
	tc := newTestConsole(t)
	tc.withHealth()
	in := seedSMTP(tc, t, "smtp-drawer", true)
	tc.putHealth(t, domain.IntegrationHealth{
		IntegrationID: in.ID, State: "degraded", Since: "2026-11-07T06:00:00Z",
		LastErrorClass: "rate_limit", LastErrorMessage: "429 slow down", LastCheckAt: "2026-11-07T06:30:00Z",
	})
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/integration-detail", url.Values{"id": {in.ID}}.Encode(), cookie)
	body := rr.Body.String()
	for _, want := range []string{`id="integration-health"`, `data-health-state="degraded"`, "rate_limit", "429 slow down", "2026-11-07T06:30:00Z"} {
		if !strings.Contains(body, want) {
			t.Errorf("drawer missing %q", want)
		}
	}
}

func TestIntegrationHealth_BannerOnEveryPageWhileFailing(t *testing.T) {
	tc := newTestConsole(t)
	tc.withHealth()
	in := seedSMTP(tc, t, "smtp-down", true)
	tc.putHealth(t, domain.IntegrationHealth{
		IntegrationID: in.ID, State: "failing", LastErrorClass: "auth", LastErrorMessage: "bad password",
	})
	cookie, _ := tc.login(t)

	for _, path := range []string{"/console/", "/console/integrations", "/console/subscribers"} {
		body := tc.get(t, path, cookie).Body.String()
		if !strings.Contains(body, `id="integration-health-banner"`) {
			t.Errorf("%s: no health banner", path)
			continue
		}
		if !strings.Contains(body, "smtp-down") || !strings.Contains(body, `href="/console/integrations/`+in.ID+`"`) {
			t.Errorf("%s: banner does not name and link the integration", path)
		}
	}
}

func TestIntegrationHealth_NoBannerUnlessFailingAndActive(t *testing.T) {
	tc := newTestConsole(t)
	tc.withHealth()
	deg := seedSMTP(tc, t, "smtp-degraded", true)
	off := seedSMTP(tc, t, "smtp-off", false)
	tc.putHealth(t, domain.IntegrationHealth{IntegrationID: deg.ID, State: "degraded"})
	tc.putHealth(t, domain.IntegrationHealth{IntegrationID: off.ID, State: "failing"})
	cookie, _ := tc.login(t)

	if body := tc.get(t, "/console/integrations", cookie).Body.String(); strings.Contains(body, "integration-health-banner") {
		t.Error("banner shown for a degraded integration and a deactivated failing one")
	}
}

func TestIntegrationHealth_BannerCountsOthers(t *testing.T) {
	tc := newTestConsole(t)
	tc.withHealth()
	a := seedSMTP(tc, t, "smtp-a", true)
	b := seedSMTP(tc, t, "smtp-b", true)
	for _, in := range []domain.Integration{a, b} {
		tc.putHealth(t, domain.IntegrationHealth{IntegrationID: in.ID, State: "failing"})
	}
	cookie, _ := tc.login(t)
	if body := tc.get(t, "/console/", cookie).Body.String(); !strings.Contains(body, "1 more integration is failing") {
		t.Error("banner does not count the other failing integration")
	}
}

func TestIntegrationHealth_BannerIsScoped(t *testing.T) {
	tc := newTestConsole(t)
	tc.withHealth()
	other := tc.altScope(t)
	oin, err := tc.st.Integrations().Create(context.Background(), other, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true, Identifier: "elsewhere", EncCredentials: []byte("x"),
	})
	if err != nil {
		t.Fatalf("seed other env: %v", err)
	}
	if err := tc.st.IntegrationHealth().Put(context.Background(), other, domain.IntegrationHealth{IntegrationID: oin.ID, State: "failing"}); err != nil {
		t.Fatalf("put other health: %v", err)
	}
	cookie, _ := tc.login(t)
	body := tc.get(t, "/console/integrations", cookie).Body.String()
	if strings.Contains(body, "integration-health-banner") || strings.Contains(body, "elsewhere") {
		t.Error("another environment's failing integration leaked into this console")
	}
}

func TestIntegrationHealth_ReadOnlyStillShowsBanner(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.ReadOnly = true
	tc.withHealth()
	in := seedSMTP(tc, t, "smtp-ro", true)
	tc.putHealth(t, domain.IntegrationHealth{IntegrationID: in.ID, State: "failing"})
	cookie, _ := tc.login(t)

	body := tc.get(t, "/console/integrations", cookie).Body.String()
	if !strings.Contains(body, "integration-health-banner") || !strings.Contains(body, "Read-only mode") {
		t.Error("read-only console should show both the read-only notice and the health banner")
	}
}

func TestIntegrationHealth_NoHealthRepoRendersNothing(t *testing.T) {
	tc := newTestConsole(t) // IntegrationHealth not wired
	seedSMTP(tc, t, "smtp-plain", true)
	cookie, _ := tc.login(t)

	body := tc.get(t, "/console/integrations", cookie).Body.String()
	if strings.Contains(body, "data-health-state") || strings.Contains(body, "integration-health-banner") {
		t.Error("health markup rendered without a health repo")
	}
}
