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
