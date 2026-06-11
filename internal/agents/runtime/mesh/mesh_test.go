package mesh_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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
	natsURL, _ := dagtest.StartSidecar(t)
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
	natsURL, _ := dagtest.StartSidecar(t)
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
	natsURL, _ := dagtest.StartSidecar(t)
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
	natsURL, _ := dagtest.StartSidecar(t)
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
	natsURL, _ := dagtest.StartSidecar(t)
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
	natsURL, _ := dagtest.StartSidecar(t)

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
	natsURL, _ := dagtest.StartSidecar(t)

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
