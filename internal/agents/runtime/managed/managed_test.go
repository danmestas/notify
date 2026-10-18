package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/secretbox"
)

type fakeClient struct {
	got  aibuilder.MessageRequest
	key  string
	resp aibuilder.MessageResponse
	err  error
}

func (f *fakeClient) Message(_ context.Context, req aibuilder.MessageRequest) (aibuilder.MessageResponse, error) {
	f.got = req
	return f.resp, f.err
}

func act(role, text string) agentdomain.Activity {
	c, _ := json.Marshal(map[string]any{"parts": []map[string]string{{"text": text}}})
	return agentdomain.Activity{Role: role, Content: c}
}

func newExec(t *testing.T, f *fakeClient) (*Executor, *secretbox.Box) {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return New(box, func(_ Config, apiKey string) aibuilder.Client { f.key = apiKey; return f }), box
}

// TestDispatch_OneModelCallFromTheConversation: a turn is one call with the
// agent's model, system prompt and default token limit, the transcript as
// alternating sides opening with the user, and the sealed key opened.
func TestDispatch_OneModelCallFromTheConversation(t *testing.T) {
	f := &fakeClient{resp: aibuilder.MessageResponse{Text: "  Sure, Friday works.  "}}
	exec, box := newExec(t, f)
	sealed, _ := box.Seal("sk-live")
	cfg, _ := json.Marshal(Config{Provider: ProviderOpenAI, BaseURL: "http://localhost:11434/v1", Model: "llama3", SystemPrompt: "Draft replies.", APIKey: sealed})

	reply, err := exec.Dispatch(context.Background(), agentdomain.Turn{
		Agent:   agentdomain.Agent{Managed: cfg},
		History: []agentdomain.Activity{act("agent", "welcome"), act("user", "hi"), act("user", "still there?"), act("agent", "yes")},
		Message: act("user", "Lunch Friday?"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Parts) != 1 || reply.Parts[0].Text != "Sure, Friday works." {
		t.Fatalf("reply = %+v", reply)
	}
	if f.key != "sk-live" || f.got.Model != "llama3" || f.got.System != "Draft replies." || f.got.MaxTokens != defaultMaxTokens {
		t.Fatalf("request = %+v key=%q", f.got, f.key)
	}
	want := []aibuilder.Message{
		{Role: aibuilder.RoleUser, Text: "hi\n\nstill there?"},
		{Role: aibuilder.RoleAssistant, Text: "yes"},
		{Role: aibuilder.RoleUser, Text: "Lunch Friday?"},
	}
	if len(f.got.Messages) != len(want) {
		t.Fatalf("messages = %+v", f.got.Messages)
	}
	for i := range want {
		if f.got.Messages[i].Role != want[i].Role || f.got.Messages[i].Text != want[i].Text {
			t.Fatalf("message %d = %+v, want %+v", i, f.got.Messages[i], want[i])
		}
	}
}

// TestDispatch_BadSettingsArePermanent: settings no retry can fix fail the
// turn permanently; a model error is left transient so the engine retries.
func TestDispatch_BadSettingsArePermanent(t *testing.T) {
	f := &fakeClient{}
	exec, _ := newExec(t, f)
	turn := func(cfg string) agentdomain.Turn {
		return agentdomain.Turn{Agent: agentdomain.Agent{Managed: json.RawMessage(cfg)}, Message: act("user", "hi")}
	}
	for _, cfg := range []string{``, `{"provider":"openai"}`, `{"provider":"bard","model":"x"}`, `{"provider":"openai","model":"x"}`, `{"provider":"openai","baseUrl":"http://h/v1","model":"x","maxTokens":99999}`, `{"provider":"openai","baseUrl":"http://h/v1","model":"x","apiKey":"enc:1:garbage"}`} {
		if _, err := exec.Dispatch(context.Background(), turn(cfg)); !agentdomain.IsPermanent(err) {
			t.Errorf("config %s: err %v, want permanent", cfg, err)
		}
	}
	f.err = errors.New("connection refused")
	if _, err := exec.Dispatch(context.Background(), turn(`{"provider":"openai","baseUrl":"http://h/v1","model":"x"}`)); err == nil || agentdomain.IsPermanent(err) {
		t.Fatalf("model error: %v, want transient", err)
	}
	f.err, f.resp = nil, aibuilder.MessageResponse{Text: "   "}
	if _, err := exec.Dispatch(context.Background(), turn(`{"provider":"openai","baseUrl":"http://h/v1","model":"x"}`)); err == nil {
		t.Fatal("empty model reply accepted")
	}
}
