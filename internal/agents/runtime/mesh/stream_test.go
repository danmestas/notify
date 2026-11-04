package mesh_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/dagtest"
)

// recSink records notes and the final state a runtime reports.
type recSink struct {
	mu    sync.Mutex
	n     []string
	state string
	usage string
}

func (s *recSink) Note(t string) { s.mu.Lock(); s.n = append(s.n, t); s.mu.Unlock() }
func (s *recSink) Final(state string, usage json.RawMessage) {
	s.mu.Lock()
	s.state, s.usage = state, string(usage)
	s.mu.Unlock()
}

func (s *recSink) notes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.n...)
}

func statusFrame(state, note string, meta any) map[string]any {
	st := map[string]any{"state": state}
	if note != "" {
		st["message"] = map[string]any{"kind": "message", "parts": []map[string]string{{"kind": "text", "text": note}}}
	}
	return map[string]any{"kind": "status-update", "taskId": "r1", "contextId": "c", "status": st, "metadata": meta}
}

// TestDispatch_StreamFramesFeedTheSink proves the collector sends
// metadata.stream, routes status frames to the sink in order, ignores artifact
// frames, and takes the final message (not the first frame) as the answer.
func TestDispatch_StreamFramesFeedTheSink(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.stub.t.stream"
	var (
		mu   sync.Mutex
		seen []map[string]any
	)
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		var env map[string]any
		_ = json.Unmarshal(m.Data, &env)
		mu.Lock()
		seen = append(seen, env)
		mu.Unlock()
		_ = m.Respond([]byte(`{}`))
		pub := func(v any) {
			b, _ := json.Marshal(v)
			_ = nc.Publish(m.Header.Get("reply_to"), b)
		}
		pub(statusFrame("working", "Running search_vault: q", nil))
		pub(statusFrame("working", "Retrying model call", nil))
		pub(map[string]any{"kind": "artifact-update", "artifact": map[string]any{"artifactId": "answer", "parts": []map[string]string{{"text": "PARTIAL"}}}})
		pub(map[string]any{"kind": "artifact-update", "lastChunk": true, "artifact": map[string]any{"artifactId": "answer"}})
		pub(statusFrame("completed", "", map[string]any{"usage": map[string]any{"tokens": 9}}))
		pub(map[string]any{"messageId": "ans", "role": "agent", "parts": []map[string]string{{"text": "FINAL"}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	c := dialShort(t, natsURL)

	sink := &recSink{}
	reply, err := c.Dispatch(domain.WithStatusSink(context.Background(), sink), turn(subject, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Parts) != 1 || reply.Parts[0].Text != "FINAL" {
		t.Errorf("reply = %+v, want FINAL", reply.Parts)
	}
	if got := strings.Join(sink.notes(), "|"); got != "Running search_vault: q|Retrying model call" {
		t.Errorf("notes = %q", got)
	}
	if sink.state != "completed" || sink.usage != `{"tokens":9}` {
		t.Errorf("final = %q %s", sink.state, sink.usage)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || metaOf(seen[0])["stream"] != true {
		t.Errorf("request metadata = %v", seen)
	}
}

// TestDispatch_AgentIgnoringStream proves an agent that sends only the final
// message still works, with no notes.
func TestDispatch_AgentIgnoringStream(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.stub.t.nostream"
	stubAgent(t, nc, subject, pong)
	c := dialShort(t, natsURL)
	sink := &recSink{}
	reply, err := c.Dispatch(domain.WithStatusSink(context.Background(), sink), turn(subject, "hi"))
	if err != nil || reply.Parts[0].Text != "PONG" {
		t.Fatalf("reply %+v, %v", reply, err)
	}
	if len(sink.notes()) != 0 {
		t.Errorf("notes = %v", sink.notes())
	}
}

// TestDispatch_HistoryRequiredResendNoted proves the history resend shows up
// as a working note.
func TestDispatch_HistoryRequiredResendNoted(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.stub.t.resend"
	stubAgent(t, nc, subject, func(n int, env map[string]any) map[string]any {
		if n == 1 {
			return needHistory(n, env)
		}
		return pong(n, env)
	})
	c := dialShort(t, natsURL)
	tn := historyTurn(subject)
	tn.Agent.History = domain.HistoryNone
	sink := &recSink{}
	if _, err := c.Dispatch(domain.WithStatusSink(context.Background(), sink), tn); err != nil {
		t.Fatal(err)
	}
	if got := sink.notes(); len(got) != 1 || !strings.Contains(got[0], "Resending") {
		t.Errorf("notes = %v", got)
	}
}
