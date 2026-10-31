package chatloop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/danmestas/notify/internal/providers/bluesky"
)

// TestSenders_TruncateToPlatformLimit proves every reply sender cuts an
// oversized agent reply to its platform's limit. Each platform rejects an
// over-limit message outright, so without the cut a long reply is never
// delivered at all.
func TestSenders_TruncateToPlatformLimit(t *testing.T) {
	long := strings.Repeat("界", 10_000)
	ctx := context.Background()

	tests := []struct {
		name string
		max  int
		// send posts long through the sender under test against srvURL.
		send func(t *testing.T, srvURL string) error
		// text extracts the sent message text from the captured request.
		text func(t *testing.T, body []byte) string
	}{
		{
			name: "telegram",
			max:  4096,
			send: func(t *testing.T, srvURL string) error {
				return newTelegramSender("tok", srvURL, http.DefaultClient).send(ctx, "42", long)
			},
			text: jsonField("text"),
		},
		{
			name: "discord",
			max:  2000,
			send: func(t *testing.T, srvURL string) error {
				return newDiscordSender("tok", srvURL, http.DefaultClient).send(ctx, "c1", long)
			},
			text: jsonField("content"),
		},
		{
			name: "mastodon",
			max:  500,
			send: func(t *testing.T, srvURL string) error {
				return newMastodonSender("tok", srvURL, http.DefaultClient).send(ctx, "alice", "", "", long)
			},
			text: func(t *testing.T, body []byte) string {
				form, err := url.ParseQuery(string(body))
				if err != nil {
					t.Fatalf("parse form: %v", err)
				}
				return form.Get("status")
			},
		},
		{
			name: "bluesky",
			max:  300,
			send: func(t *testing.T, srvURL string) error {
				client := &bluesky.Client{PDSURL: srvURL, Identifier: "bot.example.social", AppPassword: "pw"}
				return newBlueskySender(client).send(ctx, long, blueskyPendingReply{})
			},
			text: func(t *testing.T, body []byte) string {
				var req struct {
					Record struct {
						Text string `json:"text"`
					} `json:"record"`
				}
				if err := json.Unmarshal(body, &req); err != nil {
					t.Fatalf("decode createRecord: %v", err)
				}
				return req.Record.Text
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sent []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "com.atproto.server.createSession") {
					_, _ = w.Write([]byte(`{"accessJwt":"a","refreshJwt":"r","did":"did:plc:bot"}`))
					return
				}
				sent = body
				_, _ = w.Write([]byte(`{"ok":true,"id":"1","uri":"at://x","cid":"c"}`))
			}))
			defer srv.Close()

			if err := tc.send(t, srv.URL); err != nil {
				t.Fatalf("send: %v", err)
			}
			got := tc.text(t, sent)
			if n := utf8.RuneCountInString(got); n != tc.max {
				t.Fatalf("sent %d runes, want %d", n, tc.max)
			}
			if !strings.HasSuffix(got, "…") {
				t.Errorf("truncated text does not end with an ellipsis")
			}
		})
	}
}

func jsonField(name string) func(t *testing.T, body []byte) string {
	return func(t *testing.T, body []byte) string {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		s, _ := m[name].(string)
		return s
	}
}
