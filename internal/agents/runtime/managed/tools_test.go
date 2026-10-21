package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/runtime/managed/mcp"
	"github.com/danmestas/notify/internal/aibuilder"
)

// scriptClient answers model calls from a script and records every request.
type scriptClient struct {
	mu     sync.Mutex
	reqs   []aibuilder.MessageRequest
	script func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse
}

func (s *scriptClient) Message(_ context.Context, req aibuilder.MessageRequest) (aibuilder.MessageResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := req
	cp.Messages = append([]aibuilder.Message(nil), req.Messages...)
	s.reqs = append(s.reqs, cp)
	return s.script(len(s.reqs), req), nil
}

// vault is a stub MCP server (JSON responses) with tools that succeed, fail,
// stall and overflow.
type vault struct {
	// extra lists that many more tools, t000..., each with a description of
	// desc bytes; listDelay stalls tools/list.
	extra     int
	desc      int
	listDelay time.Duration
	more      []string

	mu      sync.Mutex
	calls   []string
	headers []http.Header
}

func (v *vault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	v.mu.Lock()
	v.headers = append(v.headers, r.Header.Clone())
	if req.Method == "tools/call" {
		v.calls = append(v.calls, req.Params.Name)
	}
	v.mu.Unlock()
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "s1")
		result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{}}
	case "tools/list":
		var tools []any
		for _, n := range append([]string{"get_record", "search_vault", "fail", "slow", "big", "wide"}, v.more...) {
			tools = append(tools, map[string]any{"name": n, "description": n, "inputSchema": map[string]any{"type": "object"}})
		}
		for i := 0; i < v.extra; i++ {
			tools = append(tools, map[string]any{"name": fmt.Sprintf("t%03d", i), "description": strings.Repeat("d", v.desc), "inputSchema": map[string]any{"type": "object"}})
		}
		time.Sleep(v.listDelay)
		result = map[string]any{"tools": tools}
	case "tools/call":
		switch req.Params.Name {
		case "get_record":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "BODY-MARKER as text"}}, "structuredContent": map[string]any{"id": req.Params.Arguments["id"], "passage": "BODY-MARKER passage"}}
		case "search_vault":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "BODY-MARKER hit"}}}
		case "fail":
			result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "no grant"}}}
		case "slow":
			<-r.Context().Done()
			return
		case "big":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", mcp.MaxResponseBytes)}}}
		case "wide": // fits on the wire, over the per-call result budget
			var recs []any
			for i := 0; i < 1500; i++ {
				recs = append(recs, map[string]any{"id": fmt.Sprintf("r%04d", i), "text": strings.Repeat("y", 100)})
			}
			result = map[string]any{"content": []any{}, "structuredContent": map[string]any{"records": recs}}
		}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (v *vault) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.calls)
}

type toolHarness struct {
	exec  *Executor
	model *scriptClient
	vault *vault
	logs  *bytes.Buffer
	turn  agentdomain.Turn
}

func newToolHarness(t *testing.T, script func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse) *toolHarness {
	t.Helper()
	v := &vault{}
	srv := httptest.NewServer(v)
	t.Cleanup(srv.Close)
	model := &scriptClient{script: script}
	f := &fakeClient{}
	exec, box := newExec(t, f)
	exec.newClient = func(Config, string) aibuilder.Client { return model }
	logs := &bytes.Buffer{}
	exec = exec.WithMCP(true).WithLogger(slog.New(slog.NewJSONHandler(logs, nil)))
	exec.toolTimeout = 200 * time.Millisecond
	secret, _ := box.Seal("Bearer tok-secret")
	key, _ := box.Seal("sk-model-secret")
	cfg, _ := json.Marshal(Config{Provider: ProviderOpenAI, Model: "m", APIKey: key, MCPServers: []MCPServer{{Name: "vault", URL: srv.URL, AuthSecret: secret}}})
	return &toolHarness{exec: exec, model: model, vault: v, logs: logs, turn: agentdomain.Turn{
		Agent:        agentdomain.Agent{ID: "agt_1", Managed: cfg},
		Conversation: agentdomain.Conversation{ID: "conv_1", SubscriberID: "sub_9"},
		Message:      act("user", "what did we agree with X?"),
	}}
}

func call(id, name, args string) aibuilder.ToolCall {
	return aibuilder.ToolCall{ID: id, Name: name, Input: json.RawMessage(args)}
}

func results(req aibuilder.MessageRequest) []aibuilder.ToolResult {
	return req.Messages[len(req.Messages)-1].ToolResults
}

// TestDispatch_ToolRound: tools are listed and offered as <server>__<tool>,
// each call goes to the server with the opened secret and the correlation
// headers, and its result is fed back once: structuredContent when present,
// otherwise the text.
func TestDispatch_ToolRound(t *testing.T) {
	h := newToolHarness(t, func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse {
		if n == 1 {
			return aibuilder.MessageResponse{ToolCalls: []aibuilder.ToolCall{
				call("c1", "vault__get_record", `{"id":"rec_1"}`),
				call("c2", "vault__search_vault", `{"query":"X"}`),
			}}
		}
		return aibuilder.MessageResponse{Text: "We agreed on Friday [rec_1]."}
	})
	reply, err := h.exec.Dispatch(context.Background(), h.turn)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Parts[0].Text != "We agreed on Friday [rec_1]." {
		t.Fatalf("reply = %+v", reply)
	}
	if len(h.model.reqs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(h.model.reqs))
	}
	var names []string
	for _, td := range h.model.reqs[0].Tools {
		names = append(names, td.Name)
	}
	if strings.Join(names, ",") != "vault__get_record,vault__search_vault,vault__fail,vault__slow,vault__big,vault__wide" {
		t.Fatalf("offered tools = %v", names)
	}
	second := h.model.reqs[1]
	asst := second.Messages[len(second.Messages)-2]
	if asst.Role != aibuilder.RoleAssistant || len(asst.ToolCalls) != 2 {
		t.Fatalf("assistant tool turn = %+v", asst)
	}
	res := results(second)
	if len(res) != 2 || res[0].ToolCallID != "c1" || res[1].ToolCallID != "c2" || res[0].IsError || res[1].IsError {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Content != `{"id":"rec_1","passage":"BODY-MARKER passage"}` {
		t.Fatalf("structured result = %s", res[0].Content)
	}
	if res[1].Content != "BODY-MARKER hit" {
		t.Fatalf("text result = %s", res[1].Content)
	}
	h.vault.mu.Lock()
	defer h.vault.mu.Unlock()
	for _, hd := range h.vault.headers {
		if hd.Get("Authorization") != "Bearer tok-secret" || hd.Get("Notify-Agent-Id") != "agt_1" || hd.Get("Notify-Subscriber-Id") != "sub_9" {
			t.Fatalf("headers = %v", hd)
		}
	}
}

// TestDispatch_ToolFailuresAreTypedAndTheTurnStillAnswers: a tool's own
// error, a timeout, an oversized response and an unknown tool each come back
// to the model as a typed tool error, and the turn still yields a reply.
func TestDispatch_ToolFailuresAreTypedAndTheTurnStillAnswers(t *testing.T) {
	h := newToolHarness(t, func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse {
		if n == 1 {
			return aibuilder.MessageResponse{ToolCalls: []aibuilder.ToolCall{
				call("c1", "vault__fail", `{}`),
				call("c2", "vault__slow", `{}`),
				call("c3", "vault__big", `{}`),
				call("c4", "vault__missing", `{}`),
				call("c5", "vault__get_record", `["not","an","object"]`),
			}}
		}
		return aibuilder.MessageResponse{Text: "I couldn't reach your notes."}
	})
	reply, err := h.exec.Dispatch(context.Background(), h.turn)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Parts[0].Text != "I couldn't reach your notes." {
		t.Fatalf("reply = %+v", reply)
	}
	res := results(h.model.reqs[1])
	if len(res) != 5 {
		t.Fatalf("results = %+v", res)
	}
	if !res[0].IsError || res[0].Content != "no grant" {
		t.Fatalf("isError result = %+v", res[0])
	}
	for i, want := range map[int]string{1: "timeout", 2: "too_large", 3: "rejected", 4: "rejected"} {
		var body struct {
			Error struct {
				Class string `json:"class"`
			} `json:"error"`
		}
		if !res[i].IsError || json.Unmarshal([]byte(res[i].Content), &body) != nil || body.Error.Class != want {
			t.Fatalf("result %d = %+v, want class %s", i, res[i], want)
		}
	}
}

// TestDispatch_RoundCap: a model that keeps calling tools gets at most six
// rounds; the sixth forbids tools so the agent still answers.
func TestDispatch_RoundCap(t *testing.T) {
	h := newToolHarness(t, func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse {
		return aibuilder.MessageResponse{Text: fmt.Sprintf("round %d", n), ToolCalls: []aibuilder.ToolCall{call(fmt.Sprintf("c%d", n), "vault__search_vault", `{}`)}}
	})
	reply, err := h.exec.Dispatch(context.Background(), h.turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.model.reqs) != maxModelRounds || reply.Parts[0].Text != "round 6" {
		t.Fatalf("model calls = %d reply = %+v", len(h.model.reqs), reply)
	}
	for i, r := range h.model.reqs {
		last := i == maxModelRounds-1
		if (r.ToolChoice == "none") != last || len(r.Tools) == 0 {
			t.Fatalf("request %d: tool choice %q, %d tools", i, r.ToolChoice, len(r.Tools))
		}
	}
	if got := h.vault.callCount(); got != maxModelRounds-1 {
		t.Fatalf("tool calls = %d, want %d", got, maxModelRounds-1)
	}
}

// TestDispatch_ToolCallCap: no more than twelve calls reach servers in a
// turn; the rest are refused back to the model.
func TestDispatch_ToolCallCap(t *testing.T) {
	h := newToolHarness(t, func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse {
		if n > 3 {
			return aibuilder.MessageResponse{Text: "done"}
		}
		var calls []aibuilder.ToolCall
		for i := 0; i < 5; i++ {
			calls = append(calls, call(fmt.Sprintf("c%d_%d", n, i), "vault__search_vault", `{}`))
		}
		return aibuilder.MessageResponse{ToolCalls: calls}
	})
	if _, err := h.exec.Dispatch(context.Background(), h.turn); err != nil {
		t.Fatal(err)
	}
	if got := h.vault.callCount(); got != maxToolCalls {
		t.Fatalf("calls that reached the server = %d, want %d", got, maxToolCalls)
	}
	res := results(h.model.reqs[3])
	if !res[2].IsError || !strings.Contains(res[2].Content, `"rejected"`) || res[1].IsError {
		t.Fatalf("third round results = %+v", res)
	}
}

// TestDispatch_ResultBudgets: a result over the per-call budget is cut down
// structurally with a truncation flag, and the turn's results never exceed
// the aggregate budget.
func TestDispatch_ResultBudgets(t *testing.T) {
	h := newToolHarness(t, func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse {
		if n == 1 {
			var calls []aibuilder.ToolCall
			for i := 0; i < 6; i++ {
				calls = append(calls, call(fmt.Sprintf("c%d", i), "vault__wide", `{}`))
			}
			return aibuilder.MessageResponse{ToolCalls: calls}
		}
		return aibuilder.MessageResponse{Text: "ok"}
	})
	if _, err := h.exec.Dispatch(context.Background(), h.turn); err != nil {
		t.Fatal(err)
	}
	total := 0
	for i, r := range results(h.model.reqs[1]) {
		if len(r.Content) > maxResultBytes {
			t.Fatalf("result %d is %d bytes, over %d", i, len(r.Content), maxResultBytes)
		}
		var body struct {
			Truncated bool `json:"truncated"`
			Content   struct {
				Records []struct {
					ID string `json:"id"`
				} `json:"records"`
			} `json:"content"`
			Error struct {
				Class string `json:"class"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(r.Content), &body); err != nil {
			t.Fatalf("result %d is not JSON: %v", i, err)
		}
		if body.Error.Class == "too_large" {
			continue
		}
		total += len(r.Content)
		if !body.Truncated || len(body.Content.Records) == 0 || body.Content.Records[0].ID != "r0000" {
			t.Fatalf("result %d: truncated=%v records=%d", i, body.Truncated, len(body.Content.Records))
		}
	}
	if total > maxTurnResultBytes {
		t.Fatalf("results total %d bytes, over the turn budget %d", total, maxTurnResultBytes)
	}
}

// TestDispatch_ToolLogIsRedacted: each call is logged with its tool, call id,
// record ids, byte counts, duration and error class, and never with result
// bodies, free-text arguments or auth material.
func TestDispatch_ToolLogIsRedacted(t *testing.T) {
	h := newToolHarness(t, func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse {
		if n == 1 {
			return aibuilder.MessageResponse{ToolCalls: []aibuilder.ToolCall{
				call("c1", "vault__get_record", `{"id":"rec_1","recordIds":["rec_2","rec_3"],"query":"FREE-TEXT-MARKER about X","note":"rec_9"}`),
				call("c2", "vault__fail", `{"record_id":"rec_4"}`),
			}}
		}
		return aibuilder.MessageResponse{Text: "ok"}
	})
	if _, err := h.exec.Dispatch(context.Background(), h.turn); err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	for _, banned := range []string{"BODY-MARKER", "FREE-TEXT-MARKER", "tok-secret", "sk-model-secret", "enc:1:", "no grant", "rec_9"} {
		if strings.Contains(logs, banned) {
			t.Fatalf("log carries %q:\n%s", banned, logs)
		}
	}
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "managed agent tool call" {
			entries = append(entries, m)
		}
	}
	if len(entries) != 2 {
		t.Fatalf("tool call entries = %d:\n%s", len(entries), logs)
	}
	e := entries[0]
	if e["tool"] != "vault__get_record" || e["call_id"] != "c1" || e["agent_id"] != "agt_1" || e["error_class"] != "" {
		t.Fatalf("entry = %v", e)
	}
	if ids, _ := e["record_ids"].([]any); len(ids) != 3 || ids[0] != "rec_1" || ids[1] != "rec_2" || ids[2] != "rec_3" {
		t.Fatalf("record ids = %v", e["record_ids"])
	}
	for _, k := range []string{"request_bytes", "response_bytes", "result_bytes", "duration_ms"} {
		if _, ok := e[k].(float64); !ok {
			t.Fatalf("entry has no %s: %v", k, e)
		}
	}
	if e["request_bytes"].(float64) == 0 || e["response_bytes"].(float64) == 0 {
		t.Fatalf("byte counts = %v", e)
	}
	if entries[1]["error_class"] != "tool_error" {
		t.Fatalf("isError entry = %v", entries[1])
	}
}

// TestDispatch_UnreachableServerStillAnswers: a server that cannot be listed
// offers no tools and the turn goes on without them.
func TestDispatch_UnreachableServerStillAnswers(t *testing.T) {
	h := newToolHarness(t, func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse {
		return aibuilder.MessageResponse{Text: "answered"}
	})
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	cfg, _ := json.Marshal(Config{Provider: ProviderOpenAI, Model: "m", MCPServers: []MCPServer{{Name: "gone", URL: dead.URL}}})
	h.turn.Agent.Managed = cfg
	reply, err := h.exec.Dispatch(context.Background(), h.turn)
	if err != nil || reply.Parts[0].Text != "answered" {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
	if len(h.model.reqs) != 1 || len(h.model.reqs[0].Tools) != 0 {
		t.Fatalf("requests = %+v", h.model.reqs)
	}
}

// TestDispatch_SealedToolSecretWithoutBoxIsPermanent mirrors the apiKey rule.
func TestDispatch_SealedToolSecretWithoutBoxIsPermanent(t *testing.T) {
	exec := New(nil, func(Config, string) aibuilder.Client { return &fakeClient{resp: aibuilder.MessageResponse{Text: "x"}} })
	cfg := `{"provider":"openai","model":"m","mcpServers":[{"name":"vault","url":"https://vault.example/mcp","authSecret":"enc:1:abc"}]}`
	_, err := exec.Dispatch(context.Background(), agentdomain.Turn{Agent: agentdomain.Agent{Managed: json.RawMessage(cfg)}, Message: act("user", "hi")})
	if !agentdomain.IsPermanent(err) {
		t.Fatalf("err = %v, want permanent", err)
	}
}

// TestParseConfig_MCPServers: server names must be unambiguous in
// <server>__<tool>, URLs must be https (or loopback) outside private ranges.
func TestParseConfig_MCPServers(t *testing.T) {
	base := `{"provider":"openai","model":"m","mcpServers":%s}`
	good := `[{"name":"vault","url":"https://vault.example/mcp","authSecret":"Bearer t"},{"name":"tickets-2","url":"http://127.0.0.1:9000/mcp","authHeader":"X-Api-Key"}]`
	c, err := ParseConfig(json.RawMessage(fmt.Sprintf(base, good)), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MCPServers) != 2 || c.MCPServers[0].AuthHeader != "Authorization" || c.MCPServers[1].AuthHeader != "X-Api-Key" {
		t.Fatalf("servers = %+v", c.MCPServers)
	}
	bad := map[string]string{
		"bad url":       `[{"name":"v","url":"not a url"}]`,
		"no url":        `[{"name":"v"}]`,
		"plain http":    `[{"name":"v","url":"http://vault.example/mcp"}]`,
		"private ip":    `[{"name":"v","url":"https://10.1.2.3/mcp"}]`,
		"metadata ip":   `[{"name":"v","url":"https://169.254.169.254/"}]`,
		"duplicate":     `[{"name":"v","url":"https://a.example"},{"name":"v","url":"https://b.example"}]`,
		"underscore":    `[{"name":"my_vault","url":"https://a.example"}]`,
		"upper":         `[{"name":"Vault","url":"https://a.example"}]`,
		"empty name":    `[{"name":"","url":"https://a.example"}]`,
		"long name":     `[{"name":"` + strings.Repeat("a", 33) + `","url":"https://a.example"}]`,
		"header":        `[{"name":"v","url":"https://a.example","authHeader":"Bad Header"}]`,
		"reserved hdr":  `[{"name":"v","url":"https://a.example","authHeader":"Notify-Agent-Id"}]`,
		"session hdr":   `[{"name":"v","url":"https://a.example","authHeader":"mcp-session-id"}]`,
		"too many":      tooMany(),
		"not an object": `["https://a.example"]`,
	}
	for name, servers := range bad {
		if _, err := ParseConfig(json.RawMessage(fmt.Sprintf(base, servers)), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseConfig(json.RawMessage(fmt.Sprintf(base, good)), false); err == nil {
		t.Error("loopback http accepted outside development")
	}
	if _, err := ParseConfig(json.RawMessage(`{"provider":"openai","model":"m","mcp_servers":[]}`), true); err == nil || !strings.Contains(err.Error(), "mcpServers") {
		t.Errorf("snake_case mcp_servers: %v, want a pointer to mcpServers", err)
	}
}

func tooMany() string {
	var s []string
	for i := 0; i <= maxMCPServers; i++ {
		s = append(s, fmt.Sprintf(`{"name":"s%d","url":"https://a.example"}`, i))
	}
	return "[" + strings.Join(s, ",") + "]"
}

// TestFit: an over-budget value is cut down at the structured level, never
// mid-value, and flagged.
func TestFit(t *testing.T) {
	s, cut := fitText("héllo wörld", 100)
	if cut || s != "héllo wörld" {
		t.Fatalf("fitting text = %q %v", s, cut)
	}
	s, cut = fitText(strings.Repeat("é", 100), 60)
	var w struct {
		Truncated bool   `json:"truncated"`
		Content   string `json:"content"`
	}
	if !cut || len(s) > 60 || json.Unmarshal([]byte(s), &w) != nil || !w.Truncated || w.Content == "" || !utf8.ValidString(w.Content) {
		t.Fatalf("cut text = %q (%d bytes)", s, len(s))
	}
	raw := json.RawMessage(`{"a":[1,2,3,4,5,6,7,8,9,10],"b":"` + strings.Repeat("z", 200) + `","c":true}`)
	s, cut = fitJSON(raw, 80)
	var o struct {
		Truncated bool           `json:"truncated"`
		Content   map[string]any `json:"content"`
	}
	if !cut || len(s) > 80 || json.Unmarshal([]byte(s), &o) != nil || !o.Truncated {
		t.Fatalf("cut object = %s (%d bytes)", s, len(s))
	}
	if a, _ := o.Content["a"].([]any); len(a) == 0 || a[0] != float64(1) {
		t.Fatalf("cut object lost the leading value: %s", s)
	}
	if s, cut = fitJSON(json.RawMessage(`{"n": 1}`), 80); cut || s != `{"n":1}` {
		t.Fatalf("fitting object = %s %v", s, cut)
	}
	if _, cut = fitJSON(raw, 10); !cut {
		t.Fatal("a budget too small for any content must still report a cut")
	}
}

// useVaults points the harness's agent at the given stub servers, named
// v0, v1, ... in order.
func useVaults(t *testing.T, h *toolHarness, vs ...*vault) {
	t.Helper()
	var servers []MCPServer
	for i, v := range vs {
		srv := httptest.NewServer(v)
		t.Cleanup(srv.Close)
		servers = append(servers, MCPServer{Name: fmt.Sprintf("v%d", i), URL: srv.URL, AuthHeader: "Authorization"})
	}
	cfg, _ := json.Marshal(Config{Provider: ProviderOpenAI, Model: "m", MCPServers: servers})
	h.turn.Agent.Managed = cfg
}

func answer(int, aibuilder.MessageRequest) aibuilder.MessageResponse {
	return aibuilder.MessageResponse{Text: "ok"}
}

func offered(h *toolHarness) []string {
	var names []string
	for _, td := range h.model.reqs[0].Tools {
		names = append(names, td.Name)
	}
	return names
}

// TestDispatch_ToolCountCaps: at most 64 tools per server and 128 per turn
// are offered, in server order; the rest are skipped with a warning.
func TestDispatch_ToolCountCaps(t *testing.T) {
	h := newToolHarness(t, answer)
	useVaults(t, h, &vault{extra: 70}, &vault{extra: 70}, &vault{extra: 70})
	if _, err := h.exec.Dispatch(context.Background(), h.turn); err != nil {
		t.Fatal(err)
	}
	names := offered(h)
	if len(names) != maxToolsPerTurn {
		t.Fatalf("offered %d tools, want %d", len(names), maxToolsPerTurn)
	}
	if names[0] != "v0__get_record" || names[maxToolsPerServer-1] != "v0__t057" || names[maxToolsPerServer] != "v1__get_record" || names[maxToolsPerTurn-1] != "v1__t057" {
		t.Fatalf("offered order = %v ... %v", names[:2], names[len(names)-2:])
	}
	if !strings.Contains(h.logs.String(), "managed agent tool skipped") {
		t.Fatalf("no warning for skipped tools:\n%s", h.logs.String())
	}
}

// TestDispatch_ToolDefinitionCaps: a definition over 32 KiB is skipped, and
// the turn's definitions stay within 256 KiB.
func TestDispatch_ToolDefinitionCaps(t *testing.T) {
	h := newToolHarness(t, answer)
	useVaults(t, h, &vault{extra: 1, desc: maxToolDefBytes}, &vault{extra: 40, desc: 6 << 10}, &vault{extra: 40, desc: 6 << 10})
	if _, err := h.exec.Dispatch(context.Background(), h.turn); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, td := range h.model.reqs[0].Tools {
		if td.Name == "v0__t000" {
			t.Fatal("an oversized definition was offered")
		}
		total += len(td.Name) + len(td.Description) + len(td.InputSchema)
	}
	if total > maxToolDefsBytes || len(h.model.reqs[0].Tools) < 10 {
		t.Fatalf("definitions total %d bytes over %d tools", total, len(h.model.reqs[0].Tools))
	}
	if !strings.Contains(h.logs.String(), "managed agent tool skipped") {
		t.Fatal("no warning for skipped tools")
	}
}

// TestDispatch_ServersAreListedInParallel: slow servers are listed at once,
// not one after another, and tool order stays the configured order.
func TestDispatch_ServersAreListedInParallel(t *testing.T) {
	h := newToolHarness(t, answer)
	h.exec.toolTimeout = 2 * time.Second
	useVaults(t, h, &vault{listDelay: 300 * time.Millisecond}, &vault{listDelay: 300 * time.Millisecond}, &vault{listDelay: 300 * time.Millisecond})
	start := time.Now()
	if _, err := h.exec.Dispatch(context.Background(), h.turn); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Fatalf("listing three 300ms servers took %v", d)
	}
	names := offered(h)
	if len(names) != 18 || names[0] != "v0__get_record" || names[6] != "v1__get_record" || names[12] != "v2__get_record" {
		t.Fatalf("offered = %v", names)
	}
}

// TestDispatch_FinalRoundWithoutTextFallsBack: a model that still asks for
// tools on the last round gets a fixed reply rather than a failed turn that
// would rerun forever.
func TestDispatch_FinalRoundWithoutTextFallsBack(t *testing.T) {
	h := newToolHarness(t, func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse {
		return aibuilder.MessageResponse{ToolCalls: []aibuilder.ToolCall{call(fmt.Sprintf("c%d", n), "vault__search_vault", `{}`)}}
	})
	reply, err := h.exec.Dispatch(context.Background(), h.turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.model.reqs) != maxModelRounds || reply.Parts[0].Text != fallbackReply {
		t.Fatalf("model calls %d reply %+v", len(h.model.reqs), reply)
	}
}

// TestDispatch_LogNamesOnlyOfferedToolsAndBoundsCallIDs: a name the model
// made up is logged as "unknown", and a call id is cut to 64 bytes.
func TestDispatch_LogNamesOnlyOfferedToolsAndBoundsCallIDs(t *testing.T) {
	longID := strings.Repeat("i", 200)
	h := newToolHarness(t, func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse {
		if n == 1 {
			return aibuilder.MessageResponse{ToolCalls: []aibuilder.ToolCall{call(longID, "INVENTED-NAME-MARKER", `{}`)}}
		}
		return aibuilder.MessageResponse{Text: "ok"}
	})
	if _, err := h.exec.Dispatch(context.Background(), h.turn); err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	if strings.Contains(logs, "INVENTED-NAME-MARKER") || strings.Contains(logs, longID[:65]) || !strings.Contains(logs, `"tool":"unknown"`) || !strings.Contains(logs, `"call_id":"`+longID[:64]+`"`) {
		t.Fatalf("log:\n%s", logs)
	}
}

// TestDispatch_ToolAliases: a tool name the model APIs would refuse, or one
// too long, is offered under a deterministic alias that maps back to the
// original; a name offered twice is skipped.
func TestDispatch_ToolAliases(t *testing.T) {
	long := strings.Repeat("x", 70)
	h := newToolHarness(t, func(n int, req aibuilder.MessageRequest) aibuilder.MessageResponse {
		if n == 1 {
			var calls []aibuilder.ToolCall
			for i, td := range req.Tools {
				if strings.HasPrefix(td.Name, "v0__notes_search_") || strings.HasPrefix(td.Name, "v0__"+long[:10]) {
					calls = append(calls, call(fmt.Sprintf("c%d", i), td.Name, `{}`))
				}
			}
			return aibuilder.MessageResponse{ToolCalls: calls}
		}
		return aibuilder.MessageResponse{Text: "ok"}
	})
	v := &vault{more: []string{"notes.search", long, "dup", "dup"}}
	useVaults(t, h, v)
	if _, err := h.exec.Dispatch(context.Background(), h.turn); err != nil {
		t.Fatal(err)
	}
	names := offered(h)
	if len(names) != 9 || names[8] != "v0__dup" {
		t.Fatalf("offered = %v", names)
	}
	for _, n := range names {
		if !toolNamePattern.MatchString(n) {
			t.Fatalf("offered name %q breaks the provider rule", n)
		}
	}
	if a, b := toolAlias("v0", "notes.search"), toolAlias("v0", "notes.search"); a != b || a != names[6] || a == toolAlias("v0", "notes,search") {
		t.Fatalf("alias not deterministic or not distinct: %q %q", a, b)
	}
	if len(names[7]) > 64 || !strings.HasPrefix(names[7], "v0__xxxx") {
		t.Fatalf("long alias = %q", names[7])
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if strings.Join(v.calls, ",") != "notes.search,"+long {
		t.Fatalf("server saw calls %v, want the original names", v.calls)
	}
	if !strings.Contains(h.logs.String(), "managed agent tool skipped") {
		t.Fatal("no warning for the duplicate")
	}
}

// TestParseConfig_AuthHeaderRules: the auth header must be a header token
// that does not steer routing, framing or the protocol, or impersonate the
// correlation headers.
func TestParseConfig_AuthHeaderRules(t *testing.T) {
	cfg := func(h string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"provider": "openai", "model": "m", "mcpServers": []any{map[string]any{"name": "v", "url": "https://a.example", "authHeader": h}}})
		return b
	}
	for _, h := range []string{"Authorization", "X-Api-Key", "x-vault-token", "Api_Key"} {
		if _, err := ParseConfig(cfg(h), false); err != nil {
			t.Errorf("%q refused: %v", h, err)
		}
	}
	for _, h := range []string{"Bad Header", "X:Y", "Host", "connection", "Keep-Alive", "Proxy-Authorization", "proxy-anything", "TE", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length", "content-type", "Accept", "MCP-Session-Id", "Mcp-Protocol-Version", "Forwarded", "X-Forwarded-For", "x-forwarded-host", "notify-agent-id", "NOTIFY-SUBSCRIBER-ID", strings.Repeat("a", 65)} {
		if _, err := ParseConfig(cfg(h), false); err == nil {
			t.Errorf("%q accepted", h)
		}
	}
}
