package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"

	"github.com/starfederation/datastar-go/datastar"
)

// fragments.go owns the Datastar fragment conventions (dagnats
// fragments.go port). Fragment endpoints render one named template back
// as a single PatchElements SSE event; the template's outer element
// carries a stable id so the default outer-mode patch lines up. Mutation
// endpoints that don't re-render anything answer with the toast-lift
// JSON shape instead ({"toast": "..."}).

// fragmentEnvelope captures the Datastar-signalled state fragment
// requests carry. Each filter input on a page binds into a matching
// signal; the signal value rides on every triggered request. Later
// slices append their fields here and teach envelopeToQuery the mapping.
type fragmentEnvelope struct {
	// KPIChannel filters the dashboard KPI cards + sparkline to one
	// delivery channel ("" = all channels).
	KPIChannel string `json:"kpiChannel,omitempty"`
	// WFFilter is the workflows-list name/trigger search input.
	WFFilter string `json:"wfFilter,omitempty"`
	// WFStatus is the workflows-list status select: ""|"active"|"inactive".
	WFStatus string `json:"wfStatus,omitempty"`
	// AnalyticsRange is the analytics page time-window token ("24h", "7d",
	// "30d"). Empty defaults to "7d".
	AnalyticsRange string `json:"analyticsRange,omitempty"`
	// AnalyticsChannels is a comma-separated list of toggled-on channel
	// names for the analytics charts. Empty means all channels.
	AnalyticsChannels string `json:"analyticsChannels,omitempty"`
	// Activity feed filter signals (W1-6).
	ActivityRange       string `json:"activityRange,omitempty"`
	ActivityWorkflow    string `json:"activityWorkflow,omitempty"`
	ActivityChannel     string `json:"activityChannel,omitempty"`
	ActivitySubscriber  string `json:"activitySubscriber,omitempty"`
	ActivityTransaction string `json:"activityTransaction,omitempty"`
	ActivityStatus      string `json:"activityStatus,omitempty"`
	// SelectedNotificationId drives the detail panel open/close.
	SelectedNotificationId string `json:"selectedNotificationId,omitempty"`
	// Integration store filter signals (W2).
	IntegrationChannel string `json:"integrationChannel,omitempty"`
	// IntegrationDetailId is the ID of the open detail drawer ("" = closed).
	IntegrationDetailId string `json:"integrationDetailId,omitempty"`
}

// mergeFragmentParams merges the request's URL query with the Datastar
// signal envelope. URL params take precedence over signals so explicit
// links (pagination, drill-throughs) remain authoritative when both are
// set. signalKeys gates which envelope fields apply to this view —
// irrelevant signals stay absent so they can't filter another page.
func mergeFragmentParams(
	r *http.Request, signalKeys ...string,
) map[string][]string {
	if r == nil {
		panic("mergeFragmentParams: r is nil")
	}
	if len(signalKeys) == 0 {
		panic("mergeFragmentParams: signalKeys is empty")
	}
	out := make(map[string][]string, len(r.URL.Query())+len(signalKeys))
	for k, v := range r.URL.Query() {
		out[k] = v
	}
	env, ok := readSignalsBestEffort(r)
	if !ok {
		return out
	}
	for _, key := range signalKeys {
		queryKey, value := envelopeToQuery(env, key)
		if value == "" || queryKey == "" {
			continue
		}
		if _, present := out[queryKey]; present {
			continue
		}
		out[queryKey] = []string{value}
	}
	return out
}

// envelopeToQuery maps an envelope JSON field name to its URL query name
// + value. Centralising the mapping means templates, view builders, and
// fragment endpoints share one vocabulary.
func envelopeToQuery(env fragmentEnvelope, key string) (string, string) {
	switch key {
	case "kpiChannel":
		return "channel", env.KPIChannel
	case "wfFilter":
		return "q", env.WFFilter
	case "wfStatus":
		return "status", env.WFStatus
	case "analyticsRange":
		return "range", env.AnalyticsRange
	case "analyticsChannels":
		return "channels", env.AnalyticsChannels
	// Activity feed filter signals.
	case "activityRange":
		return "range", env.ActivityRange
	case "activityWorkflow":
		return "workflow_id", env.ActivityWorkflow
	case "activityChannel":
		return "channel", env.ActivityChannel
	case "activitySubscriber":
		return "subscriber_id", env.ActivitySubscriber
	case "activityTransaction":
		return "transaction_id", env.ActivityTransaction
	case "activityStatus":
		return "status", env.ActivityStatus
	case "selectedNotificationId":
		return "notification_id", env.SelectedNotificationId
	// Integration store filter signals.
	case "integrationChannel":
		return "channel", env.IntegrationChannel
	case "integrationDetailId":
		return "id", env.IntegrationDetailId
	}
	return "", ""
}

// readSignalsBestEffort tries the Datastar SDK helper first; on any error
// (no body, malformed JSON) returns ok=false so callers fall back to URL
// params only. A missing/invalid body must never 500 the operator — the
// view simply renders unfiltered, the safe default.
func readSignalsBestEffort(r *http.Request) (fragmentEnvelope, bool) {
	if r == nil {
		panic("readSignalsBestEffort: r is nil")
	}
	var env fragmentEnvelope
	if err := datastar.ReadSignals(r, &env); err == nil {
		return env, true
	}
	// Fallback: tests / older clients may POST the JSON body directly
	// without the datastar query-param wrapping. Tolerate it.
	if r.Body == nil {
		return env, false
	}
	const maxBody = 64 * 1024
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil || len(body) == 0 {
		return env, false
	}
	if jsonErr := json.Unmarshal(body, &env); jsonErr != nil {
		return env, false
	}
	return env, true
}

// renderFragment runs one named template against data and returns the
// rendered HTML. The caller streams it via patchHTML or writes it whole.
func renderFragment(
	tmpl *template.Template, name string, data any,
) (string, error) {
	if tmpl == nil {
		panic("renderFragment: tmpl is nil")
	}
	if name == "" {
		panic("renderFragment: name is empty")
	}
	if tmpl.Lookup(name) == nil {
		return "", fmt.Errorf("template %q not registered", name)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("execute template %q: %w", name, err)
	}
	return buf.String(), nil
}

// patchHTML writes one Datastar PatchElements SSE event carrying html.
// The target element id comes from the fragment template itself.
func (c *Console) patchHTML(w http.ResponseWriter, r *http.Request, html string) {
	if w == nil {
		panic("patchHTML: w is nil")
	}
	if r == nil {
		panic("patchHTML: r is nil")
	}
	if html == "" {
		panic("patchHTML: html is empty")
	}
	sse := datastar.NewSSE(w, r)
	if err := sse.PatchElements(html); err != nil {
		c.deps.Logger.Error("console: patch elements", "err", err)
	}
}

// writeToast answers a mutation with the toast-lift JSON shape. The page
// JS (later slice) lifts {"toast": ...} into the toast outlet; until
// then the body is still a self-describing success payload.
func writeToast(w http.ResponseWriter, message string) {
	if w == nil {
		panic("writeToast: w is nil")
	}
	if message == "" {
		panic("writeToast: message is empty")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"toast": message})
}
