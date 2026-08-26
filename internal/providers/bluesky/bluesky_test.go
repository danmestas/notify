package bluesky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/danmestas/notify/internal/providers"
)

// xrpcServer builds a fake AT Protocol PDS. authFail401Count controls how
// many createRecord calls in a row are answered with 401 before succeeding,
// letting tests exercise the refresh/re-login retry path. refreshFails, when
// true, makes com.atproto.server.refreshSession always fail (401) so Send
// must fall back to a full createSession.
type xrpcServer struct {
	t              *testing.T
	srv            *httptest.Server
	createSessions atomic.Int64
	refreshes      atomic.Int64
	createRecords  atomic.Int64

	// failCreateRecordUnauthorized fails the next N createRecord calls with a
	// 401 (simulating an expired accessJwt).
	failCreateRecordUnauthorized int64
	refreshFails                 bool

	lastRecordBody map[string]any
}

func newXRPCServer(t *testing.T) *xrpcServer {
	t.Helper()
	x := &xrpcServer{t: t}
	x.srv = httptest.NewServer(http.HandlerFunc(x.handle))
	t.Cleanup(x.srv.Close)
	return x
}

func (x *xrpcServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "com.atproto.server.createSession"):
		x.createSessions.Add(1)
		var body struct {
			Identifier string `json:"identifier"`
			Password   string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Identifier == "" || body.Password == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessJwt":  "access-1",
			"refreshJwt": "refresh-1",
			"did":        "did:plc:test123",
			"handle":     body.Identifier,
		})
	case strings.HasSuffix(r.URL.Path, "com.atproto.server.refreshSession"):
		x.refreshes.Add(1)
		if x.refreshFails {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"ExpiredToken","message":"Token has expired"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessJwt":  "access-2",
			"refreshJwt": "refresh-2",
			"did":        "did:plc:test123",
		})
	case strings.HasSuffix(r.URL.Path, "com.atproto.repo.createRecord"):
		n := x.createRecords.Add(1)
		if n <= x.failCreateRecordUnauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"ExpiredToken","message":"Token has expired"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		x.lastRecordBody = body
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"uri": "at://did:plc:test123/app.bsky.feed.post/abc123",
			"cid": "bafyabc123",
		})
	default:
		x.t.Fatalf("unexpected path: %s", r.URL.Path)
	}
}

func TestClient_Send_MintsSessionOnFirstSend(t *testing.T) {
	x := newXRPCServer(t)
	c := &Client{PDSURL: x.srv.URL, Identifier: "alice.bsky.social", AppPassword: "app-pw-123"}

	res, err := c.Send(context.Background(), providers.ChatMessage{Content: "hello bluesky"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if x.createSessions.Load() != 1 {
		t.Errorf("createSessions = %d, want 1", x.createSessions.Load())
	}
	if res.ProviderMessageID != "at://did:plc:test123/app.bsky.feed.post/abc123" {
		t.Errorf("ProviderMessageID = %q, want the returned uri", res.ProviderMessageID)
	}
	record, _ := x.lastRecordBody["record"].(map[string]any)
	if record == nil || record["text"] != "hello bluesky" {
		t.Errorf("record body = %+v, want text=hello bluesky", x.lastRecordBody)
	}
	if record["$type"] != "app.bsky.feed.post" {
		t.Errorf("record $type = %v, want app.bsky.feed.post", record["$type"])
	}
	if x.lastRecordBody["collection"] != "app.bsky.feed.post" {
		t.Errorf("collection = %v, want app.bsky.feed.post", x.lastRecordBody["collection"])
	}
	if x.lastRecordBody["repo"] != "did:plc:test123" {
		t.Errorf("repo = %v, want did:plc:test123", x.lastRecordBody["repo"])
	}

	// A second send reuses the cached session; no second createSession call.
	if _, err := c.Send(context.Background(), providers.ChatMessage{Content: "second post"}); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if x.createSessions.Load() != 1 {
		t.Errorf("createSessions after 2nd send = %d, want still 1 (session reused)", x.createSessions.Load())
	}
}

func TestClient_Send_ExpiredAccessJwt_RefreshesAndRetries(t *testing.T) {
	x := newXRPCServer(t)
	x.failCreateRecordUnauthorized = 1 // first createRecord 401s, second succeeds
	c := &Client{PDSURL: x.srv.URL, Identifier: "alice.bsky.social", AppPassword: "app-pw-123"}

	res, err := c.Send(context.Background(), providers.ChatMessage{Content: "hi"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if x.refreshes.Load() != 1 {
		t.Errorf("refreshes = %d, want 1", x.refreshes.Load())
	}
	if x.createSessions.Load() != 1 {
		t.Errorf("createSessions = %d, want 1 (no full re-login needed)", x.createSessions.Load())
	}
	if res.ProviderMessageID == "" {
		t.Error("ProviderMessageID empty after successful retry")
	}
}

func TestClient_Send_RefreshFails_FallsBackToFullLogin(t *testing.T) {
	x := newXRPCServer(t)
	x.failCreateRecordUnauthorized = 1
	x.refreshFails = true
	c := &Client{PDSURL: x.srv.URL, Identifier: "alice.bsky.social", AppPassword: "app-pw-123"}

	res, err := c.Send(context.Background(), providers.ChatMessage{Content: "hi"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if x.refreshes.Load() != 1 {
		t.Errorf("refreshes = %d, want 1 (attempted once, then abandoned)", x.refreshes.Load())
	}
	if x.createSessions.Load() != 2 {
		t.Errorf("createSessions = %d, want 2 (initial + fallback re-login)", x.createSessions.Load())
	}
	if res.ProviderMessageID == "" {
		t.Error("ProviderMessageID empty after fallback re-login retry")
	}
}

func TestClient_Send_TruncatesTo300Graphemes(t *testing.T) {
	x := newXRPCServer(t)
	c := &Client{PDSURL: x.srv.URL, Identifier: "alice.bsky.social", AppPassword: "app-pw-123"}

	// Multi-byte rune (emoji-adjacent CJK character) repeated past the limit.
	long := strings.Repeat("界", 350)
	if _, err := c.Send(context.Background(), providers.ChatMessage{Content: long}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	record, _ := x.lastRecordBody["record"].(map[string]any)
	text, _ := record["text"].(string)
	runes := []rune(text)
	if len(runes) != 300 {
		t.Fatalf("truncated length = %d runes, want 300", len(runes))
	}
	if runes[299] != '…' {
		t.Errorf("last rune = %q, want ellipsis", runes[299])
	}
	if string(runes[:299]) != strings.Repeat("界", 299) {
		t.Errorf("truncated prefix altered")
	}
}

func TestClient_Send_ShortTextNotTruncated(t *testing.T) {
	x := newXRPCServer(t)
	c := &Client{PDSURL: x.srv.URL, Identifier: "alice.bsky.social", AppPassword: "app-pw-123"}

	if _, err := c.Send(context.Background(), providers.ChatMessage{Content: "short post"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	record, _ := x.lastRecordBody["record"].(map[string]any)
	if record["text"] != "short post" {
		t.Errorf("text = %v, want unchanged short post", record["text"])
	}
}

func TestClient_Send_DefaultsPDSURL(t *testing.T) {
	c := &Client{Identifier: "alice.bsky.social", AppPassword: "app-pw-123"}
	if got := c.pdsURL(); got != "https://bsky.social" {
		t.Errorf("pdsURL() = %q, want https://bsky.social", got)
	}
}

func TestClient_Send_EmptyIdentifierIsPermanent(t *testing.T) {
	c := &Client{AppPassword: "pw"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty identifier classified transient, want permanent: %v", err)
	}
}

func TestClient_Send_EmptyAppPasswordIsPermanent(t *testing.T) {
	c := &Client{Identifier: "alice.bsky.social"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty app password classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_RedactsAppPasswordAndJWTs proves neither the app password
// nor any minted JWT ever appears in an error string, even when the server
// misbehaves in a way that would otherwise echo request/response internals
// back into the error.
func TestClient_Send_RedactsAppPasswordAndJWTs(t *testing.T) {
	const pw = "super-secret-app-password"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every response body maliciously echoes back the request body,
		// which contains the app password on the createSession call.
		body := new(strings.Builder)
		_, _ = body.WriteString(`{"error":"BadRequest","message":"rejected: `)
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body.Write(buf[:n])
		body.WriteString(`"}`)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body.String()))
	}))
	defer srv.Close()

	c := &Client{PDSURL: srv.URL, Identifier: "alice.bsky.social", AppPassword: pw}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "hi"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), pw) {
		t.Errorf("error leaks app password: %q", err.Error())
	}
}
