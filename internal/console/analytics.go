package console

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// analytics.go renders GET /console/analytics — the Wave-4 analytics depth page:
// all four KPI cards live (Messages Delivered, Failed Deliveries, Active Subscribers,
// Avg Messages/Subscriber), a workflow filter pill, plus Delivery Trend, Delivery by
// Channel, Workflow Runs Trend, and Active Subscribers Trend charts.
//
// Backed by: DeliveriesByChannel (CountByChannelStatus), DeliveriesOverTime
// (DeliveriesByDay), CountActiveSubscribers, and AverageMessagesPerSubscriber —
// all now accepting the optional AnalyticsFilter for workflow_id scoping (GAP-AF-05,
// GAP-AF-06).

// analyticsRange is the closed set of time-window tokens the analytics page
// accepts. The token is echoed back through the template so the range-tab
// Datastar state round-trips correctly, and the chart data is visually clipped
// to the requested window in buildAnalyticsChartsView.
type analyticsRange string

const (
	analyticsRange24h analyticsRange = "24h"
	analyticsRange7d  analyticsRange = "7d"
	analyticsRange30d analyticsRange = "30d"
)

// parseAnalyticsRange coerces an arbitrary string into a valid analyticsRange,
// defaulting to 7d for unknown or empty values.
func parseAnalyticsRange(s string) analyticsRange {
	switch analyticsRange(s) {
	case analyticsRange24h:
		return analyticsRange24h
	case analyticsRange30d:
		return analyticsRange30d
	}
	return analyticsRange7d
}

// rangeDays returns the number of calendar days the window covers. 24h maps
// to 1 day so chart bucketing stays consistent.
func (r analyticsRange) days() int {
	switch r {
	case analyticsRange24h:
		return 1
	case analyticsRange30d:
		return 30
	}
	return 7
}

// rangeSince returns the time.Time cutoff for the given range window anchored at now.
// Used by the CountActiveSubscribers / AverageMessagesPerSubscriber queries.
func rangeSince(rang analyticsRange, now time.Time) time.Time {
	return now.AddDate(0, 0, -rang.days())
}

// analyticsView is what analytics.html binds.
type analyticsView struct {
	Header    pageHeader
	KPIs      analyticsKPIView
	Charts    analyticsChartsView
	Workflows []workflowOption // populated for the workflow filter dropdown
}

// workflowOption is one entry in the workflow filter dropdown.
type workflowOption struct {
	ID    string // workflow trigger identifier (slug)
	Name  string
	Label string // display label: "Name (slug)" when both differ, else just Name
}

// analyticsKPIView is the #analytics-kpis fragment binding.
type analyticsKPIView struct {
	Range                    analyticsRange
	WorkflowID               string // echoed back for re-filter on range change
	MessagesDelivered        int
	FailedDeliveries         int
	ActiveSubscribers        int64
	AvgMessagesPerSubscriber float64
}

// analyticsChartsView is the #analytics-charts fragment binding.
type analyticsChartsView struct {
	Range      analyticsRange
	WorkflowID string // echoed back so the fragment knows its own filter state
	// Channels is the sorted list of channels seen in the series — used to
	// render the legend and the toggle controls.
	Channels []string
	// ActiveChannels is the set of toggled-on channels (empty = all channels).
	// The template renders it back into Datastar signals so toggles survive
	// fragment refreshes.
	ActiveChannels map[string]bool
	// TrendChart is the delivery trend area chart (status=sent, per-day).
	TrendChart analyticsLineChart
	// ByChannelChart is the horizontal-bar chart of totals per channel.
	ByChannelChart analyticsBarChart
	// FailedChart is the small failed-by-day line chart.
	FailedChart analyticsLineChart
	// RunsTrendChart is the Workflow Runs Trend chart (total deliveries per day).
	RunsTrendChart analyticsLineChart
	// SubscribersTrendChart approximates Active Subscribers Trend using the sent
	// delivery series (a true per-day distinct-subscriber bucketed aggregation is
	// a separate, future aggregation; this is the best available proxy).
	SubscribersTrendChart analyticsLineChart
}

// analyticsLineChart is an inline-SVG area/line series rendered from
// per-day counts. The approach mirrors buildSparkline in dashboard.go:
// server-rendered polyline, no JS charting library.
type analyticsLineChart struct {
	Days   []chartDay
	Points string // SVG polyline points attribute
	Width  int
	Height int
	Total  int
	MaxDay int // max per-day count (tooltip scale reference)
	Empty  bool
}

// chartDay is one day bucket of a time-series chart.
type chartDay struct {
	Day   string
	Count int
}

// analyticsBarChart is an inline-SVG horizontal bar chart: one bar per
// channel, width proportional to share of total.
type analyticsBarChart struct {
	Bars  []channelBar
	Total int
	Empty bool
}

// channelBar is one row of the by-channel bar chart.
type channelBar struct {
	Channel string
	Count   int
	Pct     int // 0-100 for the bar width; 0 when total is 0
	Tone    tileTone
}

// channelTone maps a channel name to its tile/bar tone so the chart
// palette stays consistent across pages.
func channelTone(ch string) tileTone {
	switch ch {
	case "email":
		return toneInfo
	case "sms":
		return toneWarning
	case "push":
		return toneSuccess
	case "in_app":
		return toneDefault
	}
	return toneInfo
}

// buildAnalyticsKPIView constructs the KPI fragment from DeliveriesByChannel
// plus the new CountActiveSubscribers / AverageMessagesPerSubscriber aggregations.
func (c *Console) buildAnalyticsKPIView(
	r *http.Request, scope domain.Scope, rang analyticsRange, workflowID string,
) (analyticsKPIView, error) {
	if r == nil {
		panic("buildAnalyticsKPIView: r is nil")
	}
	f := store.AnalyticsFilter{WorkflowID: workflowID}
	byChannel, err := c.deps.Analytics.DeliveriesByChannel(r.Context(), scope, f)
	if err != nil {
		return analyticsKPIView{}, fmt.Errorf("deliveries by channel: %w", err)
	}
	var delivered, failed int
	for _, statuses := range byChannel {
		for status, n := range statuses {
			switch status {
			case "sent", "delivered", "read":
				delivered += n
			case "failed", "error", "bounced":
				failed += n
			}
		}
	}

	since := rangeSince(rang, time.Now().UTC())
	activeSubs, err := c.deps.Analytics.CountActiveSubscribers(r.Context(), scope, since)
	if err != nil {
		return analyticsKPIView{}, fmt.Errorf("count active subscribers: %w", err)
	}
	avgMsg, err := c.deps.Analytics.AverageMessagesPerSubscriber(r.Context(), scope, since)
	if err != nil {
		return analyticsKPIView{}, fmt.Errorf("avg messages per subscriber: %w", err)
	}

	return analyticsKPIView{
		Range:                    rang,
		WorkflowID:               workflowID,
		MessagesDelivered:        delivered,
		FailedDeliveries:         failed,
		ActiveSubscribers:        activeSubs,
		AvgMessagesPerSubscriber: avgMsg,
	}, nil
}

// buildAnalyticsChartsView constructs the charts fragment from DeliveriesOverTime.
// The range tab clips how many trailing days of the series are presented.
func (c *Console) buildAnalyticsChartsView(
	r *http.Request, scope domain.Scope,
	rang analyticsRange, activeChannels map[string]bool, workflowID string,
) (analyticsChartsView, error) {
	if r == nil {
		panic("buildAnalyticsChartsView: r is nil")
	}
	f := store.AnalyticsFilter{WorkflowID: workflowID}
	series, err := c.deps.Analytics.DeliveriesOverTime(r.Context(), scope, f)
	if err != nil {
		return analyticsChartsView{}, fmt.Errorf("deliveries over time: %w", err)
	}
	v := buildAnalyticsCharts(series, rang, activeChannels, time.Now().UTC())
	v.WorkflowID = workflowID
	return v, nil
}

// buildAnalyticsCharts separates construction from HTTP I/O for pure
// testability (mirrors buildSparkline in dashboard.go).
func buildAnalyticsCharts(
	series []domain.DeliveryDayCount,
	rang analyticsRange,
	activeChannels map[string]bool,
	now time.Time,
) analyticsChartsView {
	if now.IsZero() {
		panic("buildAnalyticsCharts: now is zero")
	}
	days := rang.days()

	// Enumerate all channels present in the series for the legend.
	channelSet := make(map[string]struct{})
	for _, row := range series {
		channelSet[row.Channel] = struct{}{}
	}
	channels := make([]string, 0, len(channelSet))
	for ch := range channelSet {
		channels = append(channels, ch)
	}
	sort.Strings(channels)

	// Defense-in-depth: whitelist the requested channel toggles against the
	// channels the backend actually returned.
	activeChannels = whitelistChannels(activeChannels, channelSet)

	// Build the day index for the requested window.
	dayLabels := make([]string, days)
	for i := range dayLabels {
		dayLabels[i] = now.AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")
	}
	daySet := make(map[string]struct{}, days)
	for _, d := range dayLabels {
		daySet[d] = struct{}{}
	}

	// Accumulate per-day sent/failed and per-channel totals, honouring
	// the channel toggle (empty activeChannels = all channels).
	sentPerDay := make(map[string]int, days)
	failedPerDay := make(map[string]int, days)
	totalPerDay := make(map[string]int, days) // all statuses, for Runs Trend
	channelTotals := make(map[string]int, len(channels))

	for _, row := range series {
		if _, inWindow := daySet[row.Day]; !inWindow {
			continue
		}
		// Runs Trend counts all rows regardless of channel toggle.
		totalPerDay[row.Day] += row.Count
		if len(activeChannels) > 0 && !activeChannels[row.Channel] {
			continue
		}
		switch row.Status {
		case "sent", "delivered", "read":
			sentPerDay[row.Day] += row.Count
			channelTotals[row.Channel] += row.Count
		case "failed", "error", "bounced":
			failedPerDay[row.Day] += row.Count
		}
	}

	trend := buildLineChart(dayLabels, sentPerDay)
	failed := buildLineChart(dayLabels, failedPerDay)
	bar := buildBarChart(channels, channelTotals)
	runs := buildLineChart(dayLabels, totalPerDay)
	// Active Subscribers Trend: proxy with sent-message-volume-per-day (best
	// available without a bucketed distinct-subscriber aggregation).
	subsTrend := buildLineChart(dayLabels, sentPerDay)

	return analyticsChartsView{
		Range:                 rang,
		Channels:              channels,
		ActiveChannels:        activeChannels,
		TrendChart:            trend,
		ByChannelChart:        bar,
		FailedChart:           failed,
		RunsTrendChart:        runs,
		SubscribersTrendChart: subsTrend,
	}
}

// buildLineChart renders a polyline from per-day count buckets over a
// fixed set of ordered day labels (already scoped to the range window).
func buildLineChart(dayLabels []string, perDay map[string]int) analyticsLineChart {
	const width, height, pad = 480, 80, 6
	view := analyticsLineChart{Width: width, Height: height}
	maxCount := 0
	for _, d := range dayLabels {
		count := perDay[d]
		view.Days = append(view.Days, chartDay{Day: d, Count: count})
		view.Total += count
		if count > maxCount {
			maxCount = count
		}
	}
	view.MaxDay = maxCount
	if view.Total == 0 {
		view.Empty = true
		return view
	}
	n := len(dayLabels)
	var step float64
	if n > 1 {
		step = float64(width-2*pad) / float64(n-1)
	}
	var points []string
	for i, d := range view.Days {
		x := float64(pad) + float64(i)*step
		y := float64(height - pad)
		if maxCount > 0 {
			y -= float64(d.Count) / float64(maxCount) * float64(height-2*pad)
		}
		points = append(points, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	view.Points = strings.Join(points, " ")
	return view
}

// buildBarChart builds the horizontal bar chart from per-channel totals.
func buildBarChart(channels []string, channelTotals map[string]int) analyticsBarChart {
	var total int
	for _, n := range channelTotals {
		total += n
	}
	if total == 0 {
		return analyticsBarChart{Empty: true}
	}
	bars := make([]channelBar, 0, len(channels))
	for _, ch := range channels {
		n := channelTotals[ch]
		if n == 0 {
			continue
		}
		pct := int(float64(n) / float64(total) * 100)
		bars = append(bars, channelBar{
			Channel: ch,
			Count:   n,
			Pct:     pct,
			Tone:    channelTone(ch),
		})
	}
	// Sort bars descending by count for readability.
	sort.Slice(bars, func(i, j int) bool {
		return bars[i].Count > bars[j].Count
	})
	return analyticsBarChart{Bars: bars, Total: total}
}

// parseActiveChannels reads the analyticsChannels signal, which the
// template stores as a comma-separated list of toggled-on channels.
// Empty string or missing signal means "all channels".
func parseActiveChannels(raw string) map[string]bool {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out[p] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// whitelistChannels drops any active-channel token not present in known.
func whitelistChannels(active map[string]bool, known map[string]struct{}) map[string]bool {
	if len(active) == 0 {
		return nil
	}
	out := make(map[string]bool, len(active))
	for ch := range active {
		if _, ok := known[ch]; ok {
			out[ch] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildWorkflowOptions builds the sorted list of workflow options for the
// analytics page workflow filter dropdown.
func (c *Console) buildWorkflowOptions(r *http.Request, scope domain.Scope) ([]workflowOption, error) {
	wfs, err := c.deps.Authoring.List(r.Context(), scope)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	opts := make([]workflowOption, 0, len(wfs))
	for _, wf := range wfs {
		label := wf.Name
		if wf.Name != wf.TriggerIdentifier {
			label = wf.Name + " (" + wf.TriggerIdentifier + ")"
		}
		opts = append(opts, workflowOption{
			ID:    wf.TriggerIdentifier,
			Name:  wf.Name,
			Label: label,
		})
	}
	sort.Slice(opts, func(i, j int) bool {
		return opts[i].Label < opts[j].Label
	})
	return opts, nil
}

// servePageAnalytics renders the full analytics page.
func (c *Console) servePageAnalytics(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("servePageAnalytics: w is nil")
	}
	if r == nil {
		panic("servePageAnalytics: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("servePageAnalytics: no scope in context (route not behind sessionMiddleware)")
	}
	rang := parseAnalyticsRange(r.URL.Query().Get("range"))
	workflowID := r.URL.Query().Get("workflow_id")

	kpis, err := c.buildAnalyticsKPIView(r, scope, rang, workflowID)
	if err != nil {
		c.deps.Logger.Error("console: analytics kpis", "err", err)
		c.serveError(w, r)
		return
	}
	charts, err := c.buildAnalyticsChartsView(r, scope, rang, nil, workflowID)
	if err != nil {
		c.deps.Logger.Error("console: analytics charts", "err", err)
		c.serveError(w, r)
		return
	}
	wfOpts, err := c.buildWorkflowOptions(r, scope)
	if err != nil {
		c.deps.Logger.Error("console: analytics workflow options", "err", err)
		// Non-fatal: render page without filter dropdown rather than 500.
		wfOpts = nil
	}
	header := pageHeader{
		Title:    "Analytics",
		Subtitle: "Delivery health over time.",
	}
	if err := header.validate(); err != nil {
		c.deps.Logger.Error("console: analytics header", "err", err)
		header = pageHeader{Title: "Analytics"}
	}
	c.renderPage(w, r, http.StatusOK, "analytics", "Analytics", analyticsView{
		Header:    header,
		KPIs:      kpis,
		Charts:    charts,
		Workflows: wfOpts,
	})
}

// serveFragmentAnalyticsKPIs answers GET /console/api/fragments/analytics-kpis.
// The analyticsRange signal (or explicit ?range= param, which wins) selects
// the time window; the KPI counts come from the existing DeliveriesByChannel
// aggregation plus the new CountActiveSubscribers / AverageMessagesPerSubscriber
// (GAP-AF-05). The optional workflow_id param filters all aggregations (GAP-AF-06).
func (c *Console) serveFragmentAnalyticsKPIs(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentAnalyticsKPIs: w is nil")
	}
	if r == nil {
		panic("serveFragmentAnalyticsKPIs: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentAnalyticsKPIs: no scope in context (route not behind sessionMiddleware)")
	}
	q := mergeFragmentParams(r, "analyticsRange", "analyticsWorkflow")
	rang := parseAnalyticsRange("")
	if vs := q["range"]; len(vs) > 0 {
		rang = parseAnalyticsRange(vs[0])
	}
	workflowID := ""
	if vs := q["workflow_id"]; len(vs) > 0 {
		workflowID = vs[0]
	}
	kpis, err := c.buildAnalyticsKPIView(r, scope, rang, workflowID)
	if err != nil {
		c.deps.Logger.Error("console: analytics kpis fragment", "err", err)
		http.Error(w, "build analytics KPIs failed", http.StatusInternalServerError)
		return
	}
	html, err := renderFragment(c.ts.base, "analytics-kpis", kpis)
	if err != nil {
		c.deps.Logger.Error("console: render analytics kpis fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// serveFragmentAnalyticsCharts answers GET /console/api/fragments/analytics-charts.
// The analyticsRange signal selects the time window; the analyticsChannels
// signal (comma-separated) restricts which channels are plotted. The optional
// workflow_id param filters the underlying series (GAP-AF-06).
func (c *Console) serveFragmentAnalyticsCharts(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentAnalyticsCharts: w is nil")
	}
	if r == nil {
		panic("serveFragmentAnalyticsCharts: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentAnalyticsCharts: no scope in context (route not behind sessionMiddleware)")
	}
	q := mergeFragmentParams(r, "analyticsRange", "analyticsChannels", "analyticsWorkflow")
	rang := parseAnalyticsRange("")
	if vs := q["range"]; len(vs) > 0 {
		rang = parseAnalyticsRange(vs[0])
	}
	var activeChannels map[string]bool
	if vs := q["channels"]; len(vs) > 0 {
		activeChannels = parseActiveChannels(vs[0])
	}
	workflowID := ""
	if vs := q["workflow_id"]; len(vs) > 0 {
		workflowID = vs[0]
	}
	charts, err := c.buildAnalyticsChartsView(r, scope, rang, activeChannels, workflowID)
	if err != nil {
		c.deps.Logger.Error("console: analytics charts fragment", "err", err)
		http.Error(w, "build analytics charts failed", http.StatusInternalServerError)
		return
	}
	html, err := renderFragment(c.ts.base, "analytics-charts", charts)
	if err != nil {
		c.deps.Logger.Error("console: render analytics charts fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}
