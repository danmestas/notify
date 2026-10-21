package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stub is a minimal Streamable HTTP MCP server. sse switches its responses
// from application/json to text/event-stream.
type stub struct {
	sse bool

	mu      sync.Mutex
	headers []http.Header
	methods []string
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.headers = append(s.headers, r.Header.Clone())
	s.methods = append(s.methods, req.Method)
	s.mu.Unlock()
	if req.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "sess-1" {
		http.Error(w, "no session", http.StatusBadRequest)
		return
	}
	if req.ID == nil { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	var rpcErr any
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "sess-1")
		result = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "stub", "version": "1"}}
	case "tools/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Cursor == "" {
			result = map[string]any{"tools": []any{map[string]any{"name": "search", "description": "Search", "inputSchema": map[string]any{"type": "object"}}}, "nextCursor": "page2"}
		} else {
			result = map[string]any{"tools": []any{map[string]any{"name": "get_record", "description": "Get", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}}}}
		}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		switch p.Name {
		case "text":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "line one"}, map[string]any{"type": "image", "data": "xx"}, map[string]any{"type": "text", "text": "line two"}}}
		case "structured":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "dup"}}, "structuredContent": map[string]any{"id": p.Arguments["id"]}}
		case "fail":
			result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "no such record"}}}
		case "slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		case "big":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", MaxResponseBytes)}}}
		default:
			rpcErr = map[string]any{"code": -32602, "message": "unknown tool"}
		}
	}
	msg := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}
	body, _ := json.Marshal(msg)
	if s.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		// A server notification ahead of the response must be skipped.
		_, _ = fmt.Fprintf(w, ": comment\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func connect(t *testing.T, s *stub) *Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	h := http.Header{}
	h.Set("Authorization", "Bearer tok")
	c := New(NewHTTPClient(true), srv.URL, h)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}

func TestClient_JSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			s := &stub{sse: sse}
			c := connect(t, s)
			tools, err := c.ListTools(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(tools) != 2 || tools[0].Name != "search" || tools[1].Name != "get_record" || !strings.Contains(string(tools[1].InputSchema), `"id"`) {
				t.Fatalf("tools = %+v", tools)
			}
			res, st, err := c.CallTool(context.Background(), "text", json.RawMessage(`{"q":"hi"}`))
			if err != nil {
				t.Fatal(err)
			}
			if res.Text != "line one\nline two" || res.Structured != nil || res.IsError {
				t.Fatalf("text result = %+v", res)
			}
			if st.RequestBytes == 0 || st.ResponseBytes == 0 {
				t.Fatalf("stats = %+v", st)
			}
			res, _, err = c.CallTool(context.Background(), "structured", json.RawMessage(`{"id":"r1"}`))
			if err != nil {
				t.Fatal(err)
			}
			if string(res.Structured) != `{"id":"r1"}` || res.Text != "" {
				t.Fatalf("structured result = %+v", res)
			}
			res, _, err = c.CallTool(context.Background(), "fail", nil)
			if err != nil || !res.IsError || res.Text != "no such record" {
				t.Fatalf("isError result = %+v, %v", res, err)
			}

			s.mu.Lock()
			defer s.mu.Unlock()
			want := []string{"initialize", "notifications/initialized", "tools/list", "tools/list", "tools/call", "tools/call", "tools/call"}
			if strings.Join(s.methods, ",") != strings.Join(want, ",") {
				t.Fatalf("methods = %v", s.methods)
			}
			for i, h := range s.headers {
				if h.Get("Authorization") != "Bearer tok" || !strings.Contains(h.Get("Accept"), "text/event-stream") || !strings.Contains(h.Get("Accept"), "application/json") {
					t.Fatalf("request %d headers = %v", i, h)
				}
				if i > 0 && h.Get("Mcp-Protocol-Version") != ProtocolVersion {
					t.Fatalf("request %d has no protocol version header: %v", i, h)
				}
			}
		})
	}
}

func TestClient_TypedErrors(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			c := connect(t, &stub{sse: sse})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, _, err := c.CallTool(ctx, "slow", nil); ClassOf(err) != Timeout {
				t.Fatalf("slow: %v (class %q), want timeout", err, ClassOf(err))
			}
			_, st, err := c.CallTool(context.Background(), "big", nil)
			if ClassOf(err) != TooLarge {
				t.Fatalf("big: %v (class %q), want too_large", err, ClassOf(err))
			}
			if st.ResponseBytes > MaxResponseBytes+1 {
				t.Fatalf("read %d bytes past the %d cap", st.ResponseBytes, MaxResponseBytes)
			}
			if _, _, err := c.CallTool(context.Background(), "nope", nil); ClassOf(err) != Rejected {
				t.Fatalf("unknown tool: %v (class %q), want rejected", err, ClassOf(err))
			}
		})
	}
}

func TestClient_HTTPFailures(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest", http.StatusFound)
	}))
	defer redirect.Close()
	if err := New(NewHTTPClient(true), redirect.URL, nil).Initialize(context.Background()); ClassOf(err) != Rejected {
		t.Fatalf("redirect: %v (class %q), want rejected and not followed", err, ClassOf(err))
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer down.Close()
	if err := New(NewHTTPClient(true), down.URL, nil).Initialize(context.Background()); ClassOf(err) != Unavailable {
		t.Fatalf("502: %v (class %q), want unavailable", err, ClassOf(err))
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	if err := New(NewHTTPClient(true), url, nil).Initialize(context.Background()); ClassOf(err) != Unavailable {
		t.Fatalf("closed: %v (class %q), want unavailable", err, ClassOf(err))
	}
	// Without the development setting, loopback is refused before dialling.
	live := httptest.NewServer(&stub{})
	defer live.Close()
	if err := New(NewHTTPClient(false), live.URL, nil).Initialize(context.Background()); ClassOf(err) != Rejected {
		t.Fatalf("loopback without dev: %v (class %q), want rejected", err, ClassOf(err))
	}
}

func TestCheckURL(t *testing.T) {
	ok := []string{"https://vault.example/mcp", "https://relay.example:8443/a/b"}
	devOnly := []string{"http://127.0.0.1:9000/mcp", "http://localhost/mcp", "http://[::1]:8080/", "https://127.0.0.1/mcp"}
	bad := []string{"", "vault.example/mcp", "ftp://vault.example", "http://vault.example/mcp", "https://10.0.0.5/mcp", "https://192.168.1.1/", "https://169.254.169.254/", "https://[fd00::1]/", "https://user:pw@vault.example/", "https:///nohost", "http://10.0.0.5/"}
	for _, u := range ok {
		if err := CheckURL(u, false); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range devOnly {
		if err := CheckURL(u, true); err != nil {
			t.Errorf("%s with loopback allowed: %v", u, err)
		}
		if err := CheckURL(u, false); err == nil {
			t.Errorf("%s accepted without loopback allowed", u)
		}
	}
	for _, u := range bad {
		if err := CheckURL(u, true); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

func TestClassOf(t *testing.T) {
	if ClassOf(nil) != "" || ClassOf(errors.New("x")) != Unavailable || ClassOf(fmt.Errorf("wrap: %w", &Error{Class: TooLarge})) != TooLarge {
		t.Fatal("ClassOf misclassifies")
	}
}
