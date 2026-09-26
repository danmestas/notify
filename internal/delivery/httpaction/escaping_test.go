package httpaction_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/engine"
)

// rawValue is #364/#365's payload: every character HTML escaping touched,
// plus the JSON-significant ones.
const rawValue = `don't "quote" <tag> & more \ end`

type seen struct {
	mu     sync.Mutex
	query  string
	header string
	body   []byte
}

func capturing(t *testing.T) (*httptest.Server, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.query, s.header, s.body = r.URL.RawQuery, r.Header.Get("X-Note"), b
		s.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, s
}

func payloadWith(t *testing.T, v string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"v": v})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestHTTPAction_JSONBodyKeepsValuesAndStaysValid is #365's body case: a
// value with quotes and a backslash inside a JSON body arrives as itself,
// in valid JSON, instead of as &quot; entities.
func TestHTTPAction_JSONBodyKeepsValuesAndStaysValid(t *testing.T) {
	srv, got := capturing(t)
	h, _, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "POST",
		"headers":    `{"Content-Type":"application/json"}`,
		"body":       `{"text": "{{payload.v}}"}`,
		"timeoutSec": 30, "failurePolicy": "fail_closed",
	}, payloadWith(t, rawValue))
	if _, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(got.body, &decoded); err != nil {
		t.Fatalf("body %q is not valid JSON: %v", got.body, err)
	}
	if decoded.Text != rawValue {
		t.Fatalf("text = %q, want %q", decoded.Text, rawValue)
	}
}

// TestHTTPAction_JSONShapedBodyWithoutContentType: with no Content-Type, a
// body shaped like JSON is treated as JSON.
func TestHTTPAction_JSONShapedBodyWithoutContentType(t *testing.T) {
	srv, got := capturing(t)
	h, _, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "POST", "headers": "{}",
		"body":       ` [ "{{payload.v}}" ]`,
		"timeoutSec": 30, "failurePolicy": "fail_closed",
	}, payloadWith(t, rawValue))
	if _, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var decoded []string
	if err := json.Unmarshal(got.body, &decoded); err != nil || len(decoded) != 1 || decoded[0] != rawValue {
		t.Fatalf("body %q decoded to %q (%v)", got.body, decoded, err)
	}
}

// TestHTTPAction_TextBodyIsVerbatim: a non-JSON body gets values as sent.
func TestHTTPAction_TextBodyIsVerbatim(t *testing.T) {
	srv, got := capturing(t)
	h, _, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "POST",
		"headers":    `{"Content-Type":"text/plain"}`,
		"body":       `note: {{payload.v}}`,
		"timeoutSec": 30, "failurePolicy": "fail_closed",
	}, payloadWith(t, rawValue))
	if _, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if string(got.body) != "note: "+rawValue {
		t.Fatalf("body = %q", got.body)
	}
}

// TestHTTPAction_HeaderValueWithAQuoteIsKept: headers are a JSON object, so
// a quote in a value used to make it unparseable and drop every header.
func TestHTTPAction_HeaderValueWithAQuoteIsKept(t *testing.T) {
	srv, got := capturing(t)
	h, _, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "GET",
		"headers":    `{"X-Note":"{{payload.v}}"}`,
		"body":       "",
		"timeoutSec": 30, "failurePolicy": "fail_closed",
	}, payloadWith(t, rawValue))
	if _, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.header != rawValue {
		t.Fatalf("X-Note = %q, want %q", got.header, rawValue)
	}
}

// TestHTTPAction_HeaderLineBreakFailsTheStep: a value that would put a line
// break in a header is refused rather than sent.
func TestHTTPAction_HeaderLineBreakFailsTheStep(t *testing.T) {
	srv, _ := capturing(t)
	h, _, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "GET",
		"headers":    `{"X-Note":"{{payload.v}}"}`,
		"body":       "",
		"timeoutSec": 30, "failurePolicy": "fail_closed",
	}, payloadWith(t, "a\r\nX-Injected: 1"))
	_, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"})
	if err == nil {
		t.Fatal("a header value with a line break was sent")
	}
	// The step's own check names the header; net/http would also refuse,
	// but with a message that does not say which header or why.
	if !strings.Contains(err.Error(), `header "X-Note" contains a line break`) {
		t.Fatalf("err = %v, want the step's own line-break refusal", err)
	}
}

// TestHTTPAction_WholeURLFromPayloadWorks: a URL taken whole from the
// payload keeps its query string; & is no longer turned into &amp;.
func TestHTTPAction_WholeURLFromPayloadWorks(t *testing.T) {
	srv, got := capturing(t)
	h, _, ctx := seed(t, map[string]any{
		"url": srv.URL + "/in?{{payload.q}}", "method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 30, "failurePolicy": "fail_closed",
	}, json.RawMessage(`{"q":"a=1&b=2"}`))
	if _, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.query != "a=1&b=2" {
		t.Fatalf("query = %q, want a=1&b=2", got.query)
	}
}
