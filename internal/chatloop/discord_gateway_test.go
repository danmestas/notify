package chatloop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/danmestas/notify/internal/domain"
)

// discordGatewayFixture spins up an in-process Discord gateway (HELLO →
// IDENTIFY → READY → the supplied MESSAGE_CREATE frames) and runs one
// discordGatewaySession against it. The svc is built with the BOT-SELF id as
// the READY user, so guild-mention gating can be exercised. mutate lets a test
// tweak the service (e.g. enable respond-all) before the session runs.
func discordGatewayFixture(t *testing.T, frames []map[string]any, mutate func(*Service)) *recordingResponder {
	t.Helper()

	wsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "done")
		ctx := r.Context()

		hello, _ := json.Marshal(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
		if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
			return
		}
		if _, _, err := c.Read(ctx); err != nil { // IDENTIFY
			return
		}

		ready, _ := json.Marshal(map[string]any{
			"op": 0, "t": "READY", "s": 1,
			"d": map[string]any{"user": map[string]any{"id": "BOT-SELF"}},
		})
		_ = c.Write(ctx, websocket.MessageText, ready)

		for i, f := range frames {
			frame := map[string]any{"op": 0, "t": "MESSAGE_CREATE", "s": i + 2, "d": f}
			b, _ := json.Marshal(frame)
			_ = c.Write(ctx, websocket.MessageText, b)
		}

		<-ctx.Done()
	})

	wsSrv := httptest.NewServer(wsHandler)
	t.Cleanup(wsSrv.Close)
	wsURL := "ws" + wsSrv.URL[len("http"):]

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"url":"` + wsURL + `"}`))
	}))
	t.Cleanup(apiSrv.Close)

	resp := &recordingResponder{}
	core := openTempStore(t)
	svc := New(core, resp, "", "disc-bot-tok", "support", nil).
		withDiscordAPIBase(apiSrv.URL)
	if mutate != nil {
		mutate(svc)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		_ = svc.discordGatewaySession(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"})
		close(done)
	}()

	// Frames are emitted in order right after READY, so once the connection is
	// up the dispatch is deterministic. Let the loop drain the queued frames,
	// then settle and tear down.
	select {
	case <-time.After(400 * time.Millisecond):
	case <-done:
	}
	cancel()
	<-done
	return resp
}

// A guild message (guild_id set) that does NOT mention the bot is dropped.
func TestDiscordGateway_GuildNonMention_Dropped(t *testing.T) {
	resp := discordGatewayFixture(t, []map[string]any{
		{
			"channel_id": "chan-1", "guild_id": "guild-1", "content": "just chatting",
			"author":   map[string]any{"id": "USER-42", "bot": false},
			"mentions": []any{},
		},
	}, nil)

	if resp.count() != 0 {
		t.Fatalf("expected guild non-mention to be dropped, got %d dispatches", resp.count())
	}
}

// A guild message that mentions the bot is routed with the mention stripped.
func TestDiscordGateway_GuildMention_RoutedAndStripped(t *testing.T) {
	resp := discordGatewayFixture(t, []map[string]any{
		{
			"channel_id": "chan-1", "guild_id": "guild-1",
			"content":  "<@BOT-SELF> hello bot",
			"author":   map[string]any{"id": "USER-42", "bot": false},
			"mentions": []any{map[string]any{"id": "BOT-SELF"}},
		},
	}, nil)

	if resp.count() != 1 {
		t.Fatalf("expected mentioned guild message to route, got %d dispatches", resp.count())
	}
	turn, _ := resp.last()
	if turn.Text != "hello bot" {
		t.Fatalf("text = %q, want %q (mention stripped)", turn.Text, "hello bot")
	}
	if turn.SubscriberID != "dc:USER-42" {
		t.Fatalf("subscriber = %q, want dc:USER-42", turn.SubscriberID)
	}
}

// The nickname mention form <@!botid> is also routed and stripped.
func TestDiscordGateway_GuildNicknameMention_RoutedAndStripped(t *testing.T) {
	resp := discordGatewayFixture(t, []map[string]any{
		{
			"channel_id": "chan-1", "guild_id": "guild-1",
			"content":  "<@!BOT-SELF> ping",
			"author":   map[string]any{"id": "USER-42", "bot": false},
			"mentions": []any{map[string]any{"id": "BOT-SELF"}},
		},
	}, nil)

	if resp.count() != 1 {
		t.Fatalf("expected nickname-mentioned guild message to route, got %d", resp.count())
	}
	turn, _ := resp.last()
	if turn.Text != "ping" {
		t.Fatalf("text = %q, want %q (nickname mention stripped)", turn.Text, "ping")
	}
}

// A DM (no guild_id) is routed regardless of mention.
func TestDiscordGateway_DM_RoutedWithoutMention(t *testing.T) {
	resp := discordGatewayFixture(t, []map[string]any{
		{
			"channel_id": "dm-1", "content": "hey there",
			"author":   map[string]any{"id": "USER-42", "bot": false},
			"mentions": []any{},
		},
	}, nil)

	if resp.count() != 1 {
		t.Fatalf("expected DM to route without mention, got %d", resp.count())
	}
	turn, _ := resp.last()
	if turn.Text != "hey there" {
		t.Fatalf("text = %q, want %q", turn.Text, "hey there")
	}
}

// NOTIFY_DISCORD_RESPOND_ALL override routes a non-mention guild message.
func TestDiscordGateway_RespondAllOverride_RoutesGuildNonMention(t *testing.T) {
	resp := discordGatewayFixture(t, []map[string]any{
		{
			"channel_id": "chan-1", "guild_id": "guild-1", "content": "open channel msg",
			"author":   map[string]any{"id": "USER-42", "bot": false},
			"mentions": []any{},
		},
	}, func(s *Service) { s.withDiscordRespondAll(true) })

	if resp.count() != 1 {
		t.Fatalf("expected respond-all override to route guild non-mention, got %d", resp.count())
	}
	turn, _ := resp.last()
	if turn.Text != "open channel msg" {
		t.Fatalf("text = %q, want %q", turn.Text, "open channel msg")
	}
}

// Bot-authored messages and the bot's own echoes are still ignored, even in a
// guild channel where they "mention" the bot.
func TestDiscordGateway_BotAndSelfMessages_Ignored(t *testing.T) {
	resp := discordGatewayFixture(t, []map[string]any{
		{ // another bot, mentions us — still ignored
			"channel_id": "chan-1", "guild_id": "guild-1", "content": "<@BOT-SELF> hi",
			"author":   map[string]any{"id": "OTHER-BOT", "bot": true},
			"mentions": []any{map[string]any{"id": "BOT-SELF"}},
		},
		{ // our own echo — ignored
			"channel_id": "chan-1", "guild_id": "guild-1", "content": "my own msg",
			"author":   map[string]any{"id": "BOT-SELF", "bot": true},
			"mentions": []any{},
		},
	}, nil)

	if resp.count() != 0 {
		t.Fatalf("expected bot/self messages to be ignored, got %d", resp.count())
	}
}
