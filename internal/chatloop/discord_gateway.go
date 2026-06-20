package chatloop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/danmestas/notify/internal/domain"
)

// Discord Gateway v10 opcodes (the subset the loop uses).
const (
	dcOpDispatch  = 0  // an event (t names it, e.g. MESSAGE_CREATE)
	dcOpHeartbeat = 1  // client → server heartbeat (carries last seq)
	dcOpIdentify  = 2  // client → server identify (token + intents)
	dcOpHello     = 10 // server → client on connect (heartbeat_interval)
)

// Gateway intents the loop needs to receive guild + DM message content:
// GUILD_MESSAGES(1<<9) | DIRECT_MESSAGES(1<<12) | MESSAGE_CONTENT(1<<15).
const discordIntents = (1 << 9) | (1 << 12) | (1 << 15)

// gatewayConn is the minimal WS connection surface the gateway loop needs,
// satisfied by *websocket.Conn. It lets tests substitute an in-process server.
type gatewayConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(code websocket.StatusCode, reason string) error
}

// gatewayDialer opens a gateway WS connection to url. The default dials
// Discord's real gateway; tests inject one that dials an in-process server.
type gatewayDialer func(ctx context.Context, url string) (gatewayConn, error)

// defaultGatewayDialer dials a real Discord gateway URL with coder/websocket.
func defaultGatewayDialer(hc *http.Client) gatewayDialer {
	return func(ctx context.Context, url string) (gatewayConn, error) {
		opts := &websocket.DialOptions{}
		if hc != nil {
			opts.HTTPClient = hc
		}
		c, resp, err := websocket.Dial(ctx, url, opts)
		// websocket.Dial returns the handshake HTTP response; its body is
		// already drained+closed by the library on success, but on failure (and
		// defensively) we close it so a non-101 response body never leaks.
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// dcGatewayURL is the GET /gateway response shape.
type dcGatewayURL struct {
	URL string `json:"url"`
}

// dcPayload is a Discord gateway frame. D and T are event-specific.
type dcPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// dcHello is the op-10 HELLO data.
type dcHello struct {
	HeartbeatInterval int64 `json:"heartbeat_interval"`
}

// dcReady is the READY dispatch data (only our bot user id is needed, to skip
// our own MESSAGE_CREATE echoes).
type dcReady struct {
	User struct {
		ID string `json:"id"`
	} `json:"user"`
}

// dcMessageCreate is the MESSAGE_CREATE dispatch data. GuildID is absent for
// DMs (Discord omits it). Mentions lists the users explicitly @-mentioned in
// the message; we use it to gate guild messages to direct mentions of the bot.
type dcMessageCreate struct {
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
	GuildID   string `json:"guild_id"`
	Author    struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	} `json:"author"`
	Mentions []dcMention `json:"mentions"`
}

// dcMention is one entry in a MESSAGE_CREATE mentions array (a user object; we
// only need the id to detect a mention of the bot).
type dcMention struct {
	ID string `json:"id"`
}

// runDiscordGateway connects the gateway and processes events until ctx is
// cancelled, reconnecting with backoff on disconnect. RESUME is a later slice;
// v1 reconnects fresh (re-IDENTIFY), accepting the small replay window.
func (s *Service) runDiscordGateway(ctx context.Context, scope domain.Scope) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.discordGatewaySession(ctx, scope); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("chatloop: discord gateway session ended", "err", err)
			if !sleepCtx(ctx, 3*time.Second) {
				return
			}
		}
	}
}

// discordGatewaySession runs one connect→identify→dispatch loop. It returns
// when the connection drops or ctx is cancelled.
func (s *Service) discordGatewaySession(ctx context.Context, scope domain.Scope) error {
	wsURL, err := s.discordGatewayURL(ctx)
	if err != nil {
		return fmt.Errorf("resolve gateway url: %w", err)
	}

	dialer := s.discordDialer
	if dialer == nil {
		dialer = defaultGatewayDialer(s.httpClient)
	}
	conn, err := dialer(ctx, wsURL)
	if err != nil {
		return fmt.Errorf("dial gateway: %w", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "bye") }()

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// First frame must be HELLO (op 10) carrying the heartbeat interval.
	_, data, err := conn.Read(sessCtx)
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	var hello dcPayload
	if err := json.Unmarshal(data, &hello); err != nil {
		return fmt.Errorf("decode hello frame: %w", err)
	}
	if hello.Op != dcOpHello {
		return fmt.Errorf("expected HELLO op %d, got op %d", dcOpHello, hello.Op)
	}
	var h dcHello
	_ = json.Unmarshal(hello.D, &h)

	// IDENTIFY (op 2) with token + intents.
	identify := dcPayload{Op: dcOpIdentify}
	identify.D, _ = json.Marshal(map[string]any{
		"token":   s.discordToken,
		"intents": discordIntents,
		"properties": map[string]string{
			"os":      "linux",
			"browser": "notify",
			"device":  "notify",
		},
	})
	idBytes, _ := json.Marshal(identify)
	if err := conn.Write(sessCtx, websocket.MessageText, idBytes); err != nil {
		return fmt.Errorf("write identify: %w", err)
	}

	// Heartbeat loop: send op-1 with the last seq at heartbeat_interval.
	var seq seqHolder
	if h.HeartbeatInterval > 0 {
		go s.discordHeartbeat(sessCtx, conn, time.Duration(h.HeartbeatInterval)*time.Millisecond, &seq)
	}

	var botUserID string
	for {
		_, data, err := conn.Read(sessCtx)
		if err != nil {
			return fmt.Errorf("read frame: %w", err)
		}
		var p dcPayload
		if err := json.Unmarshal(data, &p); err != nil {
			s.log.Warn("chatloop: discord decode frame", "err", err)
			continue
		}
		if p.S != nil {
			seq.setSeq(*p.S)
		}
		if p.Op != dcOpDispatch {
			continue
		}
		switch p.T {
		case "READY":
			var rdy dcReady
			if err := json.Unmarshal(p.D, &rdy); err == nil {
				botUserID = rdy.User.ID
			}
		case "MESSAGE_CREATE":
			var m dcMessageCreate
			if err := json.Unmarshal(p.D, &m); err != nil {
				continue
			}
			// Ignore bot-authored messages and our own echoes.
			if m.Author.Bot || (botUserID != "" && m.Author.ID == botUserID) {
				continue
			}
			if m.Content == "" {
				continue
			}
			// Gate guild-channel messages to direct @-mentions of the bot so the
			// loop does not reply to every line in a shared channel. DMs (no
			// guild_id) are a direct conversation with the bot, so they always
			// route. NOTIFY_DISCORD_RESPOND_ALL restores respond-to-all for a
			// dedicated single-purpose channel.
			text := m.Content
			if m.GuildID != "" && !s.discordRespondAll {
				if !discordMentionsBot(m.Mentions, botUserID) {
					continue
				}
				text = stripDiscordMention(text, botUserID)
			}
			if text == "" {
				continue
			}
			subscriberID := "dc:" + m.Author.ID
			s.handleInbound(sessCtx, scope, providerDiscord, m.ChannelID, subscriberID, text)
		}
	}
}

// discordGatewayURL fetches the gateway WS URL via GET {api}/gateway and appends
// the v10 + json-encoding query. Discord's real endpoint returns a wss:// host;
// a test server returns its own ws:// URL.
func (s *Service) discordGatewayURL(ctx context.Context) (string, error) {
	base := s.discordAPIBase
	if base == "" {
		base = discordAPIBaseURL
	}
	hc := s.httpClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/gateway", nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gateway url status %d", resp.StatusCode)
	}
	var g dcGatewayURL
	if err := json.Unmarshal(body, &g); err != nil {
		return "", err
	}
	if g.URL == "" {
		return "", fmt.Errorf("empty gateway url")
	}
	sep := "?"
	if strings.Contains(g.URL, "?") {
		sep = "&"
	}
	return g.URL + sep + "v=10&encoding=json", nil
}

// discordHeartbeat sends op-1 heartbeats carrying the last seq at the interval
// until ctx is cancelled.
func (s *Service) discordHeartbeat(ctx context.Context, conn gatewayConn, interval time.Duration, seq *seqHolder) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			hb := dcPayload{Op: dcOpHeartbeat}
			if last, ok := seq.getSeq(); ok {
				hb.D, _ = json.Marshal(last)
			} else {
				hb.D = json.RawMessage("null")
			}
			frame, _ := json.Marshal(hb)
			if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
				return
			}
		}
	}
}

// discordMentionsBot reports whether the bot's user id appears in the message's
// mentions array. botUserID == "" (READY not yet seen) means we cannot confirm
// a mention, so it returns false (the guild message is dropped).
func discordMentionsBot(mentions []dcMention, botUserID string) bool {
	if botUserID == "" {
		return false
	}
	for _, mn := range mentions {
		if mn.ID == botUserID {
			return true
		}
	}
	return false
}

// stripDiscordMention removes the bot's mention token from content. Discord
// renders a user mention as "<@ID>" or, for a nickname, "<@!ID>". Both forms
// are stripped and surrounding whitespace trimmed; other mentions are left
// intact so the agent receives the clean message.
func stripDiscordMention(content, botUserID string) string {
	if botUserID == "" {
		return content
	}
	out := content
	out = strings.ReplaceAll(out, "<@!"+botUserID+">", "")
	out = strings.ReplaceAll(out, "<@"+botUserID+">", "")
	return strings.TrimSpace(out)
}

// seqHolder is a tiny concurrency-safe holder for the last dispatch sequence.
type seqHolder struct {
	mu  sync.Mutex
	val int64
	set bool
}

func (h *seqHolder) setSeq(v int64) {
	h.mu.Lock()
	h.val = v
	h.set = true
	h.mu.Unlock()
}

func (h *seqHolder) getSeq() (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.val, h.set
}
