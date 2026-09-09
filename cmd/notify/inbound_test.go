package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/emailloop"
)

const pipedMessage = "Delivered-To: reply+tok@mail.example.org\r\nFrom: dev@example.com\r\nTo: list@example.com\r\nSubject: Re: hi\r\nContent-Type: text/plain\r\n\r\non it\r\n"

// TestPostInbound_ParsesAndPosts: the piped message reaches the server as the
// webhook's own JSON shape, with the secret, and the envelope recipient taken
// from Delivered-To.
func TestPostInbound_ParsesAndPosts(t *testing.T) {
	var gotSecret string
	var got emailloop.InboundEmail
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/inbound/email" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotSecret = r.Header.Get("X-Inbound-Secret")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"status":"threaded"}`)
	}))
	defer srv.Close()

	status, err := postInbound(context.Background(), strings.NewReader(pipedMessage), srv.URL, "s3cret", srv.Client())
	if err != nil {
		t.Fatalf("postInbound: %v", err)
	}
	if status != "threaded" {
		t.Errorf("status = %q", status)
	}
	if gotSecret != "s3cret" {
		t.Errorf("X-Inbound-Secret = %q", gotSecret)
	}
	if got.EnvelopeTo != "reply+tok@mail.example.org" || strings.TrimSpace(got.Text) != "on it" {
		t.Errorf("posted payload = %+v", got)
	}
}

// TestPostInbound_ExitCodes pins the sysexits mapping the MTA acts on: retry
// only for a fault that a retry could fix.
func TestPostInbound_ExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		stdin  string
		want   int
	}{
		{"server error retries", 500, "boom", pipedMessage, exitTempFail},
		{"wrong secret is config", 401, "unauthorized", pipedMessage, exitConfig},
		{"inbound not configured is config", 503, "inbound email not configured", pipedMessage, exitConfig},
		{"bad request is data", 400, "bad request", pipedMessage, exitDataErr},
		{"unparseable stdin is data", 200, "", "\x00 not mail", exitDataErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			_, err := postInbound(context.Background(), strings.NewReader(tc.stdin), srv.URL, "s", srv.Client())
			if err == nil {
				t.Fatal("want an error")
			}
			if got := exitCodeFor(err); got != tc.want {
				t.Errorf("exit code = %d, want %d (%v)", got, tc.want, err)
			}
		})
	}

	// Server unreachable: the MTA must queue, not bounce.
	_, err := postInbound(context.Background(), strings.NewReader(pipedMessage), "http://127.0.0.1:1", "s", &http.Client{})
	if exitCodeFor(err) != exitTempFail {
		t.Errorf("unreachable server exit = %d, want %d", exitCodeFor(err), exitTempFail)
	}
}

// TestRunInbound_RequiresSecret: with no secret there is nothing to
// authenticate with, and that is configuration, not a transient.
func TestRunInbound_RequiresSecret(t *testing.T) {
	t.Setenv("NOTIFY_INBOUND_SECRET", "")
	err := runInbound([]string{"-server", "http://127.0.0.1:1"}, strings.NewReader(pipedMessage), io.Discard)
	if exitCodeFor(err) != exitConfig {
		t.Fatalf("exit = %d, want %d (%v)", exitCodeFor(err), exitConfig, err)
	}
}
