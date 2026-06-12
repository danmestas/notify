package console

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// dashboard.go renders GET /console/ — the Wave-1 landing page: KPI count
// cards (deliveries by status off the analytics backend, workflow count
// off authoring.List) plus the 7-day delivery sparkline off the existing
// time-series query. All in-process; zero net-new backend.

// sparkDays is the dashboard sparkline window. Wave-4 analytics depth
// owns selectable ranges; the dashboard stays fixed.
const sparkDays = 7

// dashboardView is what dashboard.html binds.
type dashboardView struct {
	Header pageHeader
	KPIs   kpiView
}

// kpiView is the #dashboard-kpis fragment binding: the status tiles, the
// workflow count, and the sparkline. Channel echoes the active channel
// filter ("" = all) so the fragment is self-describing.
type kpiView struct {
	Channel       string
	Tiles         []tile
	WorkflowCount int
	Spark         sparklineView
}

// sparklineView is the server-rendered 7-day delivery sparkline. Points
// is the SVG polyline points attribute; Empty is true when the window
// holds no deliveries (the template renders an empty-state line instead
// of a flat zero line pretending to be data).
type sparklineView struct {
	Days   []sparkDay
	Points string
	Width  int
	Height int
	Total  int
	Empty  bool
}

// sparkDay is one day bucket of the sparkline (tooltips + tests).
type sparkDay struct {
	Day   string
	Count int
}

// statusTone maps a delivery status to its tile tone. Unknown statuses
// render informational — new engine statuses must not crash or alarm.
func statusTone(status string) tileTone {
	switch status {
	case "sent", "delivered", "read":
		return toneSuccess
	case "failed", "error", "bounced":
		return toneDanger
	case "pending", "queued", "throttled":
		return toneWarning
	}
	return toneInfo
}

// buildKPIView assembles the KPI fragment data for scope, filtered to
// channel when non-empty.
func (c *Console) buildKPIView(ctx context.Context, scope domain.Scope, channel string) (kpiView, error) {
	if ctx == nil {
		panic("buildKPIView: ctx is nil")
	}
	byChannel, err := c.deps.Analytics.DeliveriesByChannel(ctx, scope)
	if err != nil {
		return kpiView{}, fmt.Errorf("deliveries by channel: %w", err)
	}
	byStatus := make(map[string]int)
	total := 0
	for ch, statuses := range byChannel {
		if channel != "" && ch != channel {
			continue
		}
		for status, n := range statuses {
			byStatus[status] += n
			total += n
		}
	}
	tiles := make([]tile, 0, len(byStatus)+1)
	tiles = append(tiles, tile{Label: "deliveries", Count: total, Tone: toneDefault})
	statuses := make([]string, 0, len(byStatus))
	for status := range byStatus {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		tiles = append(tiles, tile{Label: status, Count: byStatus[status], Tone: statusTone(status)})
	}

	workflows, err := c.deps.Authoring.List(ctx, scope)
	if err != nil {
		return kpiView{}, fmt.Errorf("list workflows: %w", err)
	}

	series, err := c.deps.Analytics.DeliveriesOverTime(ctx, scope)
	if err != nil {
		return kpiView{}, fmt.Errorf("deliveries over time: %w", err)
	}

	return kpiView{
		Channel:       channel,
		Tiles:         tiles,
		WorkflowCount: len(workflows),
		Spark:         buildSparkline(series, channel, time.Now().UTC()),
	}, nil
}

// buildSparkline folds the per-(day, channel, status) series into one
// total per day over the trailing sparkDays window ending at now's UTC
// day, zero-filling days without deliveries, and renders the polyline
// points for the fixed-viewBox SVG in the template.
func buildSparkline(series []domain.DeliveryDayCount, channel string, now time.Time) sparklineView {
	if now.IsZero() {
		panic("buildSparkline: now is zero")
	}
	perDay := make(map[string]int, len(series))
	for _, row := range series {
		if channel != "" && row.Channel != channel {
			continue
		}
		perDay[row.Day] += row.Count
	}

	const width, height, pad = 240, 56, 4
	view := sparklineView{Width: width, Height: height}
	maxCount := 0
	for i := sparkDays - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		count := perDay[day]
		view.Days = append(view.Days, sparkDay{Day: day, Count: count})
		view.Total += count
		if count > maxCount {
			maxCount = count
		}
	}
	if view.Total == 0 {
		view.Empty = true
		return view
	}

	step := float64(width-2*pad) / float64(sparkDays-1)
	var points []string
	for i, d := range view.Days {
		x := float64(pad) + float64(i)*step
		// y axis points down; maxCount maps to the top padding line.
		y := float64(height - pad)
		if maxCount > 0 {
			y -= float64(d.Count) / float64(maxCount) * float64(height-2*pad)
		}
		points = append(points, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	view.Points = strings.Join(points, " ")
	return view
}

// serveDashboard renders the full dashboard page.
func (c *Console) serveDashboard(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveDashboard: w is nil")
	}
	if r == nil {
		panic("serveDashboard: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveDashboard: no scope in context (route not behind sessionMiddleware)")
	}
	kpis, err := c.buildKPIView(r.Context(), scope, "")
	if err != nil {
		c.deps.Logger.Error("console: build dashboard", "err", err)
		c.serveError(w, r)
		return
	}
	header := pageHeader{
		Title:    "Dashboard",
		Subtitle: "Delivery health across every channel in this environment.",
	}
	if err := header.validate(); err != nil {
		c.deps.Logger.Error("console: dashboard header", "err", err)
		header = pageHeader{Title: "Dashboard"}
	}
	c.renderPage(w, r, http.StatusOK, "dashboard", "Dashboard", dashboardView{
		Header: header,
		KPIs:   kpis,
	})
}

// serveFragmentDashboardKPIs answers /console/api/fragments/dashboard-kpis
// with the re-rendered #dashboard-kpis card strip as one PatchElements
// SSE event. The kpiChannel Datastar signal (or an explicit ?channel=
// query param, which wins) narrows the cards + sparkline to one channel.
func (c *Console) serveFragmentDashboardKPIs(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentDashboardKPIs: w is nil")
	}
	if r == nil {
		panic("serveFragmentDashboardKPIs: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentDashboardKPIs: no scope in context (route not behind sessionMiddleware)")
	}
	q := mergeFragmentParams(r, "kpiChannel")
	var channel string
	if vs := q["channel"]; len(vs) > 0 {
		channel = vs[0]
	}
	kpis, err := c.buildKPIView(r.Context(), scope, channel)
	if err != nil {
		c.deps.Logger.Error("console: kpi fragment", "err", err)
		http.Error(w, "build dashboard KPIs failed", http.StatusInternalServerError)
		return
	}
	html, err := renderFragment(c.ts.base, "dashboard-kpis", kpis)
	if err != nil {
		c.deps.Logger.Error("console: render kpi fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}
