// Package httpaction implements the http_request ACTION step handler: an
// outbound HTTP call made mid-workflow with payload/subscriber variable
// interpolation in url/headers/body, whose response (statusCode/body/headers) is
// captured into the step_outputs table so downstream channel steps can read it
// as template data ({{steps.<stepId>.statusCode}}) and as filter-condition
// sources.
//
// It lives in its own sub-package (rather than alongside the channel handlers in
// internal/delivery) so the HTTP-execution concern — request building, timeout
// enforcement, failure-policy classification, response persistence — is isolated
// from channel delivery. It reuses delivery.RebuildEnvelope (exported for this
// purpose) to obtain the run's {subscriber, payload} interpolation context from
// the store without a parent-package import cycle.
package httpaction

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
)

// maxBodyBytes caps how much of an HTTP response body is captured into the step
// output, so a hostile or runaway endpoint cannot bloat the step_outputs row.
const maxBodyBytes = 1 << 20 // 1 MiB

// maxHeaderBytes caps the total key+value bytes written into the flattened
// response-header map persisted to step_outputs. The body is separately capped
// at maxBodyBytes (1 MiB); without this cap a hostile endpoint could bloat the
// row with arbitrarily large header fields.
const maxHeaderBytes = 128 << 10 // 128 KiB

// stepTypes resolves a step's declared secret fields for run-time decryption.
var stepTypes = steptype.Default()

// Handler executes http_request action steps. It holds the store (for the run
// context and output persistence) and an HTTP client whose per-request deadline
// is governed by the step's timeoutSec (no global client timeout, so each step
// enforces its own).
type Handler struct {
	store  *store.Store
	client *http.Client
	// box decrypts declared secret control fields (http_request headers) loaded
	// from the store at run time. nil leaves values untouched, so a legacy
	// deployment without encryption still reads plaintext steps.
	box *secretbox.Box
}

// New constructs a Handler backed by s using the SSRF-safe HTTP client.
// The client's dial-time control hook blocks loopback, link-local, RFC1918
// private, unique-local IPv6, unspecified, and multicast addresses at
// post-DNS-resolution time, defeating DNS-rebinding attacks.
func New(s *store.Store, opts ...Option) *Handler {
	h := &Handler{store: s, client: NewSSRFSafeClient()}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Option configures a Handler at construction.
type Option func(*Handler)

// WithSecretBox enables decryption of declared secret control fields at run
// time. Omit it and header values are read verbatim (legacy behaviour).
func WithSecretBox(box *secretbox.Box) Option {
	return func(h *Handler) { h.box = box }
}

// NewWithClient constructs a Handler backed by s using the provided HTTP client.
// Callers are responsible for applying any security policy on client; this
// constructor is primarily for tests that need a bypass (e.g. to reach an
// httptest server on loopback) or for advanced transport customisation.
func NewWithClient(s *store.Store, client *http.Client, opts ...Option) *Handler {
	h := &Handler{store: s, client: client}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// stepConfig is the http_request step's run-time control values, loaded from the
// store. Headers is a JSON-encoded string (e.g. `{"Authorization":"Bearer x"}`),
// consistent with the flat controlMap contract the editor and registry use.
type stepConfig struct {
	URL           string `json:"url"`
	Method        string `json:"method"`
	Headers       string `json:"headers"`
	Body          string `json:"body"`
	TimeoutSec    int    `json:"timeoutSec"`
	FailurePolicy string `json:"failurePolicy"`
}

// response is the captured-output shape written to step_outputs and returned as
// the engine task output. Keys are stable, lowerCamel, and readable by both the
// Handlebars template engine and the filter EvalContext.
type response struct {
	StatusCode int               `json:"statusCode"`
	Body       string            `json:"body"`
	Headers    map[string]string `json:"headers"`
}

// Handle is registered via engine.Handle("notify.http_request", h.Handle).
//
// Failure policy (documented contract):
//   - A COMPLETED HTTP exchange — including a non-2xx response — is a SUCCESS:
//     the status code and body are captured as output and the run proceeds, so a
//     downstream filter can branch on statusCode (e.g. skip when statusCode=403).
//   - A TRANSPORT failure (connection error, DNS, or a timeoutSec deadline)
//     triggers the failure policy:
//   - failurePolicy="fail_closed" (default): return a NonRetryableError so the
//     run transitions to failed and no downstream step runs; NO output row is
//     written.
//   - failurePolicy="continue": save a zero-value output
//     ({statusCode:0,body:"",headers:{}}) and return nil error so downstream
//     steps run with that output available.
func (h *Handler) Handle(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	// {subscriber, payload, scope} for interpolation + tenancy come from the
	// store-rebuilt envelope; the returned Step field (built for the channel
	// handlers) is deliberately ignored here.
	env, _, _, err := delivery.RebuildEnvelope(ctx, h.store, t)
	if err != nil {
		return nil, engine.NewNonRetryableError(fmt.Errorf("http_request: load run context: %w", err))
	}
	scope := domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}

	cfg, err := loadConfig(ctx, h.store, h.box, t.RunID, t.StepID)
	if err != nil {
		return nil, engine.NewNonRetryableError(fmt.Errorf("http_request: load step config: %w", err))
	}

	data := map[string]any{
		"subscriber": decodeObject(env.Subscriber),
		"payload":    decodeObject(env.Payload),
	}

	// Each part escapes values for where they land (#365). The URL takes
	// values as sent: a whole URL from the payload is the common case, and
	// percent-encoding would break it. Headers are a JSON object, so values
	// are JSON-escaped; a quote no longer drops every header. The body is
	// JSON-escaped when it is JSON, and sent as written otherwise.
	noTranslate := func(key string) string { return key }
	rawURL := strings.TrimSpace(template.RenderAs(template.Plain, cfg.URL, data, noTranslate))
	if rawURL == "" {
		return h.onFailure(ctx, scope, t, cfg, fmt.Errorf("interpolated url is empty"))
	}
	headers := parseHeaders(template.RenderAs(template.JSON, cfg.Headers, data, noTranslate))
	for k, v := range headers {
		if strings.ContainsAny(k+v, "\r\n") {
			return h.onFailure(ctx, scope, t, cfg, fmt.Errorf("header %q contains a line break after interpolation", k))
		}
	}
	bodyEsc := template.Plain
	if bodyIsJSON(cfg.Body, headers) {
		bodyEsc = template.JSON
	}
	bodyStr := template.RenderAs(bodyEsc, cfg.Body, data, noTranslate)

	reqCtx, cancel := context.WithTimeout(ctx, steptype.HTTPTimeout(cfg.TimeoutSec))
	defer cancel()

	var bodyReader io.Reader
	if bodyStr != "" {
		bodyReader = strings.NewReader(bodyStr)
	}
	req, err := http.NewRequestWithContext(reqCtx, cfg.Method, rawURL, bodyReader)
	if err != nil {
		return h.onFailure(ctx, scope, t, cfg, fmt.Errorf("build request: %w", err))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		// Transport-level failure (connection, DNS, or timeout deadline).
		return h.onFailure(ctx, scope, t, cfg, fmt.Errorf("do request: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	out, merr := json.Marshal(response{
		StatusCode: resp.StatusCode,
		Body:       string(respBody),
		Headers:    flattenHeaders(resp.Header),
	})
	if merr != nil {
		return nil, engine.NewNonRetryableError(fmt.Errorf("http_request: marshal output: %w", merr))
	}
	if err := h.store.StepOutputs().Save(ctx, scope, t.RunID, t.StepID, out); err != nil {
		return nil, engine.NewNonRetryableError(fmt.Errorf("http_request: save output: %w", err))
	}
	return out, nil
}

// onFailure applies the step's failure policy to a transport-level failure.
func (h *Handler) onFailure(ctx context.Context, scope domain.Scope, t engine.TaskPayload, cfg stepConfig, cause error) (json.RawMessage, error) {
	if cfg.FailurePolicy == "continue" {
		out, _ := json.Marshal(response{StatusCode: 0, Body: "", Headers: map[string]string{}})
		if err := h.store.StepOutputs().Save(ctx, scope, t.RunID, t.StepID, out); err != nil {
			return nil, engine.NewNonRetryableError(fmt.Errorf("http_request: save zero output: %w", err))
		}
		return out, nil
	}
	// fail_closed: fail the run permanently and write NO output row.
	return nil, engine.NewNonRetryableError(fmt.Errorf("http_request: %w", cause))
}

// loadConfig resolves the http_request step's control values by run id + step
// id (notification -> workflow -> step), applying the same defaults the registry
// validates: method GET, timeoutSec 30, failurePolicy fail_closed.
func loadConfig(ctx context.Context, s *store.Store, box *secretbox.Box, runID, stepID string) (stepConfig, error) {
	notif, err := delivery.NotificationByRun(ctx, s, runID)
	if err != nil {
		return stepConfig{}, fmt.Errorf("notification by run %q: %w", runID, err)
	}
	scope := domain.Scope{OrgID: notif.OrgID, EnvID: notif.EnvID}
	wf, err := s.Workflows().GetByID(ctx, scope, notif.WorkflowID)
	if err != nil {
		return stepConfig{}, fmt.Errorf("workflow %q: %w", notif.WorkflowID, err)
	}
	var steps []domain.Step
	if len(wf.Steps) > 0 {
		if err := json.Unmarshal(wf.Steps, &steps); err != nil {
			return stepConfig{}, fmt.Errorf("unmarshal steps: %w", err)
		}
	}
	for i := range steps {
		if steps[i].StepID != stepID {
			continue
		}
		step := steps[i]
		if box != nil {
			// Open declared secret fields (headers) before the config is read;
			// a legacy plaintext value passes through unchanged.
			decrypted, derr := stepTypes.DecryptSecrets(box, step)
			if derr != nil {
				return stepConfig{}, fmt.Errorf("decrypt step secrets: %w", derr)
			}
			step = decrypted
		}
		var cfg stepConfig
		if len(step.ControlValues) > 0 {
			if err := json.Unmarshal(step.ControlValues, &cfg); err != nil {
				return stepConfig{}, fmt.Errorf("unmarshal controlValues: %w", err)
			}
		}
		if cfg.Method == "" {
			cfg.Method = http.MethodGet
		}
		if cfg.TimeoutSec <= 0 {
			cfg.TimeoutSec = 30
		}
		if cfg.FailurePolicy == "" {
			cfg.FailurePolicy = "fail_closed"
		}
		return cfg, nil
	}
	return stepConfig{}, fmt.Errorf("step %q not found in workflow %q", stepID, notif.WorkflowID)
}

// decodeObject unmarshals a raw JSON object into map[string]any for template
// interpolation; a nil/empty/non-object value yields an empty map.
func decodeObject(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

// parseHeaders decodes the interpolated headers JSON string into a string map.
// A blank or malformed value yields no headers rather than an error — a broken
// headers blob must not crash the run.
func parseHeaders(s string) map[string]string {
	out := map[string]string{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// bodyIsJSON reports whether the step's body is JSON: its Content-Type
// header says so, or it sets none and the template is shaped like a JSON
// object or array.
func bodyIsJSON(bodyTmpl string, headers map[string]string) bool {
	for k, v := range headers {
		if strings.EqualFold(k, "Content-Type") {
			return strings.Contains(strings.ToLower(v), "json")
		}
	}
	b := strings.TrimSpace(bodyTmpl)
	return strings.HasPrefix(b, "{") || strings.HasPrefix(b, "[")
}

// flattenHeaders reduces an http.Header to a string map carrying the first value
// per key, capped at maxHeaderBytes total so a hostile endpoint cannot bloat
// the step_outputs row with enormous header fields.
func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	var total int
	for k, v := range h {
		if len(v) == 0 {
			continue
		}
		total += len(k) + len(v[0])
		if total > maxHeaderBytes {
			break
		}
		out[k] = v[0]
	}
	return out
}
