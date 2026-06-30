package httpaction_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
)

// seed builds a single-step [http_request h1] workflow with the given control
// values, plus its subscriber and a notification for run r1, and returns the
// handler + store. The control values carry the test server URL, method, etc.
func seed(t *testing.T, control map[string]any, payload json.RawMessage) (*httpaction.Handler, *store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	cv, err := json.Marshal(control)
	if err != nil {
		t.Fatalf("marshal control: %v", err)
	}
	steps, err := json.Marshal([]domain.Step{
		{StepID: "h1", Type: "http_request", ControlValues: cv},
	})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Call",
		TriggerIdentifier: "call",
		Active:            true,
		Steps:             steps,
	})
	if err != nil {
		t.Fatalf("Workflows.Create: %v", err)
	}
	if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "user_1", FirstName: "Ada"}); err != nil {
		t.Fatalf("Subscribers.Upsert: %v", err)
	}
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}
	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID:           "ntf_1",
		WorkflowID:   wf.ID,
		SubscriberID: "user_1",
		RunID:        "r1",
		Payload:      payload,
	}); err != nil {
		t.Fatalf("Notifications.Create: %v", err)
	}
	// NewWithClient with a bare http.Client bypasses the SSRF guard, allowing
	// these tests to reach httptest servers on loopback. The SSRF guard is
	// exercised separately in ssrfguard_test.go.
	return httpaction.NewWithClient(s, &http.Client{}), s, ctx
}

// TestHTTPAction_ExecuteGET_StoresOutput proves the happy path: a GET to a 200
// server captures statusCode/body and persists them to step_outputs.
func TestHTTPAction_ExecuteGET_StoresOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	h, s, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 30, "failurePolicy": "fail_closed",
	}, nil)

	out, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var resp struct {
		StatusCode int    `json:"statusCode"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("statusCode = %d, want 200", resp.StatusCode)
	}
	if resp.Body != "ok" {
		t.Errorf("body = %q, want ok", resp.Body)
	}

	outputs, err := s.StepOutputs().ListByRun(ctx, "r1")
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if got, _ := outputs["h1"]["statusCode"].(float64); got != 200 {
		t.Errorf("persisted statusCode = %v, want 200", outputs["h1"]["statusCode"])
	}
}

// TestHTTPAction_URLInterpolation proves {{payload.*}} is resolved in the URL
// before the call: the server must receive the interpolated path, not the
// literal template.
func TestHTTPAction_URLInterpolation(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer srv.Close()

	h, _, ctx := seed(t, map[string]any{
		"url": srv.URL + "/{{payload.id}}", "method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 30, "failurePolicy": "fail_closed",
	}, json.RawMessage(`{"id":"42"}`))

	if _, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gotPath != "/42" {
		t.Errorf("server path = %q, want /42 (url template not interpolated)", gotPath)
	}
}

// TestHTTPAction_HeaderInterpolation proves {{payload.*}} is resolved inside the
// headers JSON before the call.
func TestHTTPAction_HeaderInterpolation(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	h, _, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "GET",
		"headers": `{"Authorization":"Bearer {{payload.token}}"}`, "body": "",
		"timeoutSec": 30, "failurePolicy": "fail_closed",
	}, json.RawMessage(`{"token":"xyz"}`))

	if _, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization = %q, want %q (header template not interpolated)", gotAuth, "Bearer xyz")
	}
}

// TestHTTPAction_Non2xx_CapturedNotFailed proves a non-2xx RESPONSE is a success
// (captured), not a failure: the run proceeds and the status is persisted so a
// downstream filter can branch on it.
func TestHTTPAction_Non2xx_CapturedNotFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("denied"))
	}))
	defer srv.Close()

	h, _, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 30, "failurePolicy": "fail_closed",
	}, nil)

	out, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"})
	if err != nil {
		t.Fatalf("Handle returned error for a 403 response (should be captured): %v", err)
	}
	var resp struct {
		StatusCode int `json:"statusCode"`
	}
	_ = json.Unmarshal(out, &resp)
	if resp.StatusCode != 403 {
		t.Errorf("statusCode = %d, want 403 captured", resp.StatusCode)
	}
}

// TestHTTPAction_FailClosed_OnTimeout proves fail_closed: a request that exceeds
// timeoutSec returns a NonRetryableError and persists NO output row.
func TestHTTPAction_FailClosed_OnTimeout(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	h, s, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 1, "failurePolicy": "fail_closed",
	}, nil)

	_, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"})
	if err == nil {
		t.Fatal("Handle: expected error on timeout under fail_closed, got nil")
	}
	var nonRetry *engine.NonRetryableError
	if !errors.As(err, &nonRetry) {
		t.Errorf("error = %v, want *engine.NonRetryableError", err)
	}
	outputs, lerr := s.StepOutputs().ListByRun(ctx, "r1")
	if lerr != nil {
		t.Fatalf("ListByRun: %v", lerr)
	}
	if _, ok := outputs["h1"]; ok {
		t.Error("fail_closed wrote a step_output row, want none")
	}
}

// TestHTTPAction_Continue_OnTimeout proves continue: a timeout saves a zero-value
// output and returns nil error so downstream steps proceed.
func TestHTTPAction_Continue_OnTimeout(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	h, s, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 1, "failurePolicy": "continue",
	}, nil)

	out, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"})
	if err != nil {
		t.Fatalf("Handle under continue: want nil error, got %v", err)
	}
	var resp struct {
		StatusCode int    `json:"statusCode"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if resp.StatusCode != 0 || resp.Body != "" {
		t.Errorf("zero output = {statusCode:%d, body:%q}, want {0, \"\"}", resp.StatusCode, resp.Body)
	}
	outputs, lerr := s.StepOutputs().ListByRun(ctx, "r1")
	if lerr != nil {
		t.Fatalf("ListByRun: %v", lerr)
	}
	if got, _ := outputs["h1"]["statusCode"].(float64); got != 0 {
		t.Errorf("persisted statusCode = %v, want 0", outputs["h1"]["statusCode"])
	}
}
