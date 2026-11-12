package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/blobstore"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/providers/twilio"
	"github.com/danmestas/notify/internal/smsloop"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// TestInboundSMS_EndToEnd: a provider posts a signed text to the public
// route with no API key; it reaches the tenant's inbound message listing,
// marked as a text. An unsigned post is refused, not asked for an API key.
func TestInboundSMS_EndToEnd(t *testing.T) {
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
	mk := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, mk)
	aud := audience.New(st)
	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatal(err)
	}
	conn := connectivity.New(st, mk)
	in, err := conn.CreateTwilio(ctx, scope, "tw", connectivity.TwilioConfig{AccountSID: "AC1", AuthToken: "tok", From: "+15559990000"}, true)
	if err != nil {
		t.Fatal(err)
	}
	const public = "https://notify.example.com"
	sms := smsloop.New(st, conn, preferences.New(st), public, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil), Inbox: inboxapi.New(st),
		Store: st, Blobs: blobs, Connectivity: conn, SMSLoop: sms,
	}))
	t.Cleanup(srv.Close)

	path := "/v1/inbound/sms/" + in.ID
	form := url.Values{"MessageSid": {"SM1"}, "From": {"+15551112222"}, "To": {"+15559990000"}, "Body": {"hello"}}
	post := func(sig string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if sig != "" {
			req.Header.Set(twilio.SignatureHeader, sig)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := post(""); got != http.StatusForbidden {
		t.Fatalf("unsigned: %d, want 403", got)
	}
	if got := post(twilio.Signature("tok", public+path, form)); got != http.StatusOK {
		t.Fatalf("signed: %d, want 200", got)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/inbound/messages", nil)
	req.Header.Set("Authorization", "ApiKey "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("listing = %v", out.Messages)
	}
	m := out.Messages[0]
	if m["channel"] != "sms" || m["from"] != "+15551112222" || m["recipient"] != "+15559990000" || m["body"] != "hello" || m["integrationId"] != in.ID {
		t.Fatalf("listed text = %v", m)
	}
}
