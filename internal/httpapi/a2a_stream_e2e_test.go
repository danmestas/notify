package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/a2a"
	"github.com/danmestas/notify/internal/a2aauth"
	agentsapi "github.com/danmestas/notify/internal/agents/api"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/agents/tasks"
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

const streamSubject = "agents.prompt.test.a2astream"

// sseEvents posts a streaming JSON-RPC call and returns every event's result
// until the server closes the stream.
func sseEvents(t *testing.T, url, token, method string, params any) []map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	r, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		raw := new(bytes.Buffer)
		_, _ = raw.ReadFrom(resp.Body)
		t.Fatalf("content-type %q: %s", ct, raw)
	}
	var out []map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		if env["id"] != float64(7) {
			t.Fatalf("event id = %v", env["id"])
		}
		res, ok := env["result"].(map[string]any)
		if !ok {
			t.Fatalf("event without result: %v", env)
		}
		out = append(out, res)
	}
	return out
}

// eventSummary renders events as "kind:state:text" for comparison.
func eventSummary(evs []map[string]any) []string {
	var out []string
	for _, e := range evs {
		kind, _ := e["kind"].(string)
		switch kind {
		case "task", "status-update":
			st, _ := e["status"].(map[string]any)
			s, _ := st["state"].(string)
			line := kind + ":" + s
			if m, ok := st["message"].(map[string]any); ok && kind == "status-update" && s == "working" {
				line += ":" + toJSON(m["parts"])
				_ = m
			}
			if f, _ := e["final"].(bool); f {
				line += ":final"
			}
			out = append(out, line)
		default:
			out = append(out, kind)
		}
	}
	return out
}

func TestA2AStreaming_E2E(t *testing.T) {
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
	meshClient, err := mesh.Dial(natsURL, mesh.WithTimeout(10*time.Second), mesh.WithDiscoveryWindow(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(meshClient.Close)

	ags := agentstore.New(st)
	status := tasks.New(ags.TaskStatuses(), nil)
	ing := ingress.New(ags, st, eng, bus).WithTasks(status).WithRuntime(agentdomain.RuntimeMesh, meshClient)
	eng.Handle(ingress.TaskMesh, ing.HandleTurn)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatal(err)
	}

	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	_, scopeA, err := id.Bootstrap(ctx, "Acme", "dev")
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
		Identifier: "streamer", Name: "Streamer", Runtime: agentdomain.RuntimeMesh, Active: true, MeshSubject: streamSubject,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The fake agent streams only when the prompt carries metadata.stream.
	var sawStream atomic.Int64
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	if _, err := nc.Subscribe(streamSubject, func(m *nats.Msg) {
		var env struct {
			Parts    []agentdomain.Part `json:"parts"`
			Metadata struct {
				Stream bool `json:"stream"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(m.Data, &env)
		_ = m.Respond([]byte(`{}`))
		var text string
		for _, p := range env.Parts {
			text += p.Text
		}
		replyTo := m.Header.Get("reply_to")
		stream := env.Metadata.Stream
		if stream {
			sawStream.Add(1)
		}
		pub := func(v any) {
			b, _ := json.Marshal(v)
			_ = nc.Publish(replyTo, b)
		}
		statusFrame := func(state, note string, meta map[string]any) map[string]any {
			f := map[string]any{
				"kind": "status-update", "taskId": "run-1", "contextId": "c", "final": state != "working",
				"status": map[string]any{"state": state},
			}
			if note != "" {
				f["status"] = map[string]any{"state": state, "message": map[string]any{
					"kind": "message", "messageId": "n", "taskId": "run-1", "role": "agent",
					"parts": []map[string]string{{"kind": "text", "text": note}},
				}}
			}
			if meta != nil {
				f["metadata"] = meta
			}
			return f
		}
		go func() {
			if stream {
				pub(statusFrame("working", "Running search_vault: q", nil))
				if strings.Contains(text, "slow") {
					time.Sleep(1500 * time.Millisecond)
				}
				pub(statusFrame("working", "Running read_note: n", nil))
				pub(map[string]any{
					"kind": "artifact-update", "taskId": "run-1", "contextId": "c", "append": true,
					"artifact": map[string]any{"artifactId": "answer", "parts": []map[string]string{{"kind": "text", "text": "ECHO::" + text}}},
				})
				pub(map[string]any{
					"kind": "artifact-update", "taskId": "run-1", "contextId": "c", "lastChunk": true,
					"artifact": map[string]any{"artifactId": "answer", "parts": []any{}},
				})
				pub(statusFrame("completed", "", map[string]any{"usage": map[string]any{"inputTokens": 12, "cost": 0.5}}))
			} else if strings.Contains(text, "slow") {
				time.Sleep(1500 * time.Millisecond)
			}
			pub(map[string]any{
				"messageId": "stub-answer", "role": "agent",
				"parts": []map[string]string{{"text": "ECHO::" + text}},
			})
		}()
	}); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}

	aud := audience.New(st)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil),
		Inbox: inboxapi.New(st), Store: st, Ingress: ing,
		Agents:  agentsapi.New(ags),
		A2AAuth: auth,
		A2A:     a2a.New(ags, ing, eng).WithTasks(status).WithWait(time.Second).WithPublicBaseURL("https://notify.example"),
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/a2a/" + agent.ID

	wantDone := []string{
		"task:submitted",
		`status-update:working:[{"kind":"text","text":"Running search_vault: q"}]`,
		`status-update:working:[{"kind":"text","text":"Running read_note: n"}]`,
		"artifact-update",
		"status-update:completed:final",
	}

	t.Run("card advertises streaming", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, url+"/.well-known/agent-card.json", nil)
		req.Header.Set("Authorization", keyA1)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var c map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&c)
		caps, _ := c["capabilities"].(map[string]any)
		if caps["streaming"] != true {
			t.Errorf("streaming = %v", caps["streaming"])
		}
	})

	var doneTaskID string
	t.Run("message/stream yields notes then the reply", func(t *testing.T) {
		evs := sseEvents(t, url, keyA1, "message/stream", sendParams("s-1", "ctx-s", "hello"))
		got := eventSummary(evs)
		// The task may already carry the first note when it is read.
		if len(got) < len(wantDone)-1 {
			t.Fatalf("events = %v", got)
		}
		if got[len(got)-1] != "status-update:completed:final" {
			t.Fatalf("last event = %v", got)
		}
		joined := strings.Join(got, "|")
		for _, w := range wantDone[1:] {
			if !strings.Contains(joined, w) {
				t.Errorf("missing %q in %v", w, got)
			}
		}
		if strings.Index(joined, "search_vault") > strings.Index(joined, "read_note") {
			t.Errorf("notes out of order: %v", got)
		}
		art := toJSON(evs[len(evs)-2])
		if !strings.Contains(art, "ECHO::hello") || !strings.Contains(art, `"lastChunk":true`) {
			t.Errorf("artifact event: %s", art)
		}
		last := evs[len(evs)-1]
		if !strings.Contains(toJSON(last["metadata"]), `"inputTokens":12`) {
			t.Errorf("final usage: %v", last["metadata"])
		}
		doneTaskID, _ = last["taskId"].(string)
		if sawStream.Load() == 0 {
			t.Error("agent never saw metadata.stream")
		}
	})

	t.Run("tasks/get after completion returns notes and usage", func(t *testing.T) {
		_, out := a2aRPC(t, url, keyA1, "tasks/get", map[string]any{"id": doneTaskID})
		res := rpcResult(t, out)
		if taskState(res) != "completed" {
			t.Fatalf("task: %v", res)
		}
		meta := toJSON(res["metadata"])
		for _, w := range []string{"Running search_vault: q", "Running read_note: n", `"inputTokens":12`} {
			if !strings.Contains(meta, w) {
				t.Errorf("metadata %s lacks %q", meta, w)
			}
		}
	})

	t.Run("tasks/resubscribe mid-turn follows to the end", func(t *testing.T) {
		_, out := a2aRPC(t, url, keyA1, "message/send", sendParams("s-2", "ctx-slow", "slow please"))
		res := rpcResult(t, out)
		taskID, _ := res["id"].(string)
		if taskState(res) == "completed" {
			t.Fatalf("finished too fast: %v", res)
		}
		evs := sseEvents(t, url, keyA1, "tasks/resubscribe", map[string]any{"id": taskID})
		got := eventSummary(evs)
		if got[0] != "task:working" && got[0] != "task:submitted" {
			t.Fatalf("first event %v", got)
		}
		if got[len(got)-1] != "status-update:completed:final" || !strings.Contains(toJSON(evs[len(evs)-2]), "ECHO::slow please") {
			t.Fatalf("events = %v", got)
		}
	})

	t.Run("resubscribe to a finished task replays its end", func(t *testing.T) {
		got := eventSummary(sseEvents(t, url, keyA1, "tasks/resubscribe", map[string]any{"id": doneTaskID}))
		if len(got) != 3 || got[0] != "task:completed" || got[1] != "artifact-update" || got[2] != "status-update:completed:final" {
			t.Fatalf("events = %v", got)
		}
	})

	t.Run("another credential or environment cannot reach the task", func(t *testing.T) {
		_, out := a2aRPC(t, url, keyA2, "tasks/get", map[string]any{"id": doneTaskID})
		if rpcErrCode(out) != -32001 {
			t.Errorf("tasks/get by other credential: %v", out)
		}
		if code, _ := a2aRPC(t, url, keyB, "tasks/resubscribe", map[string]any{"id": doneTaskID}); code != http.StatusNotFound {
			t.Errorf("other environment: HTTP %d", code)
		}
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"tasks/resubscribe","params":{"id":"`+doneTaskID+`"}}`,
		))
		req.Header.Set("Authorization", keyA2)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out2 map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out2)
		if rpcErrCode(out2) != -32001 || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Errorf("resubscribe by other credential: %v", out2)
		}
	})

	t.Run("websocket carries the same events and plain calls", func(t *testing.T) {
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/a2a/" + agent.ID + "/ws"
		wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
		defer wcancel()
		// No credential: refused at the upgrade.
		c0, resp0, err := websocket.Dial(wctx, wsURL, nil)
		if resp0 != nil && resp0.Body != nil {
			_ = resp0.Body.Close()
		}
		if err == nil {
			_ = c0.CloseNow()
			t.Fatal("dial without credential succeeded")
		}
		c, resp1, err := websocket.Dial(wctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {keyA1}}})
		if resp1 != nil && resp1.Body != nil {
			_ = resp1.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
		write := func(id int, method string, params any) {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
			if err := c.Write(wctx, websocket.MessageText, b); err != nil {
				t.Fatal(err)
			}
		}
		read := func() map[string]any {
			_, data, err := c.Read(wctx)
			if err != nil {
				t.Fatal(err)
			}
			var env map[string]any
			_ = json.Unmarshal(data, &env)
			return env
		}
		write(1, "message/stream", sendParams("w-1", "ctx-w", "over ws"))
		var evs []map[string]any
		for {
			env := read()
			res := rpcResult(t, env)
			evs = append(evs, res)
			if f, _ := res["final"].(bool); f {
				break
			}
		}
		got := eventSummary(evs)
		if got[len(got)-1] != "status-update:completed:final" || !strings.Contains(strings.Join(got, "|"), "read_note") {
			t.Fatalf("ws events = %v", got)
		}
		taskID, _ := evs[len(evs)-1]["taskId"].(string)
		write(2, "tasks/get", map[string]any{"id": taskID})
		if res := rpcResult(t, read()); taskState(res) != "completed" {
			t.Errorf("ws tasks/get: %v", res)
		}
		write(3, "message/send", sendParams("w-2", "ctx-w", "again"))
		if res := rpcResult(t, read()); taskState(res) != "completed" {
			t.Errorf("ws message/send: %v", res)
		}
	})

	t.Run("a streaming-unaware agent still answers", func(t *testing.T) {
		// Same agent, but a retry-free check: message/send without notes.
		_, out := a2aRPC(t, url, keyA1, "message/send", sendParams("p-1", "", "plain"))
		if res := rpcResult(t, out); taskState(res) != "completed" {
			t.Errorf("send: %v", res)
		}
	})
}
