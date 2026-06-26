// analytics_test.go proves the Wave-4 analytics depth page and its two fragment
// endpoints render live counts off a seeded store — full login + session +
// Datastar-signal stack. Wave-4 adds: live Active Subscribers + Avg Messages
// KPI cards (GAP-AF-05), workflow filter (GAP-AF-06), and two new charts.
//
// Test surface:
//
//	TestAnalytics_RendersSeededKPIsAndCharts       — full page seeded render, 7d default
//	TestAnalytics_LiveActiveSubscribersCard        — W4 Active Subscribers card is live (not "—")
//	TestAnalytics_LiveAvgMessagesCard              — W4 Avg Messages card is live (not "—")
//	TestAnalytics_WorkflowFilterRendersDropdown    — workflow filter dropdown present
//	TestAnalytics_WorkflowFilter_IsolatesKPIs     — workflow_id scopes KPI fragment
//	TestAnalytics_WorkflowFilter_IsolatesCharts   — workflow_id scopes charts fragment
//	TestAnalytics_RunsTrendChart                   — Workflow Runs Trend chart present
//	TestAnalytics_SubscribersTrendChart            — Active Subscribers Trend chart present
//	TestAnalytics_EmptyStoreRendersZeroState       — empty store empty-states
//	TestAnalytics_RangeTab24h                      — 24h range param echoed in fragment
//	TestAnalytics_RangeTab30d                      — 30d range param echoed in fragment
//	TestAnalytics_NavEntryIsLive                   — nav Analytics link is present
//	TestFragment_AnalyticsKPIs_SSEFraming          — fragment returns text/event-stream
//	TestFragment_AnalyticsKPIs_SeededCounts        — live cards show seeded totals
//	TestFragment_AnalyticsKPIs_RangeSignal         — analyticsRange signal applied
//	TestFragment_AnalyticsCharts_SSEFraming        — charts fragment SSE framing
//	TestFragment_AnalyticsCharts_ChannelToggle     — analyticsChannels signal filters
//	TestAnalytics_RouteAuth                        — analytics routes behind session gate
//	TestAnalytics_BuildLineChart                   — pure unit: line chart math
//	TestAnalytics_BuildBarChart                    — pure unit: bar chart math
//	TestAnalytics_ParseActiveChannels              — pure unit: channel list parse
//	TestAnalytics_ParseRange                       — pure unit: range parse/default
//	TestAnalytics_ScopeIsolation                   — cross-tenant isolation
package console

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// --- Page render tests -------------------------------------------------------

func TestAnalytics_RendersSeededKPIsAndCharts(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent"},
		{Channel: "email", Status: "sent"},
		{Channel: "in_app", Status: "sent"},
		{Channel: "email", Status: "failed"},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/analytics", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/analytics: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// KPI cards: 3 delivered, 1 failed.
	for _, want := range []string{
		`<span class="analytics-kpi-value">3</span>`,
		`<span class="analytics-kpi-label">Messages Delivered</span>`,
		`<span class="analytics-kpi-value">1</span>`,
		`<span class="analytics-kpi-label">Failed Deliveries</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("analytics page missing %s", want)
		}
	}

	// Wave-4 live cards are present (no stub "—" for Active Subscribers or Avg).
	for _, want := range []string{
		"Active Subscribers",
		"Avg Messages / Subscriber",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("analytics page missing W4 card label %q", want)
		}
	}
	// Wave-4 cards must NOT render the stub "—" any longer.
	stubCount := strings.Count(body, `<span class="analytics-kpi-value">—</span>`)
	if stubCount != 0 {
		t.Errorf("Wave-4: expected 0 '—' stub cards, got %d", stubCount)
	}

	// Range tabs present.
	for _, tab := range []string{"24h", "7d", "30d"} {
		if !strings.Contains(body, tab) {
			t.Errorf("analytics page missing range tab %q", tab)
		}
	}

	// Chart containers present.
	for _, want := range []string{
		`id="analytics-kpis"`,
		`id="analytics-charts"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("analytics page missing %s", want)
		}
	}
}

// TestAnalytics_LiveActiveSubscribersCard proves the Active Subscribers KPI card
// renders a live number (not "—") when there are subscribers in the window (GAP-AF-05).
func TestAnalytics_LiveActiveSubscribersCard(t *testing.T) {
	tc := newTestConsole(t)
	// Seed with explicit SubscriberIDs so CountActiveSubscribers returns > 0.
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", SubscriberID: "sub-1"},
		{Channel: "email", Status: "sent", SubscriberID: "sub-2"},
		{Channel: "in_app", Status: "sent", SubscriberID: "sub-1"},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/analytics", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/analytics: %d", rr.Code)
	}
	body := rr.Body.String()

	// The "—" stub must not appear at all (all four cards are live).
	if strings.Contains(body, `<span class="analytics-kpi-value">—</span>`) {
		t.Errorf("Active Subscribers card still showing stub '—': %s", body[:300])
	}
	// The label must be present.
	if !strings.Contains(body, "Active Subscribers") {
		t.Errorf("Active Subscribers label missing")
	}
}

// TestAnalytics_LiveAvgMessagesCard proves the Avg Messages/Subscriber KPI card
// renders a live float (not "—") when there is data (GAP-AF-05).
func TestAnalytics_LiveAvgMessagesCard(t *testing.T) {
	tc := newTestConsole(t)
	// sub-1 gets 2 deliveries, sub-2 gets 1 → avg = 1.5.
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", SubscriberID: "sub-1"},
		{Channel: "in_app", Status: "sent", SubscriberID: "sub-1"},
		{Channel: "email", Status: "sent", SubscriberID: "sub-2"},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/analytics", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/analytics: %d", rr.Code)
	}
	body := rr.Body.String()

	if strings.Contains(body, `<span class="analytics-kpi-value">—</span>`) {
		t.Errorf("Avg Messages/Subscriber card still showing stub '—'")
	}
	if !strings.Contains(body, "Avg Messages / Subscriber") {
		t.Errorf("Avg Messages / Subscriber label missing")
	}
	// "1.5" must appear somewhere in the KPI cards section.
	if !strings.Contains(body, "1.5") {
		t.Errorf("expected avg 1.5 to appear in the analytics page; body: %.400s", body)
	}
}

// TestAnalytics_WorkflowFilterRendersDropdown verifies that the workflow filter
// select element appears when there are workflows, and is absent when there are none (GAP-AF-06).
func TestAnalytics_WorkflowFilterRendersDropdown(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	// Without any workflows: no dropdown.
	rr := tc.get(t, "/console/analytics", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/analytics: %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, `analytics-workflow-select`) {
		t.Errorf("workflow filter dropdown rendered with no workflows")
	}

	// Seed a workflow — dropdown must appear.
	tc.seedWorkflow(t, "Welcome Flow", "welcome")
	rr = tc.get(t, "/console/analytics", cookie)
	body = rr.Body.String()
	if !strings.Contains(body, `analytics-workflow-select`) {
		t.Errorf("workflow filter dropdown missing after seeding a workflow")
	}
	if !strings.Contains(body, "Welcome Flow") {
		t.Errorf("workflow option label 'Welcome Flow' missing from dropdown")
	}
}

// TestAnalytics_WorkflowFilter_IsolatesKPIs proves that the workflow_id query
// param scopes the KPI fragment to only that workflow's rows (GAP-AF-06).
func TestAnalytics_WorkflowFilter_IsolatesKPIs(t *testing.T) {
	tc := newTestConsole(t)
	// 3 rows for wf-a, 2 rows for wf-b.
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", WorkflowID: "wf-a"},
		{Channel: "email", Status: "sent", WorkflowID: "wf-a"},
		{Channel: "email", Status: "sent", WorkflowID: "wf-a"},
		{Channel: "email", Status: "sent", WorkflowID: "wf-b"},
		{Channel: "email", Status: "sent", WorkflowID: "wf-b"},
	})
	cookie, _ := tc.login(t)

	// Filter to wf-a: expect delivered=3.
	rr := tc.get(t, "/console/api/fragments/analytics-kpis?workflow_id=wf-a", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-kpis?workflow_id=wf-a: %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `<span class="analytics-kpi-value">3</span>`) {
		t.Errorf("workflow_id=wf-a: expected delivered=3, body: %.300s", body)
	}
	// wf-b's 2 rows must not appear as the Messages Delivered count.
	if strings.Contains(body, `<span class="analytics-kpi-value">5</span>`) {
		t.Errorf("workflow_id=wf-a filter leaked wf-b rows (showed total=5)")
	}
}

// TestAnalytics_WorkflowFilter_IsolatesCharts proves the charts fragment is also
// scoped by workflow_id (GAP-AF-06).
func TestAnalytics_WorkflowFilter_IsolatesCharts(t *testing.T) {
	tc := newTestConsole(t)
	now := time.Now().UTC()
	today := now.Format("2006-01-02") + "T12:00:00Z"
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", WorkflowID: "wf-a", CreatedAt: today},
		{Channel: "email", Status: "sent", WorkflowID: "wf-a", CreatedAt: today},
		{Channel: "sms", Status: "sent", WorkflowID: "wf-b", CreatedAt: today},
	})
	cookie, _ := tc.login(t)

	// Charts for wf-a should have email but NOT sms.
	rr := tc.get(t, "/console/api/fragments/analytics-charts?workflow_id=wf-a", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-charts?workflow_id=wf-a: %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "email") {
		t.Errorf("wf-a chart missing email channel")
	}
	if strings.Contains(body, "3 total") {
		t.Errorf("workflow_id=wf-a chart leaked wf-b (showed 3 total)")
	}
}

// TestAnalytics_RunsTrendChart verifies the Workflow Runs Trend chart section
// is present in the charts fragment (Wave 4 addition).
func TestAnalytics_RunsTrendChart(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/analytics-charts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-charts: %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Workflow Runs Trend") {
		t.Errorf("Workflow Runs Trend chart section missing from analytics-charts fragment")
	}
}

// TestAnalytics_SubscribersTrendChart verifies the Active Subscribers Trend chart
// section is present in the charts fragment (Wave 4 addition).
func TestAnalytics_SubscribersTrendChart(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/analytics-charts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-charts: %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Active Subscribers Trend") {
		t.Errorf("Active Subscribers Trend chart section missing from analytics-charts fragment")
	}
}

func TestAnalytics_EmptyStoreRendersZeroState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/analytics", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/analytics: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// Zero delivered and failed.
	if !strings.Contains(body, `<span class="analytics-kpi-value">0</span>`) {
		t.Errorf("empty analytics missing zero KPI values")
	}

	// Empty-state messages for the charts.
	if !strings.Contains(body, "No deliveries in this period.") {
		t.Errorf("empty analytics missing delivery trend empty state")
	}
	if !strings.Contains(body, "No failures in this period.") {
		t.Errorf("empty analytics missing failed chart empty state")
	}
}

func TestAnalytics_RangeTab24h(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/analytics?range=24h", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/analytics?range=24h: status = %d", rr.Code)
	}
	body := rr.Body.String()
	// The active tab carries is-active.
	if !strings.Contains(body, `analytics-range-tab is-active">24h`) {
		t.Errorf("24h range tab not active in page body")
	}
	// KPI fragment echoes the range.
	if !strings.Contains(body, `data-range="24h"`) {
		t.Errorf("analytics page missing data-range=24h")
	}
}

func TestAnalytics_RangeTab30d(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/analytics?range=30d", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/analytics?range=30d: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `analytics-range-tab is-active">30d`) {
		t.Errorf("30d range tab not active in page body")
	}
}

func TestAnalytics_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/analytics"`) {
		t.Errorf("nav missing the Analytics link on the dashboard")
	}
}

// --- Fragment tests -----------------------------------------------------------

func TestFragment_AnalyticsKPIs_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/analytics-kpis", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("analytics-kpis fragment missing SSE event marker")
	}
	if !strings.Contains(body, `id="analytics-kpis"`) {
		t.Errorf("analytics-kpis fragment missing the stable patch target id")
	}
}

func TestFragment_AnalyticsKPIs_SeededCounts(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent"},
		{Channel: "email", Status: "sent"},
		{Channel: "in_app", Status: "failed"},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/analytics-kpis", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-kpis fragment: %d", rr.Code)
	}
	body := rr.Body.String()
	// 2 sent → MessagesDelivered
	if !strings.Contains(body, `<span class="analytics-kpi-value">2</span>`) {
		t.Errorf("analytics-kpis fragment missing delivered count 2")
	}
	// 1 failed → FailedDeliveries
	if !strings.Contains(body, `<span class="analytics-kpi-value">1</span>`) {
		t.Errorf("analytics-kpis fragment missing failed count 1")
	}
	// No stubs.
	if strings.Contains(body, `<span class="analytics-kpi-value">—</span>`) {
		t.Errorf("analytics-kpis fragment has unexpected stub card '—'")
	}
}

func TestFragment_AnalyticsKPIs_RangeSignal(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	v := url.Values{}
	v.Set("datastar", `{"analyticsRange":"30d"}`)
	rr := tc.get(t, "/console/api/fragments/analytics-kpis?"+v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-kpis with range signal: %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-range="30d"`) {
		t.Errorf("analytics-kpis fragment did not echo the range signal")
	}
}

func TestFragment_AnalyticsCharts_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/analytics-charts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-charts fragment: %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("analytics-charts fragment missing SSE event marker")
	}
	if !strings.Contains(body, `id="analytics-charts"`) {
		t.Errorf("analytics-charts fragment missing the stable patch target id")
	}
}

func TestFragment_AnalyticsCharts_ChannelToggle(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent"},
		{Channel: "email", Status: "sent"},
		{Channel: "in_app", Status: "sent"},
	})
	cookie, _ := tc.login(t)

	// Request with analyticsChannels=email — in_app bar should not appear.
	v := url.Values{}
	v.Set("datastar", `{"analyticsChannels":"email"}`)
	rr := tc.get(t, "/console/api/fragments/analytics-charts?"+v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-charts channel filter: %d", rr.Code)
	}
	body := rr.Body.String()
	// email must appear in the bar chart.
	if !strings.Contains(body, "email") {
		t.Errorf("analytics-charts filtered fragment missing email channel")
	}
	// Verify the fragment echoes the active channel via the fragment-level
	// presence of an is-active toggle button for email.
	if !strings.Contains(body, `is-active`) {
		t.Errorf("analytics-charts filtered fragment missing is-active channel toggle")
	}
}

// TestFragment_AnalyticsCharts_ChannelToggleQuoteSafe proves the channel-toggle
// control is immune to JS-literal breakout. The toggle now embeds the channel
// name as a jsEscape'd JS literal in the data-on:click expression (not via
// $el.dataset.channel, which Datastar does not resolve reliably at runtime).
//
// html/template HTML-encodes the single quotes in template.JS values when they
// appear inside an HTML attribute, so 'ev\'il' becomes &#39;ev\x27il&#39; in
// the raw source. The browser decodes &#39; back to ' before Datastar parses
// the expression, so the runtime JS sees 'ev\x27il' (a well-formed string
// literal).
func TestFragment_AnalyticsCharts_ChannelToggleQuoteSafe(t *testing.T) {
	tc := newTestConsole(t)
	const evil = "ev'il"
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: evil, Status: "sent"},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/analytics-charts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-charts fragment: %d", rr.Code)
	}
	body := rr.Body.String()

	// The click expression must NOT use $el.dataset.channel (unreliable in Datastar).
	if strings.Contains(body, "$el.dataset.channel") {
		t.Errorf("channel-toggle must not use $el.dataset.channel (Datastar does not resolve it reliably)")
	}
	// A raw unescaped 'ev'il' must never appear — that would break out of the JS literal.
	if strings.Contains(body, `=== 'ev'il'`) || strings.Contains(body, `'ev'il'`) {
		t.Errorf("channel name broke out of a JS string literal: %.400s", body)
	}
	// jsEscapeStringLiteral("ev'il") → 'ev\'il'; html/template then HTML-encodes
	// both outer quotes and the escaped inner quote in the attribute value,
	// yielding &#39;ev\&#39;il&#39;. The browser decodes &#39; → ' before JS
	// evaluation, so the runtime expression sees 'ev\'il' — a valid JS literal.
	if !strings.Contains(body, `&#39;ev\&#39;il&#39;`) {
		t.Errorf("channel name not JS-escaped in click expression (want &#39;ev\\&#39;il&#39;): %.400s", body)
	}
}

// TestFragment_AnalyticsCharts_ChannelToggleExpressionLiteral is a regression
// guard for the channel-toggle bug: the data-on:click expression must embed
// the channel as a literal JS string value (not via $el.dataset) so that
// Datastar actually updates $analyticsChannels at runtime before the @get.
//
// html/template HTML-encodes single quotes in template.JS attribute values:
// 'in_app' renders as &#39;in_app&#39; in the raw HTML source. The browser
// decodes this correctly before Datastar evaluates the expression.
func TestFragment_AnalyticsCharts_ChannelToggleExpressionLiteral(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/analytics-charts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-charts fragment: %d", rr.Code)
	}
	body := rr.Body.String()

	// The click handler must assign $analyticsChannels to the literal channel
	// value embedded in the expression. html/template encodes 'in_app' as
	// &#39;in_app&#39; in the attribute value.
	if !strings.Contains(body, `&#39;in_app&#39;`) {
		t.Errorf("channel toggle click expression missing encoded literal &#39;in_app&#39;: %.400s", body)
	}
	// $el.dataset.channel must not be used.
	if strings.Contains(body, "$el.dataset.channel") {
		t.Errorf("channel toggle must not use $el.dataset.channel")
	}
}

// --- Route auth smoke --------------------------------------------------------

func TestAnalytics_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)
	for _, path := range []string{
		"/console/analytics",
		"/console/api/fragments/analytics-kpis",
		"/console/api/fragments/analytics-charts",
	} {
		rr := tc.get(t, path, nil)
		if rr.Code != http.StatusSeeOther {
			t.Errorf("GET %s unauthenticated: status = %d, want 303", path, rr.Code)
		}
		if got := rr.Header().Get("Location"); got != "/console/login" {
			t.Errorf("GET %s: Location = %q, want /console/login", path, got)
		}
	}
}

// --- Pure unit tests ---------------------------------------------------------

func TestAnalytics_BuildLineChart(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	days := 7
	dayLabels := make([]string, days)
	for i := range dayLabels {
		dayLabels[i] = now.AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")
	}
	perDay := map[string]int{
		now.AddDate(0, 0, -6).Format("2006-01-02"): 5,
		now.AddDate(0, 0, -3).Format("2006-01-02"): 10,
		now.Format("2006-01-02"):                   3,
	}
	chart := buildLineChart(dayLabels, perDay)
	if chart.Empty {
		t.Fatalf("buildLineChart returned Empty=true for non-empty data")
	}
	if chart.Total != 18 {
		t.Errorf("Total = %d, want 18", chart.Total)
	}
	if chart.MaxDay != 10 {
		t.Errorf("MaxDay = %d, want 10", chart.MaxDay)
	}
	if len(chart.Days) != days {
		t.Errorf("len(Days) = %d, want %d", len(chart.Days), days)
	}
	if chart.Points == "" {
		t.Errorf("Points is empty for non-empty chart")
	}
	// Points must have 7 coordinate pairs.
	pairs := strings.Fields(chart.Points)
	if len(pairs) != days {
		t.Errorf("Points has %d pairs, want %d", len(pairs), days)
	}
}

func TestAnalytics_BuildLineChart_Empty(t *testing.T) {
	dayLabels := []string{"2026-06-10", "2026-06-09"}
	chart := buildLineChart(dayLabels, map[string]int{})
	if !chart.Empty {
		t.Errorf("buildLineChart with zero counts should be Empty")
	}
	if chart.Points != "" {
		t.Errorf("empty chart should have no Points")
	}
}

func TestAnalytics_BuildBarChart(t *testing.T) {
	channels := []string{"email", "in_app", "sms"}
	totals := map[string]int{"email": 40, "in_app": 40, "sms": 20}
	chart := buildBarChart(channels, totals)
	if chart.Empty {
		t.Fatalf("buildBarChart returned Empty for non-empty data")
	}
	if chart.Total != 100 {
		t.Errorf("Total = %d, want 100", chart.Total)
	}
	if len(chart.Bars) != 3 {
		t.Errorf("len(Bars) = %d, want 3", len(chart.Bars))
	}
	for _, b := range chart.Bars {
		if b.Pct < 0 || b.Pct > 100 {
			t.Errorf("bar %s: Pct = %d, out of range [0,100]", b.Channel, b.Pct)
		}
	}
}

func TestAnalytics_BuildBarChart_Empty(t *testing.T) {
	chart := buildBarChart([]string{"email"}, map[string]int{})
	if !chart.Empty {
		t.Errorf("buildBarChart with zero totals should be Empty")
	}
}

func TestAnalytics_ParseActiveChannels(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]bool
	}{
		{"", nil},
		{"email", map[string]bool{"email": true}},
		{"email,in_app", map[string]bool{"email": true, "in_app": true}},
		{"email, in_app ", map[string]bool{"email": true, "in_app": true}},
		{",,,", nil},
	}
	for _, tc := range cases {
		got := parseActiveChannels(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseActiveChannels(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for k := range tc.want {
			if !got[k] {
				t.Errorf("parseActiveChannels(%q): missing key %q", tc.in, k)
			}
		}
	}
}

func TestAnalytics_ParseRange(t *testing.T) {
	cases := []struct {
		in   string
		want analyticsRange
	}{
		{"24h", analyticsRange24h},
		{"7d", analyticsRange7d},
		{"30d", analyticsRange30d},
		{"", analyticsRange7d},
		{"bogus", analyticsRange7d},
	}
	for _, tc := range cases {
		if got := parseAnalyticsRange(tc.in); got != tc.want {
			t.Errorf("parseAnalyticsRange(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAnalytics_BuildCharts_WindowExclusion proves the range window clips
// out-of-window rows from the chart totals.
func TestAnalytics_BuildCharts_WindowExclusion(t *testing.T) {
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -60).Format("2006-01-02")
	recent := now.AddDate(0, 0, -2).Format("2006-01-02")
	series := []domain.DeliveryDayCount{
		{Day: old, Channel: "email", Status: "sent", Count: 999},
		{Day: recent, Channel: "email", Status: "sent", Count: 5},
	}
	charts := buildAnalyticsCharts(series, analyticsRange7d, nil, now)
	if charts.TrendChart.Total != 5 {
		t.Errorf("window did not exclude old data: Total = %d, want 5", charts.TrendChart.Total)
	}
}

// TestAnalytics_ScopeIsolation seeds deliveries under a SECOND org/env scope
// and asserts they never appear in the test scope's analytics KPIs or charts.
func TestAnalytics_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)

	// One delivery for the test scope.
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent"},
	})

	// Many deliveries for a DIFFERENT tenant.
	otherScope := domain.Scope{OrgID: "other-org", EnvID: "other-env"}
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		err := tc.st.Analytics().Insert(ctx, otherScope, domain.AnalyticsDelivery{
			Channel: "email", Status: "sent",
		})
		if err != nil {
			t.Fatalf("seed other-scope delivery: %v", err)
		}
	}

	cookie, _ := tc.login(t)

	// KPI fragment: Messages Delivered must be 1 (this scope), not 51.
	rr := tc.get(t, "/console/api/fragments/analytics-kpis", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-kpis fragment: %d", rr.Code)
	}
	kpiBody := rr.Body.String()
	if !strings.Contains(kpiBody, `<span class="analytics-kpi-value">1</span>`) {
		t.Errorf("analytics-kpis leaked cross-scope data: want delivered=1, body=%.300s", kpiBody)
	}
	if strings.Contains(kpiBody, `<span class="analytics-kpi-value">51</span>`) ||
		strings.Contains(kpiBody, `<span class="analytics-kpi-value">50</span>`) {
		t.Errorf("analytics-kpis rendered another tenant's totals")
	}

	// Charts fragment: by-channel total must be 1, not 51.
	rr = tc.get(t, "/console/api/fragments/analytics-charts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("analytics-charts fragment: %d", rr.Code)
	}
	chartBody := rr.Body.String()
	if !strings.Contains(chartBody, "1 total") {
		t.Errorf("analytics-charts by-channel total not scoped to this tenant: body=%.300s", chartBody)
	}
	if strings.Contains(chartBody, "51 total") || strings.Contains(chartBody, "50 total") {
		t.Errorf("analytics-charts rendered another tenant's totals")
	}
}

func TestAnalytics_BuildCharts_ChannelFilter(t *testing.T) {
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	day := now.Format("2006-01-02")
	series := []domain.DeliveryDayCount{
		{Day: day, Channel: "email", Status: "sent", Count: 10},
		{Day: day, Channel: "in_app", Status: "sent", Count: 20},
	}
	active := map[string]bool{"email": true}
	charts := buildAnalyticsCharts(series, analyticsRange7d, active, now)
	if charts.TrendChart.Total != 10 {
		t.Errorf("channel filter did not work: Total = %d, want 10", charts.TrendChart.Total)
	}
	// Bar chart should only show email.
	for _, b := range charts.ByChannelChart.Bars {
		if b.Channel != "email" {
			t.Errorf("bar chart contains unexpected channel %q after filter", b.Channel)
		}
	}
}

// TestAnalytics_BuildCharts_RunsTrendIncludesAllStatuses verifies that
// RunsTrendChart counts all delivery rows regardless of status or channel filter,
// while TrendChart counts only sent rows through the filter.
func TestAnalytics_BuildCharts_RunsTrendIncludesAllStatuses(t *testing.T) {
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	day := now.Format("2006-01-02")
	series := []domain.DeliveryDayCount{
		{Day: day, Channel: "email", Status: "sent", Count: 3},
		{Day: day, Channel: "email", Status: "failed", Count: 2},
		{Day: day, Channel: "sms", Status: "sent", Count: 4},
	}
	// Filter to email only.
	active := map[string]bool{"email": true}
	charts := buildAnalyticsCharts(series, analyticsRange7d, active, now)
	// TrendChart: only email+sent = 3.
	if charts.TrendChart.Total != 3 {
		t.Errorf("TrendChart.Total = %d, want 3", charts.TrendChart.Total)
	}
	// RunsTrendChart: all rows regardless of filter = 9.
	if charts.RunsTrendChart.Total != 9 {
		t.Errorf("RunsTrendChart.Total = %d, want 9 (all rows)", charts.RunsTrendChart.Total)
	}
}

func TestAnalytics_RangeDays(t *testing.T) {
	cases := []struct {
		r    analyticsRange
		want int
	}{
		{analyticsRange24h, 1},
		{analyticsRange7d, 7},
		{analyticsRange30d, 30},
	}
	for _, tc := range cases {
		if got := tc.r.days(); got != tc.want {
			t.Errorf("%q.days() = %d, want %d", tc.r, got, tc.want)
		}
	}
}

func TestAnalytics_HandlerRoutesRegistered(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	for _, path := range []string{
		"/console/analytics",
		"/console/api/fragments/analytics-kpis",
		"/console/api/fragments/analytics-charts",
	} {
		rr := tc.get(t, path, cookie)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200 (body: %.200s)",
				path, rr.Code, rr.Body.String())
		}
	}
}

// TestAnalytics_RouteCountGrowth verifies the analytics page 200s.
func TestAnalytics_RouteCountGrowth(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/analytics", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("/console/analytics: %d", rr.Code)
	}
}

// TestAnalytics_RangeSince verifies rangeSince produces the correct cutoff.
func TestAnalytics_RangeSince(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		rang analyticsRange
		want time.Time
	}{
		{analyticsRange24h, time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)},
		{analyticsRange7d, time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)},
		{analyticsRange30d, time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got := rangeSince(tc.rang, now)
		if !got.Equal(tc.want) {
			t.Errorf("rangeSince(%q, now) = %v, want %v", tc.rang, got, tc.want)
		}
	}
}

// Prevent unused import warning: fmt is used by seedDeliveries helper indirectly.
var _ = fmt.Sprintf
