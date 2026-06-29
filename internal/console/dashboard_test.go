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

	// Delivery-health composition: the "Sent today" card totals all 3
	// deliveries; the per-channel breakdown shows in_app (2) and email (1);
	// "Active workflows … of 2" reflects the two seeded workflows; the
	// errors card surfaces the one failed delivery.
	for _, want := range []string{
		"Delivery health",
		`class="kpi-card-value">`, // KPI cards present
		`class="kpi-breakdown"`,   // per-channel breakdown
		`class="kpi-dot channel-in_app"`,
		`class="kpi-dot channel-email"`,
		"of 2", // active-workflows denominator
		"Errors · 24h",
		`class="dashboard-chart`, // per-channel chart card
		"Recent activity",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// The chart renders a polyline per named channel (deliveries landed
	// today, so the chart is non-empty).
	if !strings.Contains(body, "dashboard-chart-svg") {
		t.Errorf("dashboard missing the per-channel chart SVG")
	}
	// Sidebar nav: grouped sections + Dashboard active; Workflows,
	// Activity, and Analytics all live. No dead links remain.
	for _, want := range []string{
		"console-sidebar",
		">NOTIFY<", ">CHANNELS<", ">AGENTS<", ">SYSTEM<",
		`href="/console/"`,
		`href="/console/workflows"`,
		`href="/console/activity"`,
		`href="/console/analytics"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard sidebar missing %q", want)
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
	// Empty store: chart renders its empty state, the "Sent today" card
	// shows a zero total, and the recent-activity panel its empty line.
	if !strings.Contains(body, "No deliveries in the last 7 days.") {
		t.Errorf("empty dashboard missing the chart empty state")
	}
	if !strings.Contains(body, `class="kpi-card-value">`) {
		t.Errorf("empty dashboard missing the KPI cards")
	}
	if !strings.Contains(body, "No recent activity in this environment.") {
		t.Errorf("empty dashboard missing the recent-activity empty state")
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

// TestDashboard_SentTodayCountsOnlySentStatus verifies that the "Sent today"
// KPI card total only counts deliveries with status="sent" and does NOT
// include failed, error, or bounced deliveries. A failed delivery must not
// inflate the headline number.
//
// Seed shape: Sent.Total=3, Errors.Count=1 — the two KPI values are distinct
// so an assertion on 3 cannot false-pass by matching the Errors card.
func TestDashboard_SentTodayCountsOnlySentStatus(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
		{Channel: "in_app", Status: "sent"},
		{Channel: "email", Status: "sent"},
		{Channel: "email", Status: "failed"}, // must NOT count toward Sent total (Errors.Count=1)
	})
	cookie, _ := tc.login(t)

	// Always assert on the view struct — the HTML substring check is
	// unreliable when multiple KPI cards share the same numeric value.
	ctx := domain.ContextWithScope(testBackground(), tc.scope)
	view, err := tc.c.buildDashboardView(ctx, tc.scope, time.Now().UTC())
	if err != nil {
		t.Fatalf("buildDashboardView: %v", err)
	}
	if view.Cards.Sent.Total != 3 {
		t.Errorf("Sent.Total = %d, want 3 (failed must not count)", view.Cards.Sent.Total)
	}
	if view.Cards.Errors.Count != 1 {
		t.Errorf("Errors.Count = %d, want 1 (only the failed delivery)", view.Cards.Errors.Count)
	}

	// Also verify the HTTP response loads successfully. The struct assertions
	// above (Sent.Total, Errors.Count) are the authoritative correctness guard;
	// no fragile HTML-substring value check is needed here.
	rr := tc.get(t, "/console/", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/: status = %d, want 200", rr.Code)
	}
}

// TestBuildDashboardView_SentCardExcludesFailures is the pure-unit companion:
// it calls buildDashboardView with a seeded store and asserts exact counts
// without going through HTTP rendering, proving the bug is at the computation
// layer not the template layer.
func TestBuildDashboardView_SentCardExcludesFailures(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
		{Channel: "in_app", Status: "sent"},
		{Channel: "email", Status: "failed"},
		{Channel: "push", Status: "bounced"},
		{Channel: "push", Status: "error"},
	})

	ctx := domain.ContextWithScope(testBackground(), tc.scope)
	view, err := tc.c.buildDashboardView(ctx, tc.scope, time.Now().UTC())
	if err != nil {
		t.Fatalf("buildDashboardView: %v", err)
	}

	// Only the two in_app "sent" rows count toward Sent today.
	if view.Cards.Sent.Total != 2 {
		t.Errorf("Sent.Total = %d, want 2 (failed/bounced/error must not count)", view.Cards.Sent.Total)
	}

	// Per-channel breakdown dots must also reflect only sent counts.
	for _, ch := range view.Cards.Sent.Channels {
		switch ch.Channel {
		case "in_app":
			if ch.Count != 2 {
				t.Errorf("in_app breakdown count = %d, want 2", ch.Count)
			}
		case "email":
			// email had only a "failed" — it should not appear in the breakdown
			// at all (count=0 channels are omitted) or appear with count=0.
			if ch.Count != 0 {
				t.Errorf("email breakdown count = %d, want 0 (only failed deliveries)", ch.Count)
			}
		case "push":
			// push had only bounced+error — same: should not appear or count=0.
			if ch.Count != 0 {
				t.Errorf("push breakdown count = %d, want 0 (only bounced/error)", ch.Count)
			}
		}
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
