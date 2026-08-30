package chatloop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/danmestas/notify/internal/domain"
)

// recordingResponder counts handleInbound dispatches by observing the turns the
// responder receives (handleInbound calls Respond on every accepted message).
type recordingResponder struct {
	mu    sync.Mutex
	turns []InboundTurn
}

func (r *recordingResponder) Respond(ctx context.Context, turn InboundTurn) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns = append(r.turns, turn)
	return turn.ConversationID, nil
}

func (r *recordingResponder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.turns)
}

func (r *recordingResponder) last() (InboundTurn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.turns) == 0 {
		return InboundTurn{}, false
	}
	return r.turns[len(r.turns)-1], true
}

func TestTelegramPoll_FeedsTextIgnoresBotAndAdvancesOffset(t *testing.T) {
	var offsets []string
	var mu sync.Mutex
	var served atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		offsets = append(offsets, r.URL.Query().Get("offset"))
		mu.Unlock()
		n := served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// One human text message (update 10) + one bot message (update 11).
			_, _ = w.Write([]byte(`{"ok":true,"result":[
				{"update_id":10,"message":{"text":"hi there","chat":{"id":555},"from":{"id":7,"is_bot":false}}},
				{"update_id":11,"message":{"text":"i am a bot","chat":{"id":555},"from":{"id":9,"is_bot":true}}}
			]}`))
			return
		}
		// Subsequent polls: empty (loop idles).
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()

	resp := &recordingResponder{}
	core := openTempStore(t)
	svc := New(core, resp, "bot-tok", "", "support", nil).
		withTelegramBaseURL(srv.URL).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		svc.runTelegramPoll(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"})
		close(done)
	}()

	// Wait until the human message is processed and at least one empty poll has
	// been served (so the offset advanced to 12).
	deadline := time.After(3 * time.Second)
	for resp.count() < 1 || served.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out: composes=%d served=%d", resp.count(), served.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if resp.count() != 1 {
		t.Fatalf("expected exactly 1 inbound (bot message ignored), got %d", resp.count())
	}
	turn, _ := resp.last()
	if turn.Text != "hi there" {
		t.Fatalf("inbound text = %q, want %q", turn.Text, "hi there")
	}
	if turn.SubscriberID != "tg:7" {
		t.Fatalf("subscriber = %q, want tg:7", turn.SubscriberID)
	}

	// First poll has no offset; a later poll must carry offset=12 (last id 11 +1).
	mu.Lock()
	defer mu.Unlock()
	if len(offsets) < 2 {
		t.Fatalf("expected >=2 polls, got %d", len(offsets))
	}
	if offsets[0] != "" {
		t.Fatalf("first poll offset = %q, want empty", offsets[0])
	}
	var sawAdvanced bool
	for _, o := range offsets[1:] {
		if o == "12" {
			sawAdvanced = true
		}
	}
	if !sawAdvanced {
		t.Fatalf("offset never advanced to 12; saw %v", offsets)
	}
}

// fakeGatewayServer is an in-process Discord gateway: it sends HELLO, waits for
// IDENTIFY, then emits the queued MESSAGE_CREATE frames.
func TestDiscordGateway_DispatchesMessageCreateAndIdentifies(t *testing.T) {
	type capture struct {
		mu            sync.Mutex
		identifyToken string
		identifyInt   int64
		gotIdentify   bool
	}
	cap := &capture{}

	// The fake gateway WS server.
	wsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "done")
		ctx := r.Context()

		// HELLO with a long heartbeat so the test isn't flooded.
		hello, _ := json.Marshal(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
		if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
			return
		}

		// Expect IDENTIFY (op 2).
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var idf struct {
			Op int `json:"op"`
			D  struct {
				Token   string `json:"token"`
				Intents int64  `json:"intents"`
			} `json:"d"`
		}
		_ = json.Unmarshal(data, &idf)
		cap.mu.Lock()
		cap.gotIdentify = idf.Op == 2
		cap.identifyToken = idf.D.Token
		cap.identifyInt = idf.D.Intents
		cap.mu.Unlock()

		// READY names our bot user id.
		ready, _ := json.Marshal(map[string]any{
			"op": 0, "t": "READY", "s": 1,
			"d": map[string]any{"user": map[string]any{"id": "BOT-SELF"}},
		})
		_ = c.Write(ctx, websocket.MessageText, ready)

		// Our own echo: must be ignored (author.id == bot self).
		ownEcho, _ := json.Marshal(map[string]any{
			"op": 0, "t": "MESSAGE_CREATE", "s": 2,
			"d": map[string]any{
				"channel_id": "chan-1", "content": "my own msg",
				"author": map[string]any{"id": "BOT-SELF", "bot": true},
			},
		})
		_ = c.Write(ctx, websocket.MessageText, ownEcho)

		// A real human message: must be dispatched.
		human, _ := json.Marshal(map[string]any{
			"op": 0, "t": "MESSAGE_CREATE", "s": 3,
			"d": map[string]any{
				"channel_id": "chan-1", "content": "hello bot",
				"author": map[string]any{"id": "USER-42", "bot": false},
			},
		})
		_ = c.Write(ctx, websocket.MessageText, human)

		// Keep the connection open until the client cancels.
		<-ctx.Done()
	})

	// The /gateway bootstrap server points the loop at our WS server.
	wsSrv := httptest.NewServer(wsHandler)
	defer wsSrv.Close()
	wsURL := "ws" + wsSrv.URL[len("http"):] // http->ws, https->wss

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"url":"` + wsURL + `"}`))
	}))
	defer apiSrv.Close()

	resp := &recordingResponder{}
	core := openTempStore(t)
	svc := New(core, resp, "", "disc-bot-tok", "support", nil).
		withDiscordAPIBase(apiSrv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = svc.discordGatewaySession(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"})
		close(done)
	}()

	deadline := time.After(4 * time.Second)
	for resp.count() < 1 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for MESSAGE_CREATE dispatch (count=%d)", resp.count())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if resp.count() != 1 {
		t.Fatalf("expected exactly 1 dispatch (own echo ignored), got %d", resp.count())
	}
	turn, _ := resp.last()
	if turn.Text != "hello bot" {
		t.Fatalf("dispatched text = %q, want %q", turn.Text, "hello bot")
	}
	if turn.SubscriberID != "dc:USER-42" {
		t.Fatalf("subscriber = %q, want dc:USER-42", turn.SubscriberID)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if !cap.gotIdentify {
		t.Fatalf("server never received IDENTIFY (op 2)")
	}
	if cap.identifyToken != "disc-bot-tok" {
		t.Fatalf("identify token = %q, want disc-bot-tok", cap.identifyToken)
	}
	wantIntents := int64((1 << 9) | (1 << 12) | (1 << 15))
	if cap.identifyInt != wantIntents {
		t.Fatalf("identify intents = %d, want %d", cap.identifyInt, wantIntents)
	}
}
