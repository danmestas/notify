package chatloop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	corestore "github.com/danmestas/notify/internal/store"
)

// fakeResponder records the turns it is asked to handle and echoes the
// supplied conversation id (handleInbound mints it before dispatch).
type fakeResponder struct {
	mu    sync.Mutex
	turns []InboundTurn
}

func (f *fakeResponder) Respond(ctx context.Context, turn InboundTurn) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, turn)
	return turn.ConversationID, nil
}

func (f *fakeResponder) calls() []InboundTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InboundTurn, len(f.turns))
	copy(out, f.turns)
	return out
}

func openTempStore(t *testing.T) *corestore.Store {
	t.Helper()
	s, err := corestore.Open(context.Background(), t.TempDir()+"/notify.db")
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestHandleInbound_ColdMessageMintsBindingAndComposes(t *testing.T) {
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	resp := &fakeResponder{}
	svc := New(core, resp, "tok", "", "support", nil)

	svc.handleInbound(context.Background(), scope, providerTelegram, "chat-1", "tg:7", "hello")

	calls := resp.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 compose call, got %d", len(calls))
	}
	if calls[0].ConversationID == "" {
		t.Fatalf("cold message must dispatch with a minted conversation id, got empty")
	}
	if calls[0].AgentIdentifier != "support" {
		t.Fatalf("cold message must route to default agent, got %q", calls[0].AgentIdentifier)
	}

	// The binding now exists, keyed on the minted conversation id.
	bind, err := core.ChatThreads().GetByChat(context.Background(), scope, providerTelegram, "chat-1")
	if err != nil {
		t.Fatalf("binding not created: %v", err)
	}
	if bind.PlatformThreadID != calls[0].ConversationID || bind.AgentIdentifier != "support" {
		t.Fatalf("binding mismatch: %+v, want thread %q", bind, calls[0].ConversationID)
	}
}

func TestHandleInbound_SecondMessageThreadsSameConversation(t *testing.T) {
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	resp := &fakeResponder{}
	svc := New(core, resp, "tok", "", "support", nil)

	svc.handleInbound(context.Background(), scope, providerTelegram, "chat-1", "tg:7", "first")
	svc.handleInbound(context.Background(), scope, providerTelegram, "chat-1", "tg:7", "second")

	calls := resp.calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 compose calls, got %d", len(calls))
	}
	if calls[0].ConversationID == "" {
		t.Fatalf("first call should carry the minted conv id, got empty")
	}
	if calls[1].ConversationID != calls[0].ConversationID {
		t.Fatalf("second call should thread onto the same conversation %q, got %q", calls[0].ConversationID, calls[1].ConversationID)
	}
}

func TestHandleInbound_NoDefaultAgentNoBindingDropped(t *testing.T) {
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	resp := &fakeResponder{}
	svc := New(core, resp, "tok", "", "" /* no default agent */, nil)

	svc.handleInbound(context.Background(), scope, providerTelegram, "chat-1", "tg:7", "hello")

	if n := len(resp.calls()); n != 0 {
		t.Fatalf("unbound cold message with no default agent must drop, got %d compose calls", n)
	}
	if _, err := core.ChatThreads().GetByChat(context.Background(), scope, providerTelegram, "chat-1"); err == nil {
		t.Fatalf("no binding should be created for a dropped message")
	}
}

// captureSend records the request a reply send produced.
type captureSend struct {
	mu   sync.Mutex
	path string
	auth string
	body string
	hits int
}

func (c *captureSend) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.path = r.URL.Path
	c.auth = r.Header.Get("Authorization")
	c.body = string(b)
	c.hits++
}

func TestReplyHook_TelegramHitSendsBack(t *testing.T) {
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	cap := &captureSend{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// Bind a telegram chat_thread for conversation "conv-1".
	if _, err := core.ChatThreads().Upsert(context.Background(), scope, corestore.ChatThread{
		AgentIdentifier:  "support",
		PlatformThreadID: "conv-1",
		SubscriberID:     "tg:7",
		Provider:         providerTelegram,
		ChatID:           "987",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	svc := New(core, &fakeResponder{}, "bot-tok", "", "support", nil).
		withTelegramBaseURL(srv.URL)

	svc.ReplyHook(context.Background(), scope,
		agentdomain.Conversation{PlatformThreadID: "conv-1"}, "the answer")

	if cap.hits != 1 {
		t.Fatalf("expected 1 telegram send, got %d", cap.hits)
	}
	if cap.path != "/bot bot-tok/sendMessage" && cap.path != "/botbot-tok/sendMessage" {
		t.Fatalf("unexpected telegram path %q", cap.path)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(cap.body), &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent["chat_id"] != "987" || sent["text"] != "the answer" {
		t.Fatalf("telegram send payload mismatch: %v", sent)
	}
}

func TestReplyHook_DiscordHitSendsViaBotREST(t *testing.T) {
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	cap := &captureSend{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		_, _ = w.Write([]byte(`{"id":"msg-1"}`))
	}))
	defer srv.Close()

	if _, err := core.ChatThreads().Upsert(context.Background(), scope, corestore.ChatThread{
		AgentIdentifier:  "support",
		PlatformThreadID: "conv-2",
		SubscriberID:     "dc:55",
		Provider:         providerDiscord,
		ChatID:           "channel-9",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	svc := New(core, &fakeResponder{}, "", "disc-bot-tok", "support", nil).
		withDiscordAPIBase(srv.URL)

	svc.ReplyHook(context.Background(), scope,
		agentdomain.Conversation{PlatformThreadID: "conv-2"}, "discord answer")

	if cap.hits != 1 {
		t.Fatalf("expected 1 discord send, got %d", cap.hits)
	}
	if cap.path != "/channels/channel-9/messages" {
		t.Fatalf("unexpected discord path %q", cap.path)
	}
	if cap.auth != "Bot disc-bot-tok" {
		t.Fatalf("discord bot auth header = %q, want %q", cap.auth, "Bot disc-bot-tok")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(cap.body), &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent["content"] != "discord answer" {
		t.Fatalf("discord send payload mismatch: %v", sent)
	}
}

func TestReplyHook_MissNoOps(t *testing.T) {
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	cap := &captureSend{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	svc := New(core, &fakeResponder{}, "bot-tok", "", "support", nil).
		withTelegramBaseURL(srv.URL)

	// No binding for conv-404: ReplyHook must no-op and not send anything.
	svc.ReplyHook(context.Background(), scope,
		agentdomain.Conversation{PlatformThreadID: "conv-404"}, "should not send")

	if cap.hits != 0 {
		t.Fatalf("ReplyHook miss must not send, got %d sends", cap.hits)
	}
}
