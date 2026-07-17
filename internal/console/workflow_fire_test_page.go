package console

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/trigger"
)

// workflow_fire_test_page.go renders the workflow fire-test page (W2-A5):
//
//   - GET  /console/workflows/{slug}/test                 fire-test page
//   - POST /console/api/workflows/{slug}/test              fire test transaction
//
// Blocked server-side (InactiveBlocked) when the workflow is inactive; the
// mutation handler is gated on Deps.ReadOnly like every other console POST.

// workflowTestView is what workflow_test.html binds (W2-A5).
type workflowTestView struct {
	Header            pageHeader
	TriggerIdentifier string
	Name              string
	Active            bool
	// StepCount is the number of steps; when >1 the #17 banner is shown.
	StepCount int
	// PayloadExample is a pre-filled JSON example derived from the payload
	// schema (or "{}" when absent). Pre-populates the textarea.
	PayloadExample string
	// FormError is an inline error surfaced after a bad POST (e.g. invalid
	// JSON payload).
	FormError string
	// TransactionID is surfaced after a successful test fire (from ?txid=).
	TransactionID string
	// RunID is the dagnats run id minted for the test fire's transaction
	// (empty when not yet available). Surfaced alongside RunURL so the operator
	// sees the durable run reference even when the deep-link base is unset
	// (W4 ADR-013, #55).
	RunID string
	// RunURL is the dagnats deep-link for the transaction's run (empty when
	// DagnatsConsoleURL is unset or the run id is not yet available).
	RunURL string
	// InactiveBlocked is true when the workflow is inactive — the POST is
	// blocked and the form shows an explanatory message.
	InactiveBlocked bool
	ReadOnly        bool
	Signals         template.JS
	CSRF            string
}

// ─── W2-A5: test / trigger page ───────────────────────────────────────────────

// wfTestSignalsJSON returns the initial signals for the test page. The payload
// textarea is plain state managed by the browser; only the CSRF comes from the
// server.
func wfTestSignalsJSON() template.JS {
	b, _ := json.Marshal(map[string]string{"testSubscriberId": "", "testPayload": "{}"})
	return template.JS(b)
}

// serveWorkflowTestPage renders GET /console/workflows/{slug}/test (W2-A5).
func (c *Console) serveWorkflowTestPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveWorkflowTestPage: w is nil")
	}
	if r == nil {
		panic("serveWorkflowTestPage: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveWorkflowTestPage: no scope in context")
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		c.serveNotFound(w, r)
		return
	}
	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: test page get workflow", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	steps, _ := decodeSteps(wf.Steps)

	payloadExample := "{}"
	if ex := payloadExampleFromSchema(wf.PayloadSchema); ex != "" {
		payloadExample = ex
	}

	s, _ := sessionFromContext(r.Context())
	header := pageHeader{
		Title:    wf.Name,
		Subtitle: fmt.Sprintf("Trigger identifier: %s", wf.TriggerIdentifier),
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: wf.Name}
	}

	view := workflowTestView{
		Header:            header,
		TriggerIdentifier: wf.TriggerIdentifier,
		Name:              wf.Name,
		Active:            wf.Active,
		StepCount:         len(steps),
		PayloadExample:    payloadExample,
		FormError:         r.URL.Query().Get("error"),
		TransactionID:     r.URL.Query().Get("txid"),
		InactiveBlocked:   !wf.Active,
		ReadOnly:          c.deps.ReadOnly,
		Signals:           wfTestSignalsJSON(),
		CSRF:              s.CSRF,
	}
	// If we have a txid, resolve the run id + deep-link from the notification
	// store so the operator sees the durable run reference (W4 ADR-013, #55).
	if txid := r.URL.Query().Get("txid"); txid != "" {
		view.RunID = c.runIDFromTransaction(r.Context(), scope, txid)
		view.RunURL = c.runURL(view.RunID)
	}

	c.renderPage(w, r, http.StatusOK, "workflow-test", wf.Name+" — Test", view)
}

// handleWorkflowTest handles POST /console/api/workflows/{slug}/test (W2-A5).
// Validates JSON payload, calls trigger.Ingest in-process, redirects to the
// test page with ?txid= on success or ?error= on failure.
func (c *Console) handleWorkflowTest(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowTest: w is nil")
	}
	if r == nil {
		panic("handleWorkflowTest: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowTest: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	testURL := "/console/workflows/" + slug + "/test"

	// Validate the workflow is active before calling Ingest.
	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: test trigger lookup", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}
	if !wf.Active {
		http.Redirect(w, r, testURL+"?error="+
			url.QueryEscape("Workflow is inactive. Activate it before sending a test trigger."),
			http.StatusSeeOther)
		return
	}

	subscriberID := strings.TrimSpace(r.FormValue("subscriber_id"))
	payloadRaw := strings.TrimSpace(r.FormValue("payload"))
	if subscriberID == "" {
		http.Redirect(w, r, testURL+"?error="+
			url.QueryEscape("Subscriber ID is required."), http.StatusSeeOther)
		return
	}
	if payloadRaw == "" {
		payloadRaw = "{}"
	}
	if !json.Valid([]byte(payloadRaw)) {
		http.Redirect(w, r, testURL+"?error="+
			url.QueryEscape("Payload must be valid JSON."), http.StatusSeeOther)
		return
	}

	if c.deps.Trigger == nil {
		http.Redirect(w, r, testURL+"?error="+
			url.QueryEscape("Trigger service unavailable."), http.StatusSeeOther)
		return
	}

	req := trigger.TriggerRequest{
		Name: slug,
		To: trigger.ToField{
			SubscriberID: subscriberID,
		},
		Payload: json.RawMessage(payloadRaw),
	}
	resp, err := c.deps.Trigger.Ingest(r.Context(), scope, req)
	if err != nil {
		c.deps.Logger.Error("console: test trigger ingest", "slug", slug, "err", err)
		http.Redirect(w, r, testURL+"?error="+
			url.QueryEscape("Trigger failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	if !resp.Acknowledged {
		msg := "Trigger not acknowledged"
		if len(resp.Error) > 0 {
			msg = resp.Error[0]
		}
		http.Redirect(w, r, testURL+"?error="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}

	// Success: redirect to the test page with the transaction id surfaced.
	activityHref := "/console/workflows/" + slug + "/activity"
	_ = activityHref // surfaced in template via TransactionID link
	http.Redirect(w, r, testURL+"?txid="+url.QueryEscape(resp.TransactionID),
		http.StatusSeeOther)
}

// payloadExampleFromSchema derives a simple example JSON object from a
// payload schema. Returns "" when the schema is absent or unparseable.
// This is a best-effort hint for the test form's payload textarea.
func payloadExampleFromSchema(schema json.RawMessage) string {
	if len(schema) == 0 {
		return ""
	}
	var s struct {
		Properties map[string]struct {
			Type    string `json:"type"`
			Default any    `json:"default"`
			Example any    `json:"example"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Properties) == 0 {
		return ""
	}
	out := make(map[string]any, len(s.Properties))
	for k, v := range s.Properties {
		switch {
		case v.Example != nil:
			out[k] = v.Example
		case v.Default != nil:
			out[k] = v.Default
		default:
			switch v.Type {
			case "string":
				out[k] = ""
			case "number", "integer":
				out[k] = 0
			case "boolean":
				out[k] = false
			default:
				out[k] = nil
			}
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
