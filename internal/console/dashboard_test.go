// dashboard_test.go proves the Wave-1 landing page renders real counts
// off a seeded store: deliveries-by-status tiles, the workflow-count
// tile, the 7-day sparkline, and the nav chrome — all through the full
// login + session stack.
package console

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

func TestDashboard_RendersSeededKPIsAndNav(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
		{Channel: "in_app", Status: "sent"},
		{Channel: "email", Status: "failed"},
	})
	tc.seedWorkflow(t, "Welcome", "welcome")
	tc.seedWorkflow(t, "Digest", "digest")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// KPI tiles: 3 total deliveries, 2 sent, 1 failed, 2 workflows.
	for _, want := range []string{
		`<span class="page-header-tile-count">3</span>`,
		`<span class="page-header-tile-label">deliveries</span>`,
		`<span class="page-header-tile-count">2</span>`,
		`<span class="page-header-tile-label">sent</span>`,
		`<span class="page-header-tile-count">1</span>`,
		`<span class="page-header-tile-label">failed</span>`,
		`<span class="page-header-tile-label">workflows</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %s", want)
		}
	}
	// Sparkline rendered (3 deliveries land in today's bucket).
	if !strings.Contains(body, "dashboard-sparkline") {
		t.Errorf("dashboard missing the sparkline SVG")
	}
	// Nav: Dashboard link active; no dead links to unbuilt pages.
	if !strings.Contains(body, `href="/console/"`) {
		t.Errorf("nav missing the Dashboard link")
	}
	for _, dead := range []string{`href="/console/workflows"`, `href="/console/activity"`, `href="/console/analytics"`} {
		if strings.Contains(body, dead) {
			t.Errorf("nav carries a placeholder link %s before its page exists", dead)
		}
	}
}

func TestDashboard_EmptyStoreRendersZeroState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No deliveries in the last 7 days.") {
		t.Errorf("empty dashboard missing the sparkline empty state")
	}
	if !strings.Contains(body, `<span class="page-header-tile-count">0</span>`) {
		t.Errorf("empty dashboard missing zero tiles")
	}
}

func TestDashboard_NotFoundPageKeepsChrome(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	rr := tc.get(t, "/console/nope", cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /console/nope: status = %d, want 404", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Page not found") {
		t.Errorf("404 page missing title")
	}
	if !strings.Contains(body, `href="/console/"`) {
		t.Errorf("404 page missing the way back to the dashboard")
	}
	// The 404 handler runs behind sessionMiddleware, so the layout's
	// sign-out form must carry the live session's CSRF token — an empty
	// token would 403 the logout POST from this page.
	if !strings.Contains(body, `name="csrf_token" value="`+csrf+`"`) {
		t.Errorf("404 page logout form missing the session CSRF token")
	}
}

func TestDashboard_NotFoundWithRevokedKeyRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	tc.revokeAllKeys(t)
	rr := tc.get(t, "/console/nope", cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("revoked-key 404: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("revoked-key 404: Location = %q, want /console/login", got)
	}
}

func TestBuildSparkline_ZeroFillsAndScales(t *testing.T) {
	now := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	series := []domain.DeliveryDayCount{
		{Day: "2026-06-12", Channel: "in_app", Status: "sent", Count: 4},
		{Day: "2026-06-10", Channel: "email", Status: "failed", Count: 2},
		{Day: "2026-06-01", Channel: "in_app", Status: "sent", Count: 9}, // outside window
	}
	view := buildSparkline(series, "", now)
	if len(view.Days) != sparkDays {
		t.Fatalf("days = %d, want %d", len(view.Days), sparkDays)
	}
	if view.Days[0].Day != "2026-06-06" || view.Days[6].Day != "2026-06-12" {
		t.Errorf("window = %s..%s, want 2026-06-06..2026-06-12", view.Days[0].Day, view.Days[6].Day)
	}
	if view.Total != 6 {
		t.Errorf("total = %d, want 6 (out-of-window row must not count)", view.Total)
	}
	if view.Empty {
		t.Errorf("view marked empty with deliveries present")
	}
	if points := strings.Split(view.Points, " "); len(points) != sparkDays {
		t.Errorf("polyline points = %d, want %d", len(points), sparkDays)
	}
}

func TestBuildSparkline_ChannelFilterAndEmpty(t *testing.T) {
	now := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	series := []domain.DeliveryDayCount{
		{Day: "2026-06-12", Channel: "email", Status: "sent", Count: 3},
	}
	view := buildSparkline(series, "push", now)
	if !view.Empty {
		t.Errorf("push-filtered view not empty despite zero push deliveries")
	}
	if view.Total != 0 {
		t.Errorf("total = %d, want 0", view.Total)
	}
	unfiltered := buildSparkline(series, "email", now)
	if unfiltered.Empty || unfiltered.Total != 3 {
		t.Errorf("email view = (empty=%v total=%d), want (false, 3)", unfiltered.Empty, unfiltered.Total)
	}
}
