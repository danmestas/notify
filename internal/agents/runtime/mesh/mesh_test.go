package mesh_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	"github.com/danmestas/notify/internal/dagtest"
)

// connect dials the sidecar's NATS for a test stub agent.
func connect(t *testing.T, natsURL string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// turn builds a minimal Turn targeting subject with one user message.
func turn(subject, text string) domain.Turn {
	return domain.Turn{
		Agent:   domain.Agent{Identifier: "stub", Runtime: domain.RuntimeMesh, MeshSubject: subject},
		Message: domain.Activity{Role: "user", Content: json.RawMessage(`{"parts":[{"text":"` + text + `"}]}`)},
	}
}

// TestDispatch_PiStyleReplyEnvelope proves the reply_to path: the stub acks
// the request and publishes a single answer envelope to the reply_to header
// subject. Also asserts the v2 request envelope shape on the wire.
func TestDispatch_PiStyleReplyEnvelope(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)

	const subject = "agents.prompt.test.pi"
	gotEnv := make(chan map[string]any, 1)
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		var env map[string]any
		_ = json.Unmarshal(m.Data, &env)
		gotEnv <- env
		_ = m.Respond([]byte(`{}`)) // ack
		answer, _ := json.Marshal(map[string]any{
			"messageId": "ans-1", "role": "agent",
			"parts": []map[string]string{{"text": "PONG"}},
		})
		_ = nc.Publish(m.Header.Get("reply_to"), answer)
	})
	if err != nil {
		t.Fatalf("stub subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	c, err := mesh.Dial(natsURL, mesh.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	defer c.Close()

	reply, err := c.Dispatch(context.Background(), turn(subject, "ping"))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(reply.Parts) != 1 || reply.Parts[0].Text != "PONG" {
		t.Errorf("reply = %+v, want one part PONG", reply)
	}

	env := <-gotEnv
	if env["role"] != "user" {
		t.Errorf("envelope role = %v, want user", env["role"])
	}
	if id, _ := env["messageId"].(string); id == "" {
		t.Error("envelope messageId is empty")
	}
	parts, _ := env["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("envelope parts = %v, want 1", env["parts"])
	}
	if p, _ := parts[0].(map[string]any); p["text"] != "ping" {
		t.Errorf("envelope part text = %v, want ping", parts[0])
	}
}

// TestDispatch_ClaudeStyleStreamFrames proves the task-stream path: the stub
// acks, streams role:"agent" message frames, and finishes with a terminal
// state frame (one frame uses task.status.state, the terminal uses
// statusUpdate.state — both shapes appear in the wild).
func TestDispatch_ClaudeStyleStreamFrames(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)

	const subject = "agents.prompt.test.claude"
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		_ = m.Respond([]byte(`{}`)) // ack; answer rides the task stream
		working, _ := json.Marshal(map[string]any{
			"task": map[string]any{"status": map[string]string{"state": "TASK_STATE_WORKING"}},
		})
		_ = nc.Publish("agents.task.stream.t1", working)
		frame1, _ := json.Marshal(map[string]any{
			"kind":    "message",
			"message": map[string]any{"role": "agent", "parts": []map[string]string{{"text": "PO"}}},
		})
		_ = nc.Publish("agents.task.stream.t1", frame1)
		frame2, _ := json.Marshal(map[string]any{
			"kind":    "message",
			"message": map[string]any{"role": "agent", "parts": []map[string]string{{"text": "NG"}}},
		})
		_ = nc.Publish("agents.task.stream.t1", frame2)
		terminal, _ := json.Marshal(map[string]any{
			"statusUpdate": map[string]string{"state": "TASK_STATE_COMPLETED"},
		})
		_ = nc.Publish("agents.task.stream.t1", terminal)
	})
	if err != nil {
		t.Fatalf("stub subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	c, err := mesh.Dial(natsURL, mesh.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	defer c.Close()

	reply, err := c.Dispatch(context.Background(), turn(subject, "ping"))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(reply.Parts) != 1 || reply.Parts[0].Text != "PONG" {
		t.Errorf("reply = %+v, want aggregated PONG", reply)
	}
}

// TestDispatch_ServiceError4xx_Permanent proves Nats-Service-Error-Code
// surfacing: a 400 ack with no answer is a permanent ServiceError.
func TestDispatch_ServiceError4xx_Permanent(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)

	const subject = "agents.prompt.test.err"
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		resp := nats.NewMsg(m.Reply)
		resp.Header.Set("Nats-Service-Error-Code", "400")
		resp.Header.Set("Nats-Service-Error", "bad envelope")
		_ = nc.PublishMsg(resp)
	})
	if err != nil {
		t.Fatalf("stub subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	c, err := mesh.Dial(natsURL, mesh.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	defer c.Close()

	_, err = c.Dispatch(context.Background(), turn(subject, "ping"))
	if err == nil {
		t.Fatal("Dispatch err = nil, want service error")
	}
	if !domain.IsPermanent(err) {
		t.Errorf("err = %v, want permanent", err)
	}
	var se *mesh.ServiceError
	if !errors.As(err, &se) || se.Code != "400" {
		t.Errorf("err = %v, want ServiceError code 400", err)
	}
	if !strings.Contains(err.Error(), "bad envelope") {
		t.Errorf("err = %v, want detail surfaced", err)
	}
}

// TestDispatch_V03AckNoAnswer proves the v0.3 heuristic: an agent that acks
// the request but never opens a v2 task stream or reply envelope yields the
// distinguishable ErrAckedNoV2Answer after the ~3s grace, well before the
// dispatch deadline.
func TestDispatch_V03AckNoAnswer(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)

	const subject = "agents.prompt.test.v03"
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		_ = m.Respond([]byte(`{}`)) // acks, then silence forever
	})
	if err != nil {
		t.Fatalf("stub subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	c, err := mesh.Dial(natsURL, mesh.WithTimeout(30*time.Second))
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	defer c.Close()

	start := time.Now()
	_, err = c.Dispatch(context.Background(), turn(subject, "ping"))
	if !errors.Is(err, mesh.ErrAckedNoV2Answer) {
		t.Fatalf("err = %v, want ErrAckedNoV2Answer", err)
	}
	if !domain.IsPermanent(err) {
		t.Errorf("err = %v, want permanent", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("heuristic took %s, want well under the 30s deadline", elapsed)
	}
}

// TestDispatch_TaskFailed_Permanent proves an explicit TASK_STATE_FAILED with
// no answer text is permanent (straight to the DLQ, no pointless retries).
func TestDispatch_TaskFailed_Permanent(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)

	const subject = "agents.prompt.test.failed"
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		_ = m.Respond([]byte(`{}`))
		terminal, _ := json.Marshal(map[string]any{
			"statusUpdate": map[string]string{"state": "TASK_STATE_FAILED"},
		})
		_ = nc.Publish("agents.task.stream.t2", terminal)
	})
	if err != nil {
		t.Fatalf("stub subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	c, err := mesh.Dial(natsURL, mesh.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	defer c.Close()

	_, err = c.Dispatch(context.Background(), turn(subject, "ping"))
	if err == nil || !domain.IsPermanent(err) {
		t.Fatalf("err = %v, want permanent task-failed error", err)
	}
	if !strings.Contains(err.Error(), "TASK_STATE_FAILED") {
		t.Errorf("err = %v, want TASK_STATE_FAILED named", err)
	}
}

// TestDispatch_NoResponders_Transient proves an offline agent (no responder
// on the subject) is a transient error: the engine may retry it.
func TestDispatch_NoResponders_Transient(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	c, err := mesh.Dial(natsURL, mesh.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	defer c.Close()

	_, err = c.Dispatch(context.Background(), turn("agents.prompt.test.offline", "ping"))
	if err == nil {
		t.Fatal("Dispatch err = nil, want transient error")
	}
	if domain.IsPermanent(err) {
		t.Errorf("err = %v, want transient (offline agents must retry)", err)
	}
}

// TestDispatch_SubjectOutsideAllowlist proves the §8 guard: a mesh subject
// outside the allowlist pattern is rejected permanently before any publish.
func TestDispatch_SubjectOutsideAllowlist(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	c, err := mesh.Dial(natsURL) // default allowlist agents.prompt.>
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	defer c.Close()

	_, err = c.Dispatch(context.Background(), turn("api.runs.start", "ping"))
	if err == nil || !domain.IsPermanent(err) {
		t.Fatalf("err = %v, want permanent allowlist rejection", err)
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("err = %v, want allowlist named", err)
	}
}

// TestPrompt_HistoryPrepend pins the v1 history-in-prompt wire format: a
// short header, "role: text" lines oldest-first, a separator, then the new
// message. No history means the bare message.
func TestPrompt_HistoryPrepend(t *testing.T) {
	tn := turn("agents.prompt.x", "what next?")
	if got := mesh.Prompt(tn); got != "what next?" {
		t.Errorf("bare prompt = %q, want %q", got, "what next?")
	}

	tn.History = []domain.Activity{
		{Role: "user", Content: json.RawMessage(`{"parts":[{"text":"hello"}]}`)},
		{Role: "agent", Content: json.RawMessage(`{"parts":[{"text":"hi "},{"text":"there"}]}`)},
	}
	want := "Conversation so far:\nuser: hello\nagent: hi there\n---\nwhat next?"
	if got := mesh.Prompt(tn); got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}

// TestPrompt_HistoryIsBoundedInBytes pins that an oversized transcript cannot
// push the request past NATS max_payload: each line is cut, and the oldest
// lines drop first while the new message is always kept whole.
func TestPrompt_HistoryIsBoundedInBytes(t *testing.T) {
	text := func(s string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"parts": []map[string]string{{"text": s}}})
		return b
	}
	huge := strings.Repeat("é", 400<<10) // 800 KiB of two-byte runes
	tn := turn("agents.prompt.x", "what next?")
	for i := range 20 {
		tn.History = append(tn.History, domain.Activity{Role: "agent", Content: text(fmt.Sprintf("%d:%s", i, huge))})
	}
	tn.History = append(tn.History, domain.Activity{Role: "user", Content: text("latest")})

	got := mesh.Prompt(tn)
	if len(got) > mesh.MaxPromptHistoryBytes+len("what next?")+64 {
		t.Fatalf("prompt is %d bytes, over the %d history budget", len(got), mesh.MaxPromptHistoryBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("prompt was cut inside a rune")
	}
	if !strings.HasSuffix(got, "user: latest\n---\nwhat next?") {
		t.Errorf("newest history line or the message was dropped: ...%q", got[max(0, len(got)-80):])
	}
	if strings.Contains(got, "agent: 0:") {
		t.Error("oldest line kept while newer ones were dropped")
	}
	if !strings.Contains(got, "agent: 19:") {
		t.Error("newest oversized line dropped instead of cut")
	}
}

// TestSubjectAllowed pins the NATS wildcard matcher semantics.
func TestSubjectAllowed(t *testing.T) {
	cases := []struct {
		pattern, subject string
		want             bool
	}{
		{"agents.prompt.>", "agents.prompt.cc.dan.lead", true},
		{"agents.prompt.>", "agents.prompt.x", true},
		{"agents.prompt.>", "agents.prompt", false},
		{"agents.prompt.>", "api.runs.start", false},
		{"agents.*.x", "agents.prompt.x", true},
		{"agents.*.x", "agents.prompt.y", false},
		{"agents.prompt.x", "agents.prompt.x", true},
		{"agents.prompt.x", "agents.prompt.x.y", false},
		{"agents.prompt.>", "", false},
		{"", "agents.prompt.x", false},
	}
	for _, c := range cases {
		if got := mesh.SubjectAllowed(c.pattern, c.subject); got != c.want {
			t.Errorf("SubjectAllowed(%q, %q) = %v, want %v", c.pattern, c.subject, got, c.want)
		}
	}
}

// stubAgent acks every prompt on subject and answers via reply_to with
// answer(n, env), where n counts requests from 1. It returns the envelopes
// received, in order.
func stubAgent(t *testing.T, nc *nats.Conn, subject string, answer func(n int, env map[string]any) map[string]any) func() []map[string]any {
	t.Helper()
	var (
		mu   sync.Mutex
		envs []map[string]any
	)
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		var env map[string]any
		_ = json.Unmarshal(m.Data, &env)
		mu.Lock()
		envs = append(envs, env)
		n := len(envs)
		mu.Unlock()
		_ = m.Respond([]byte(`{}`))
		out, _ := json.Marshal(answer(n, env))
		_ = nc.Publish(m.Header.Get("reply_to"), out)
	})
	if err != nil {
		t.Fatalf("stub subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), envs...)
	}
}

func pong(int, map[string]any) map[string]any {
	return map[string]any{"messageId": "ans", "role": "agent", "parts": []map[string]string{{"text": "PONG"}}}
}

func needHistory(int, map[string]any) map[string]any {
	return map[string]any{
		"messageId": "ans", "role": "agent", "contextId": "c1", "parts": []any{},
		"metadata": map[string]any{"historyRequired": true},
	}
}

func partText(env map[string]any) string {
	parts, _ := env["parts"].([]any)
	if len(parts) != 1 {
		return fmt.Sprintf("<%d parts>", len(parts))
	}
	p, _ := parts[0].(map[string]any)
	s, _ := p["text"].(string)
	return s
}

func metaOf(env map[string]any) map[string]any {
	m, _ := env["metadata"].(map[string]any)
	return m
}

func historyTurn(subject string) domain.Turn {
	tn := turn(subject, "next")
	tn.Conversation = domain.Conversation{ID: "conv-1", SubscriberID: "sub-1"}
	tn.Message.ID = "act-1"
	tn.History = []domain.Activity{{Role: "user", Content: json.RawMessage(`{"parts":[{"text":"hello"}]}`)}}
	return tn
}

func dialShort(t *testing.T, natsURL string) *mesh.Client {
	t.Helper()
	c, err := mesh.Dial(natsURL, mesh.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// TestDispatch_TurnFields pins the A2A turn fields: messageId is the user
// activity id and survives a second dispatch of the same turn, contextId is
// the conversation id, subscriberId rides in metadata, and the default
// history setting pastes the transcript.
func TestDispatch_TurnFields(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.test.fields"
	got := stubAgent(t, nc, subject, pong)
	c := dialShort(t, natsURL)

	tn := historyTurn(subject)
	for i := 0; i < 2; i++ {
		if _, err := c.Dispatch(context.Background(), tn); err != nil {
			t.Fatalf("Dispatch %d: %v", i, err)
		}
	}
	envs := got()
	if len(envs) != 2 {
		t.Fatalf("got %d envelopes, want 2", len(envs))
	}
	for _, env := range envs {
		if env["messageId"] != "act-1" || env["contextId"] != "conv-1" {
			t.Errorf("ids = %v/%v, want act-1/conv-1", env["messageId"], env["contextId"])
		}
		if m := metaOf(env); m["history"] != "inline" || m["subscriberId"] != "sub-1" {
			t.Errorf("metadata = %v", m)
		}
		if want := mesh.Prompt(tn); partText(env) != want {
			t.Errorf("text = %q, want %q", partText(env), want)
		}
	}

	// Unknown subscriber is omitted from the wire.
	tn.Conversation.SubscriberID = ""
	if _, err := c.Dispatch(context.Background(), tn); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if m := metaOf(got()[2]); m["subscriberId"] != nil {
		t.Errorf("subscriberId = %v, want omitted", m["subscriberId"])
	}
}

// TestDispatch_HistoryNoneSendsOnlyMessage pins history none: the envelope
// carries just the new message text and metadata.history:"none".
func TestDispatch_HistoryNoneSendsOnlyMessage(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.test.none"
	got := stubAgent(t, nc, subject, pong)
	c := dialShort(t, natsURL)

	tn := historyTurn(subject)
	tn.Agent.History = domain.HistoryNone
	reply, err := c.Dispatch(context.Background(), tn)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(reply.Parts) != 1 || reply.Parts[0].Text != "PONG" {
		t.Errorf("reply = %+v", reply)
	}
	envs := got()
	if len(envs) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(envs))
	}
	if partText(envs[0]) != "next" || metaOf(envs[0])["history"] != "none" {
		t.Errorf("envelope = %v, want bare text and history none", envs[0])
	}
}

// TestDispatch_HistoryRequiredResendsOnceInline pins the no-state handshake:
// the empty historyRequired reply is never delivered; the turn is resent once
// with the same ids, inline.
func TestDispatch_HistoryRequiredResendsOnceInline(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.test.resend"
	got := stubAgent(t, nc, subject, func(n int, env map[string]any) map[string]any {
		if n == 1 {
			return needHistory(n, env)
		}
		return pong(n, env)
	})
	c := dialShort(t, natsURL)

	tn := historyTurn(subject)
	tn.Agent.History = domain.HistoryNone
	reply, err := c.Dispatch(context.Background(), tn)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(reply.Parts) != 1 || reply.Parts[0].Text != "PONG" {
		t.Errorf("reply = %+v, want PONG", reply)
	}
	envs := got()
	if len(envs) != 2 {
		t.Fatalf("got %d envelopes, want exactly 2", len(envs))
	}
	if metaOf(envs[0])["history"] != "none" || metaOf(envs[1])["history"] != "inline" {
		t.Errorf("histories = %v, %v", metaOf(envs[0]), metaOf(envs[1]))
	}
	if partText(envs[1]) != mesh.Prompt(tn) {
		t.Errorf("resend text = %q, want pasted transcript", partText(envs[1]))
	}
	for _, env := range envs {
		if env["messageId"] != "act-1" || env["contextId"] != "conv-1" {
			t.Errorf("resend changed ids: %v", env)
		}
	}
}

// TestDispatch_HistoryRequiredTwicePermanent pins that a second
// historyRequired reply ends in a permanent error, after exactly two sends.
func TestDispatch_HistoryRequiredTwicePermanent(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.test.twice"
	got := stubAgent(t, nc, subject, needHistory)
	c := dialShort(t, natsURL)

	tn := historyTurn(subject)
	tn.Agent.History = domain.HistoryNone
	_, err := c.Dispatch(context.Background(), tn)
	if err == nil || !domain.IsPermanent(err) {
		t.Fatalf("err = %v, want permanent", err)
	}
	if n := len(got()); n != 2 {
		t.Errorf("got %d envelopes, want 2", n)
	}
}
