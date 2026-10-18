package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/blobstore"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

func startArchiveServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := blobstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	aud := audience.New(st)
	key, _, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil), Inbox: inboxapi.New(st), Store: st, Blobs: blobs}))
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key
}

func archive(t *testing.T, srv *httptest.Server, auth, recipient string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/inbound/messages?recipient="+recipient, bytes.NewReader(body))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "message/rfc822")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestInboundMessages_Archive: a client archives mail it received to any
// address, with no reply token: the bytes are kept exactly, headers are read
// for the listing, and sending the same message again returns the same record.
func TestInboundMessages_Archive(t *testing.T) {
	srv, auth := startArchiveServer(t)
	raw := []byte("From: Ann <ann@example.com>\r\nSubject: =?utf-8?q?caf=C3=A9?=\r\nMessage-ID: <m-9@example.com>\r\n\r\nbody \xff\r\n")

	status, first := archive(t, srv, auth, "daniel@example.org", raw)
	if status != http.StatusOK || first["from"] != "Ann <ann@example.com>" || first["subject"] != "café" || first["messageId"] != "<m-9@example.com>" || first["recipient"] != "daniel@example.org" {
		t.Fatalf("archive: %d %v", status, first)
	}
	if _, again := archive(t, srv, auth, "daniel@example.org", raw); again["id"] != first["id"] {
		t.Fatalf("a resend made a new record: %v vs %v", again["id"], first["id"])
	}
	if _, other := archive(t, srv, auth, "team@example.org", raw); other["id"] == first["id"] {
		t.Fatal("the same message to another address shared a record")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/inbound/messages/"+first["id"].(string)+"/raw", nil)
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(got, raw) {
		t.Fatalf("raw = %q", got)
	}

	if s, _ := archive(t, srv, auth, "", raw); s != http.StatusBadRequest {
		t.Fatalf("no recipient: %d, want 400", s)
	}
	if s, _ := archive(t, srv, auth, "d@example.org", nil); s != http.StatusBadRequest {
		t.Fatalf("empty body: %d, want 400", s)
	}
	if s, _ := archive(t, srv, auth, "d@example.org", make([]byte, 40<<20+1)); s != http.StatusRequestEntityTooLarge {
		t.Fatalf("over 40 MiB: %d, want 413", s)
	}
	if s, _ := archive(t, srv, "ApiKey wrong", "d@example.org", raw); s != http.StatusUnauthorized {
		t.Fatalf("bad key: %d, want 401", s)
	}
}

// TestInboundMessages_ListAndRaw: accepted mail is listed oldest first with a
// cursor, and its original bytes come back exactly, as message/rfc822.
func TestInboundMessages_ListAndRaw(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := blobstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	aud := audience.New(st)
	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil), Inbox: inboxapi.New(st), Store: st, Blobs: blobs}))
	t.Cleanup(srv.Close)
	auth := "ApiKey " + key

	var raws [][]byte
	for _, body := range []string{"first\r\n", "second \xff\r\n"} {
		raw := []byte("From: a@x\r\nSubject: s\r\n\r\n" + body)
		ref, err := blobs.Put(ctx, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.InboundMessages().Record(ctx, scope, store.InboundMessage{SHA256: ref.SHA256, Size: ref.Size, Recipient: "r@x", Sender: "a@x", Subject: "s"}); err != nil {
			t.Fatal(err)
		}
		raws = append(raws, raw)
	}

	var page struct {
		Messages []struct {
			ID   string `json:"id"`
			Size int64  `json:"size"`
		} `json:"messages"`
		Cursor string `json:"cursor"`
	}
	status, body := doGet(t, srv.URL+"/v1/inbound/messages?limit=1", auth)
	if status != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil || len(page.Messages) != 1 {
		t.Fatalf("page 1: %d %s", status, body)
	}
	first := page.Messages[0].ID
	_, body = doGet(t, srv.URL+"/v1/inbound/messages?after="+page.Cursor, auth)
	if json.Unmarshal([]byte(body), &page) != nil || len(page.Messages) != 1 || page.Messages[0].ID == first {
		t.Fatalf("page 2: %s", body)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/inbound/messages/"+first+"/raw", nil)
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "message/rfc822" || !bytes.Equal(got, raws[0]) {
		t.Fatalf("raw: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), got)
	}
	if status, _ := doGet(t, srv.URL+"/v1/inbound/messages/nope/raw", auth); status != http.StatusNotFound {
		t.Fatalf("unknown id: %d, want 404", status)
	}
}
