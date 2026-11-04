package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/a2a"
	"github.com/danmestas/notify/internal/a2aauth"
	agentsapi "github.com/danmestas/notify/internal/agents/api"
	"github.com/danmestas/notify/internal/agents/cards"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

const a2aSubject = "agents.prompt.test.a2a"

// a2aRPC posts a JSON-RPC call and returns the HTTP status and decoded reply.
func a2aRPC(t *testing.T, url, token, method string, params any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func sendParams(messageID, contextID, text string) map[string]any {
	m := map[string]any{
		"kind": "message", "messageId": messageID, "role": "user",
		"parts": []map[string]any{{"kind": "text", "text": text}},
	}
	if contextID != "" {
		m["contextId"] = contextID
	}
	return map[string]any{"message": m}
}

func rpcResult(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	return res
}

func rpcErrCode(out map[string]any) int {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return int(c)
}

func taskState(res map[string]any) string {
	st, _ := res["status"].(map[string]any)
	s, _ := st["state"].(string)
	return s
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestA2AEndpoint_E2E(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	eng, err := engine.New(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Stop() })
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	meshClient, err := mesh.Dial(natsURL, mesh.WithTimeout(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(meshClient.Close)

	ags := agentstore.New(st)
	ing := ingress.New(ags, st, eng, bus).WithRuntime(agentdomain.RuntimeMesh, meshClient)
	eng.Handle(ingress.TaskMesh, ing.HandleTurn)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatal(err)
	}

	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	apiKeyA, scopeA, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatal(err)
	}
	_, scopeB, err := id.Bootstrap(ctx, "Other", "dev")
	if err != nil {
		t.Fatal(err)
	}
	auth := a2aauth.New(st, id, nil)
	mint := func(scope domain.Scope, name string) string {
		t.Helper()
		_, tok, err := auth.Mint(ctx, scope, name, "")
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + tok
	}
	keyA1, keyA2, keyB := mint(scopeA, "one"), mint(scopeA, "two"), mint(scopeB, "other-env")

	agent, err := ags.Agents().Create(ctx, scopeA, agentdomain.Agent{
		Identifier: "stub", Name: "Stub", Runtime: agentdomain.RuntimeMesh, Active: true, MeshSubject: a2aSubject,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The fake mesh agent: ack, then answer "ECHO::<text>"; a prompt that
	// contains "slow" answers after a delay. turns counts dispatches.
	var turns atomic.Int64
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	if _, err := nc.Subscribe(a2aSubject, func(m *nats.Msg) {
		turns.Add(1)
		var env struct {
			Parts []agentdomain.Part `json:"parts"`
		}
		_ = json.Unmarshal(m.Data, &env)
		_ = m.Respond([]byte(`{}`))
		var text string
		for _, p := range env.Parts {
			text += p.Text
		}
		replyTo := m.Header.Get("reply_to")
		go func() {
			if strings.Contains(text, "slow") {
				time.Sleep(1500 * time.Millisecond)
			}
			ans, _ := json.Marshal(map[string]any{
				"messageId": "stub-answer", "role": "agent",
				"parts": []map[string]string{{"text": "ECHO::" + text}},
			})
			_ = nc.Publish(replyTo, ans)
		}()
	}); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}

	src := &fakeCards{m: map[string]cards.Card{}}
	src.set("test.a2a", `{"protocolVersion":"0.3.0","name":"Receiver","description":"d","version":"1","url":"http://old","preferredTransport":"GRPC",
"capabilities":{"streaming":true,"extensions":[{"uri":"`+cards.RuntimeExtensionURI+`","params":{"model":"m"}}]},
"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[{"id":"s","name":"S","description":"d","tags":[]}]}`, time.Now())

	aud := audience.New(st)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil),
		Inbox: inboxapi.New(st), Store: st, Ingress: ing,
		Agents:  agentsapi.New(ags).WithCards(src),
		A2AAuth: auth,
		A2A:     a2a.New(ags, ing, eng).WithCards(src).WithWait(700 * time.Millisecond).WithPublicBaseURL("https://notify.example"),
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/a2a/" + agent.ID

	t.Run("card overrides what notify owns and keeps the receiver's", func(t *testing.T) {
		code, body := doGet(t, url+"/.well-known/agent-card.json", keyA1)
		if code != http.StatusOK {
			t.Fatalf("%d %s", code, body)
		}
		var c map[string]any
		_ = json.Unmarshal([]byte(body), &c)
		if c["url"] != "https://notify.example/a2a/"+agent.ID || c["preferredTransport"] != "JSONRPC" {
			t.Errorf("url/transport: %v %v", c["url"], c["preferredTransport"])
		}
		if c["name"] != "Receiver" || !strings.Contains(body, cards.RuntimeExtensionURI) || !strings.Contains(body, `"skills":[{`) {
			t.Errorf("receiver fields lost: %s", body)
		}
		caps, _ := c["capabilities"].(map[string]any)
		if caps["streaming"] != false {
			t.Errorf("streaming = %v", caps["streaming"])
		}
		if !strings.Contains(body, `"securitySchemes":{"bearer":{"scheme":"bearer","type":"http"}}`) || !strings.Contains(body, `"security":[{"bearer":[]}]`) {
			t.Errorf("security: %s", body)
		}
	})

	t.Run("fast reply completes", func(t *testing.T) {
		code, out := a2aRPC(t, url, keyA1, "message/send", sendParams("m-fast", "", "hello"))
		if code != http.StatusOK {
			t.Fatalf("%d %v", code, out)
		}
		res := rpcResult(t, out)
		if taskState(res) != "completed" || res["kind"] != "task" || res["contextId"] == "" {
			t.Fatalf("task: %v", res)
		}
		if !strings.Contains(toJSON(res["artifacts"]), "ECHO::hello") {
			t.Errorf("artifacts: %v", res["artifacts"])
		}
	})

	t.Run("slow reply is working then completed via tasks/get", func(t *testing.T) {
		_, out := a2aRPC(t, url, keyA1, "message/send", sendParams("m-slow", "ctx-slow", "slow please"))
		res := rpcResult(t, out)
		if taskState(res) != "working" || res["contextId"] != "ctx-slow" {
			t.Fatalf("want working in ctx-slow: %v", res)
		}
		taskID, _ := res["id"].(string)
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, out = a2aRPC(t, url, keyA1, "tasks/get", map[string]any{"id": taskID})
			res = rpcResult(t, out)
			if taskState(res) == "completed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("never completed: %v", res)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !strings.Contains(toJSON(res["artifacts"]), "ECHO::slow please") {
			t.Errorf("artifacts: %v", res["artifacts"])
		}
		// Another credential in the same environment cannot read the task.
		_, out = a2aRPC(t, url, keyA2, "tasks/get", map[string]any{"id": taskID})
		if rpcErrCode(out) != -32001 {
			t.Errorf("other credential's tasks/get = %v, want -32001", out)
		}
	})

	t.Run("retry of the same messageId is the same task and one turn", func(t *testing.T) {
		before := turns.Load()
		_, out1 := a2aRPC(t, url, keyA1, "message/send", sendParams("m-retry", "ctx-retry", "once"))
		_, out2 := a2aRPC(t, url, keyA1, "message/send", sendParams("m-retry", "ctx-retry", "once"))
		r1, r2 := rpcResult(t, out1), rpcResult(t, out2)
		if r1["id"] != r2["id"] || r1["id"] == "" {
			t.Fatalf("ids differ: %v %v", r1["id"], r2["id"])
		}
		if taskState(r2) != "completed" {
			t.Errorf("retry state = %s", taskState(r2))
		}
		if got := turns.Load() - before; got != 1 {
			t.Errorf("turns = %d, want 1", got)
		}
		_, out3 := a2aRPC(t, url, keyA1, "message/send", sendParams("m-retry", "ctx-retry", "different"))
		if rpcErrCode(out3) != -32602 {
			t.Errorf("same id, different text = %v, want -32602", out3)
		}
	})

	t.Run("a contextId is private to its credential", func(t *testing.T) {
		_, o1 := a2aRPC(t, url, keyA1, "message/send", sendParams("m-c1", "shared-ctx", "one"))
		_, o2 := a2aRPC(t, url, keyA2, "message/send", sendParams("m-c2", "shared-ctx", "two"))
		t1, t2 := rpcResult(t, o1), rpcResult(t, o2)
		if t1["id"] == t2["id"] {
			t.Fatal("same task")
		}
		convs, err := ags.Conversations().ListByAgent(ctx, scopeA, agent.ID, 100, "")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, c := range convs {
			if strings.HasSuffix(c.PlatformThreadID, ":shared-ctx") {
				n++
			}
		}
		if n != 2 {
			t.Errorf("conversations for shared-ctx = %d, want 2", n)
		}
	})

	t.Run("wrong environment and unknown agent are the same 404", func(t *testing.T) {
		code, _ := a2aRPC(t, url, keyB, "message/send", sendParams("m-x", "", "hi"))
		if code != http.StatusNotFound {
			t.Errorf("wrong env = %d", code)
		}
		if code, _ := doGet(t, url+"/.well-known/agent-card.json", keyB); code != http.StatusNotFound {
			t.Errorf("wrong env card = %d", code)
		}
		code, _ = a2aRPC(t, srv.URL+"/a2a/nope", keyA1, "message/send", sendParams("m-y", "", "hi"))
		if code != http.StatusNotFound {
			t.Errorf("unknown agent = %d", code)
		}
	})

	t.Run("a tenant API key is 401", func(t *testing.T) {
		code, _ := a2aRPC(t, url, "ApiKey "+apiKeyA, "message/send", sendParams("m-z", "", "hi"))
		if code != http.StatusUnauthorized {
			t.Errorf("tenant key = %d", code)
		}
		if code, _ := a2aRPC(t, url, "", "message/send", sendParams("m-z", "", "hi")); code != http.StatusUnauthorized {
			t.Errorf("no credential = %d", code)
		}
	})

	t.Run("unsupported and malformed", func(t *testing.T) {
		for _, m := range []string{"message/stream", "tasks/resubscribe", "tasks/cancel", "nope"} {
			_, out := a2aRPC(t, url, keyA1, m, map[string]any{})
			if rpcErrCode(out) != -32004 {
				t.Errorf("%s = %v, want -32004", m, out)
			}
		}
		r, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("{not json"))
		r.Header.Set("Authorization", keyA1)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(raw), "-32700") {
			t.Errorf("malformed JSON = %s", raw)
		}
		_, out := a2aRPC(t, url, keyA1, "message/send", map[string]any{"message": map[string]any{
			"messageId": "m-d", "parts": []map[string]any{{"kind": "data", "data": map[string]any{}}},
		}})
		if rpcErrCode(out) != -32602 {
			t.Errorf("data part = %v, want -32602", out)
		}
		_, out = a2aRPC(t, url, keyA1, "tasks/get", map[string]any{"id": "01NOSUCH"})
		if rpcErrCode(out) != -32001 {
			t.Errorf("unknown task = %v", out)
		}
	})
}
