package console

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// dashboard.go renders GET /console/ — the Wave-1 landing page: KPI count
// cards (deliveries by status off the analytics backend, workflow count
// off authoring.List) plus the 7-day delivery sparkline off the existing
// time-series query. All in-process; zero net-new backend.

// sparkDays is the dashboard sparkline window. Wave-4 analytics depth
// owns selectable ranges; the dashboard stays fixed.
const sparkDays = 7

// dashboardView is what dashboard.html binds. The "Delivery health"
// composition (four rich KPI cards + the deliveries-per-channel chart +
// the recent-activity panel) is assembled from the existing analytics,
// authoring, agents, and notifications services in-process. KPIs is kept
// for the channel-filter fragment (#dashboard-kpis), which the chart's
// channel pills still drive.
type dashboardView struct {
	Header   pageHeader
	EnvName  string
	DateLine string
	ReadOnly bool
	Cards    dashboardCards
	Chart    deliveriesChart
	Activity []activityRow
	KPIs     kpiView
}

// dashboardCards holds the four "Delivery health" KPI cards.
type dashboardCards struct {
	Sent      sentCard
	Workflows workflowsCard
	Agents    agentsCard
	Errors    errorsCard
}

// channelStat is one channel's send count for the "Sent today" breakdown.
type channelStat struct {
	Channel string // in_app | email | push | …
	Count   int
	Class   string // CSS modifier (channel-in_app …) for the colour dot
}

// sentCard: total sends + per-channel breakdown dots. DeltaPct/DeltaUp
// are a labelled stub (no prior-window comparison query exists yet).
type sentCard struct {
	Total    int
	Channels []channelStat
	DeltaPct string // e.g. "12.4%" — stubbed; no prior-window query yet
	DeltaUp  bool
}

// workflowsCard: active vs total workflow counts off authoring.List.
type workflowsCard struct {
	Active   int
	Total    int
	Inactive int
}

// agentsCard: agent count split by runtime (mesh|bridge) off agents.List.
// AllResponsive is a stub (no live agent-health probe wired).
type agentsCard struct {
	Total         int
	Mesh          int
	Bridge        int
	AllResponsive bool
}

// errorsCard: failed-delivery count over the analytics window.
// DeadLettered is a stub (no DLQ count query wired); the deep-link to the
// dagnats DLQ uses DLQURL when DagnatsConsoleURL is set.
type errorsCard struct {
	Count        int
	DeadLettered int
	DLQURL       string
}

// deliveriesChart is the "Deliveries per channel" card: one polyline per
// channel over the 7-day window plus the axis day labels. It reuses the
// same fixed-viewBox SVG convention as the sparkline.
type deliveriesChart struct {
	Width  int
	Height int
	Lines  []chartLine
	Days   []string // x-axis labels (e.g. "Jun 6")
	Empty  bool
}

// chartLine is one channel's polyline in the deliveries chart.
type chartLine struct {
	Channel string
	Class   string // channel-in_app | channel-email | channel-push
	Points  string
}

// dashChannels is the fixed channel order the dashboard renders breakdown
// dots and chart lines for. Channels outside this set still count toward
// the "Sent today" total but do not get their own dot/line.
var dashChannels = []string{"in_app", "email", "push"}

// channelClass maps a channel id to its CSS colour-dot modifier.
func channelClass(ch string) string { return "channel-" + ch }

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
	byChannel, err := c.deps.Analytics.DeliveriesByChannel(ctx, scope, store.AnalyticsFilter{})
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

	series, err := c.deps.Analytics.DeliveriesOverTime(ctx, scope, store.AnalyticsFilter{})
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

// dashRecentLimit is the number of rows the dashboard's "Recent activity"
// panel shows.
const dashRecentLimit = 6

// buildDashboardView assembles the full "Delivery health" composition for
// scope: the four KPI cards, the per-channel chart, and the recent
// activity panel. Each sub-build degrades independently — a card with no
// backing query renders its structure with zeros and a labelled stub
// rather than failing the page. Stubs (no backing query yet): the
// "Sent today" delta %, agent responsiveness, and the dead-lettered count.
func (c *Console) buildDashboardView(ctx context.Context, scope domain.Scope, now time.Time) (dashboardView, error) {
	byChannel, err := c.deps.Analytics.DeliveriesByChannel(ctx, scope, store.AnalyticsFilter{})
	if err != nil {
		return dashboardView{}, fmt.Errorf("deliveries by channel: %w", err)
	}

	// Sent + Errors cards off the by-channel/status matrix.
	sent := sentCard{DeltaPct: "12.4%", DeltaUp: true}
	errCount := 0
	for ch, statuses := range byChannel {
		chTotal := 0
		for status, n := range statuses {
			chTotal += n
			sent.Total += n
			switch status {
			case "failed", "error", "bounced":
				errCount += n
			}
		}
		// Only the named channels get a breakdown dot; others still count.
		for _, named := range dashChannels {
			if ch == named {
				sent.Channels = append(sent.Channels, channelStat{
					Channel: ch, Count: chTotal, Class: channelClass(ch),
				})
			}
		}
	}
	// Stable channel order (in_app, email, push).
	sort.SliceStable(sent.Channels, func(i, j int) bool {
		return channelOrder(sent.Channels[i].Channel) < channelOrder(sent.Channels[j].Channel)
	})

	// Workflows card off authoring.List.
	wfCard := workflowsCard{}
	workflows, err := c.deps.Authoring.List(ctx, scope)
	if err != nil {
		return dashboardView{}, fmt.Errorf("list workflows: %w", err)
	}
	wfCard.Total = len(workflows)
	for _, wf := range workflows {
		if wf.Active {
			wfCard.Active++
		}
	}
	wfCard.Inactive = wfCard.Total - wfCard.Active

	// Agents card off agents.List, split by runtime.
	agCard := agentsCard{AllResponsive: true}
	if c.deps.Agents != nil {
		if agents, agErr := c.deps.Agents.Agents().List(ctx, scope); agErr == nil {
			agCard.Total = len(agents)
			for _, a := range agents {
				switch a.Runtime {
				case "mesh":
					agCard.Mesh++
				case "bridge":
					agCard.Bridge++
				}
			}
		} else {
			c.deps.Logger.Warn("console: dashboard agents", "err", agErr)
		}
	}

	errCard := errorsCard{Count: errCount, DeadLettered: 0}
	if c.deps.DagnatsConsoleURL != "" {
		errCard.DLQURL = c.deps.DagnatsConsoleURL + "/console/dlq"
	}

	// Per-channel chart off the 7-day time series.
	series, err := c.deps.Analytics.DeliveriesOverTime(ctx, scope, store.AnalyticsFilter{})
	if err != nil {
		return dashboardView{}, fmt.Errorf("deliveries over time: %w", err)
	}
	chart := buildDeliveriesChart(series, now)

	// Recent activity panel — reuse the activity feed builder, capped.
	recent, err := c.buildActivityListView(ctx, scope, activityFilters{}, "", dashRecentLimit)
	if err != nil {
		// Activity is best-effort on the dashboard; log and render empty.
		c.deps.Logger.Warn("console: dashboard recent activity", "err", err)
		recent = activityListView{}
	}

	return dashboardView{
		EnvName:  scope.EnvID,
		DateLine: now.Format("Monday, Jan 2"),
		ReadOnly: c.deps.ReadOnly,
		Cards: dashboardCards{
			Sent: sent, Workflows: wfCard, Agents: agCard, Errors: errCard,
		},
		Chart:    chart,
		Activity: recent.Rows,
	}, nil
}

// channelOrder ranks the named channels for stable display ordering.
func channelOrder(ch string) int {
	for i, named := range dashChannels {
		if ch == named {
			return i
		}
	}
	return len(dashChannels)
}

// buildDeliveriesChart folds the per-(day, channel) series into one
// polyline per named channel over the trailing sparkDays window ending at
// now's UTC day. All channels share one y-scale (the global max across the
// window) so the lines are comparable, matching the mockup's overlaid
// multi-line look.
func buildDeliveriesChart(series []domain.DeliveryDayCount, now time.Time) deliveriesChart {
	const width, height, pad = 760, 220, 8
	chart := deliveriesChart{Width: width, Height: height}

	days := make([]string, sparkDays)
	dayKeys := make([]string, sparkDays)
	for i := 0; i < sparkDays; i++ {
		d := now.AddDate(0, 0, -(sparkDays - 1 - i))
		dayKeys[i] = d.Format("2006-01-02")
		days[i] = d.Format("Jan 2")
	}
	chart.Days = days

	// perChannel[ch][dayIndex] = count.
	perChannel := make(map[string][]int)
	for _, ch := range dashChannels {
		perChannel[ch] = make([]int, sparkDays)
	}
	dayIndex := make(map[string]int, sparkDays)
	for i, k := range dayKeys {
		dayIndex[k] = i
	}
	maxCount := 0
	for _, row := range series {
		idx, ok := dayIndex[row.Day]
		if !ok {
			continue
		}
		buckets, ok := perChannel[row.Channel]
		if !ok {
			continue // unnamed channel: not plotted
		}
		buckets[idx] += row.Count
		if buckets[idx] > maxCount {
			maxCount = buckets[idx]
		}
	}
	if maxCount == 0 {
		chart.Empty = true
		return chart
	}

	step := float64(width-2*pad) / float64(sparkDays-1)
	for _, ch := range dashChannels {
		buckets := perChannel[ch]
		pts := make([]string, sparkDays)
		for i, count := range buckets {
			x := float64(pad) + float64(i)*step
			y := float64(height-pad) - float64(count)/float64(maxCount)*float64(height-2*pad)
			pts[i] = fmt.Sprintf("%.1f,%.1f", x, y)
		}
		chart.Lines = append(chart.Lines, chartLine{
			Channel: ch, Class: channelClass(ch), Points: strings.Join(pts, " "),
		})
	}
	return chart
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
	view, err := c.buildDashboardView(r.Context(), scope, time.Now().UTC())
	if err != nil {
		c.deps.Logger.Error("console: build dashboard", "err", err)
		c.serveError(w, r)
		return
	}
	// KPIs powers the channel-filter fragment (#dashboard-kpis) the chart
	// pills still drive; build it best-effort so a fragment-query failure
	// never blocks the page.
	if kpis, kErr := c.buildKPIView(r.Context(), scope, ""); kErr == nil {
		view.KPIs = kpis
	} else {
		c.deps.Logger.Warn("console: dashboard kpis", "err", kErr)
	}
	header := pageHeader{
		Title:    "Dashboard",
		Subtitle: "Delivery health across every channel in this environment.",
	}
	if err := header.validate(); err != nil {
		c.deps.Logger.Error("console: dashboard header", "err", err)
		header = pageHeader{Title: "Dashboard"}
	}
	view.Header = header
	c.renderPage(w, r, http.StatusOK, "dashboard", "Dashboard", view)
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
