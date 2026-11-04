package a2areceiver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/a2areceiver"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// fallback is the configured runtime stand-in: it records the turns it gets.
type fallback struct {
	mu    sync.Mutex
	turns []agentdomain.Turn
}

func (f *fallback) Dispatch(_ context.Context, t agentdomain.Turn) (agentdomain.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, t)
	return agentdomain.Reply{Parts: []agentdomain.Part{{Text: "from runtime"}}}, nil
}

func (f *fallback) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, t := range f.turns {
		out = append(out, t.Message.ID)
	}
	return out
}

type fixture struct {
	svc    *a2areceiver.Service
	srv    *httptest.Server
	scope  domain.Scope
	agent  agentdomain.Agent
	other  agentdomain.Agent
	fb     *fallback
	store  *store.Store
	secret string
}

func newFixture(t *testing.T, opts ...func(*a2areceiver.Service)) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	_, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	ags := agentstore.New(st)
	mk := func(ident string) agentdomain.Agent {
		a, err := ags.Agents().Create(ctx, scope, agentdomain.Agent{
			Identifier: ident, Name: ident, Runtime: agentdomain.RuntimeMesh, Active: true,
			MeshSubject: "agents.prompt." + ident,
		})
		if err != nil {
			t.Fatalf("create agent: %v", err)
		}
		return a
	}
	svc := a2areceiver.New(st, ags, nil)
	for _, o := range opts {
		o(svc)
	}
	r := chi.NewRouter()
	r.Get("/a2a/{agentId}/receive", svc.ServeReceive)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	f := &fixture{svc: svc, srv: srv, scope: scope, agent: mk("bot"), other: mk("other"), fb: &fallback{}, store: st}
	_, f.secret, err = svc.Mint(ctx, scope, "bot", "laptop")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return f
}

func (f *fixture) turn(msgID string) agentdomain.Turn {
	return agentdomain.Turn{
		Agent:        f.agent,
		Conversation: agentdomain.Conversation{ID: "conv-1", SubscriberID: "alice"},
		Message:      agentdomain.Activity{ID: msgID, Content: json.RawMessage(`{"parts":[{"text":"hello"}]}`)},
	}
}

func (f *fixture) url(agentID string) string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/a2a/" + agentID + "/receive"
}

func (f *fixture) dial(t *testing.T, secret string) *websocket.Conn {
	t.Helper()
	ws, resp, err := websocket.Dial(context.Background(), f.url(f.agent.ID), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	waitFor(t, "live", func() bool { _, ok := f.svc.Live(f.agent.ID); return ok })
	return ws
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type turnFrame struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

func readTurn(t *testing.T, ws *websocket.Conn) (turnFrame, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read turn: %v", err)
	}
	var tf turnFrame
	if err := json.Unmarshal(data, &tf); err != nil {
		t.Fatalf("decode turn: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(tf.Message, &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	return tf, msg
}

func send(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func answer(t *testing.T, ws *websocket.Conn, replyTo, text string) {
	t.Helper()
	send(t, ws, map[string]any{"kind": "status-update", "replyTo": replyTo, "status": map[string]any{"state": "working"}})
	send(t, ws, map[string]any{"kind": "artifact-update", "replyTo": replyTo})
	send(t, ws, map[string]any{
		"kind": "message", "role": "agent", "replyTo": replyTo, "contextId": "conv-1",
		"parts": []map[string]string{{"text": text}},
	})
}

func TestConnectedReceiverAnswersTurn(t *testing.T) {
	f := newFixture(t)

	// Disconnected: the configured runtime answers.
	reply, err := f.svc.Dispatch(context.Background(), f.turn("act-0"), f.fb)
	if err != nil || reply.Parts[0].Text != "from runtime" {
		t.Fatalf("disconnected dispatch = %+v, %v", reply, err)
	}

	ws := f.dial(t, f.secret)
	type out struct {
		r   agentdomain.Reply
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := f.svc.Dispatch(context.Background(), f.turn("act-1"), f.fb)
		done <- out{r, err}
	}()

	tf, msg := readTurn(t, ws)
	if tf.Type != "turn" || msg["messageId"] != "act-1" || msg["contextId"] != "conv-1" || msg["role"] != "user" {
		t.Fatalf("turn frame = %s", tf.Message)
	}
	if md, _ := msg["metadata"].(map[string]any); md["subscriberId"] != "alice" {
		t.Fatalf("metadata = %v", msg["metadata"])
	}
	answer(t, ws, "act-1", "from harness")

	select {
	case o := <-done:
		if o.err != nil || o.r.Parts[0].Text != "from harness" {
			t.Fatalf("live dispatch = %+v, %v", o.r, o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not return")
	}
	if got := f.fb.ids(); len(got) != 1 || got[0] != "act-0" {
		t.Fatalf("fallback turns = %v, want only act-0", got)
	}

	// Disconnect: the next turn goes back to the runtime.
	_ = ws.Close(websocket.StatusNormalClosure, "bye")
	waitFor(t, "not live", func() bool { _, ok := f.svc.Live(f.agent.ID); return !ok })
	reply, err = f.svc.Dispatch(context.Background(), f.turn("act-2"), f.fb)
	if err != nil || reply.Parts[0].Text != "from runtime" {
		t.Fatalf("post-disconnect dispatch = %+v, %v", reply, err)
	}
}

func TestSeveralTurnsInFlight(t *testing.T) {
	f := newFixture(t)
	ws := f.dial(t, f.secret)
	res := make(chan string, 2)
	for _, id := range []string{"a", "b"} {
		go func() {
			r, err := f.svc.Dispatch(context.Background(), f.turn(id), f.fb)
			if err != nil {
				res <- "err:" + err.Error()
				return
			}
			res <- id + "=" + r.Parts[0].Text
		}()
	}
	_, m1 := readTurn(t, ws)
	_, m2 := readTurn(t, ws)
	// Answer in the opposite order from arrival.
	answer(t, ws, m2["messageId"].(string), "re:"+m2["messageId"].(string))
	answer(t, ws, m1["messageId"].(string), "re:"+m1["messageId"].(string))
	got := map[string]bool{<-res: true, <-res: true}
	if !got["a=re:a"] || !got["b=re:b"] {
		t.Fatalf("replies = %v", got)
	}
}

func TestInFlightTurnRedispatchedOnDisconnect(t *testing.T) {
	f := newFixture(t)
	ws := f.dial(t, f.secret)
	done := make(chan agentdomain.Reply, 1)
	go func() {
		r, _ := f.svc.Dispatch(context.Background(), f.turn("act-9"), f.fb)
		done <- r
	}()
	readTurn(t, ws) // the harness takes the turn, then dies
	_ = ws.CloseNow()
	select {
	case r := <-done:
		if r.Parts[0].Text != "from runtime" {
			t.Fatalf("reply = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn was not redispatched")
	}
	if got := f.fb.ids(); len(got) != 1 || got[0] != "act-9" {
		t.Fatalf("fallback saw %v, want the same messageId act-9", got)
	}
}

func TestSecondConnectionReplacesFirst(t *testing.T) {
	f := newFixture(t)
	first := f.dial(t, f.secret)
	firstSince, _ := f.svc.Live(f.agent.ID)
	time.Sleep(5 * time.Millisecond)
	second := f.dial(t, f.secret)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := first.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Reason != "replaced" {
		t.Fatalf("first conn read err = %v, want close reason \"replaced\"", err)
	}
	waitFor(t, "second is live", func() bool {
		s, ok := f.svc.Live(f.agent.ID)
		return ok && s.After(firstSince)
	})

	done := make(chan agentdomain.Reply, 1)
	go func() {
		r, _ := f.svc.Dispatch(context.Background(), f.turn("act-3"), f.fb)
		done <- r
	}()
	_, msg := readTurn(t, second)
	answer(t, second, msg["messageId"].(string), "second answers")
	if r := <-done; r.Parts[0].Text != "second answers" {
		t.Fatalf("reply = %+v", r)
	}
	if len(f.fb.ids()) != 0 {
		t.Fatalf("fallback should not have run: %v", f.fb.ids())
	}
}

func TestAuthRejections(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, otherSecret, err := f.svc.Mint(ctx, f.scope, "other", "x")
	if err != nil {
		t.Fatal(err)
	}
	revoked, revokedSecret, err := f.svc.Mint(ctx, f.scope, "bot", "old")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Revoke(ctx, f.scope, "bot", revoked.ID); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"no header":      "",
		"wrong agent":    "Bearer " + otherSecret,
		"revoked":        "Bearer " + revokedSecret,
		"garbage":        "Bearer a2arx_zz",
		"wrong prefix":   "Bearer a2ack_" + strings.Repeat("a", 64),
		"not bearer":     "ApiKey " + f.secret,
		"unknown secret": "Bearer a2arx_" + strings.Repeat("0", 64),
	}
	for name, authz := range cases {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/a2a/"+f.agent.ID+"/receive", nil)
			if authz != "" {
				req.Header.Set("Authorization", authz)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
	// The right key still works.
	f.dial(t, f.secret)
}

func TestRevokeDisconnectsLiveReceiver(t *testing.T) {
	f := newFixture(t)
	ws := f.dial(t, f.secret)
	keys, err := f.svc.List(context.Background(), f.scope, "bot")
	if err != nil || len(keys) != 1 {
		t.Fatalf("list = %v, %v", keys, err)
	}
	if keys[0].SecretHash != "" {
		t.Fatal("List leaked the secret hash")
	}
	if err := f.svc.Revoke(context.Background(), f.scope, "bot", keys[0].ID); err != nil {
		t.Fatal(err)
	}
	// Revoke drops the session before the close handshake, so no reply read
	// during it is delivered and new turns fall back at once.
	if _, ok := f.svc.Live(f.agent.ID); ok {
		t.Fatal("receiver still live straight after Revoke")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("expected the socket to close")
	}
	waitFor(t, "not live", func() bool { _, ok := f.svc.Live(f.agent.ID); return !ok })
}

func TestPingTimeoutFallsBack(t *testing.T) {
	f := newFixture(t, func(s *a2areceiver.Service) { s.WithHeartbeat(30*time.Millisecond, 100*time.Millisecond) })
	// The client never reads, so it never answers a ping.
	f.dial(t, f.secret)
	waitFor(t, "ping timeout to drop the receiver", func() bool { _, ok := f.svc.Live(f.agent.ID); return !ok })
	reply, err := f.svc.Dispatch(context.Background(), f.turn("act-5"), f.fb)
	if err != nil || reply.Parts[0].Text != "from runtime" {
		t.Fatalf("dispatch = %+v, %v", reply, err)
	}
}

func TestHeartbeatKeepsAnsweringReceiverConnected(t *testing.T) {
	f := newFixture(t, func(s *a2areceiver.Service) { s.WithHeartbeat(30*time.Millisecond, 500*time.Millisecond) })
	ws := f.dial(t, f.secret)
	go func() { // a reading client answers pings
		for {
			if _, _, err := ws.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	if _, ok := f.svc.Live(f.agent.ID); !ok {
		t.Fatal("responsive receiver was dropped")
	}
}

func TestNoAnswerTimesOutTransiently(t *testing.T) {
	f := newFixture(t, func(s *a2areceiver.Service) { s.WithTimeout(100 * time.Millisecond) })
	ws := f.dial(t, f.secret)
	go func() { _, _, _ = ws.Read(context.Background()) }()
	_, err := f.svc.Dispatch(context.Background(), f.turn("act-6"), f.fb)
	if err == nil || agentdomain.IsPermanent(err) {
		t.Fatalf("err = %v, want a transient timeout", err)
	}
}

func TestMintValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, _, err := f.svc.Mint(ctx, f.scope, "bot", " "); !errors.Is(err, a2areceiver.ErrInvalidName) {
		t.Fatalf("blank name err = %v", err)
	}
	if _, _, err := f.svc.Mint(ctx, f.scope, "nope", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown agent err = %v", err)
	}
}
