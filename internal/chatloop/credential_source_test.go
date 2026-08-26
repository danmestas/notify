package chatloop

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestStart_NilCredentialSource_DisablesMastodonAndBluesky proves Start does
// not panic or error when no CredentialSource is injected (the default from
// New), and logs both new providers disabled — mirroring the empty-token
// telegram/discord behaviour.
func TestStart_NilCredentialSource_DisablesMastodonAndBluesky(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	core := openTempStore(t)
	resp := &fakeResponder{}
	svc := New(core, resp, "", "", "support", logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"})

	out := buf.String()
	if !strings.Contains(out, "provider=mastodon") {
		t.Errorf("log missing mastodon disabled line: %q", out)
	}
	if !strings.Contains(out, "provider=bluesky") {
		t.Errorf("log missing bluesky disabled line: %q", out)
	}
	if !strings.Contains(out, "no credential source") {
		t.Errorf("log missing no-credential-source reason: %q", out)
	}
}

// TestStart_CredentialSourceNoActiveIntegration_DisablesPoller proves a
// non-nil CredentialSource that reports "no active integration" (nil map, nil
// error — the sentinel described on CredentialSource) also disables the
// poller, distinctly logged from the nil-source case.
func TestStart_CredentialSourceNoActiveIntegration_DisablesPoller(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	core := openTempStore(t)
	resp := &fakeResponder{}
	svc := New(core, resp, "", "", "support", logger).
		withCredentialSource(func(ctx context.Context, providerID string) (map[string]string, error) {
			return nil, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"})

	out := buf.String()
	if !strings.Contains(out, "no active integration") {
		t.Errorf("log missing no-active-integration reason: %q", out)
	}
	if strings.Contains(out, "no credential source") {
		t.Errorf("log wrongly attributed disable to nil source: %q", out)
	}
}

// TestStart_CredentialSourceError_LogsAndDisables proves a lookup error from
// the CredentialSource (store/decrypt failure) is logged as an error, not
// silently swallowed, and does not panic Start.
func TestStart_CredentialSourceError_LogsAndDisables(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	wantErr := errors.New("boom")
	core := openTempStore(t)
	resp := &fakeResponder{}
	svc := New(core, resp, "", "", "support", logger).
		withCredentialSource(func(ctx context.Context, providerID string) (map[string]string, error) {
			return nil, wantErr
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"})

	out := buf.String()
	if !strings.Contains(out, "boom") {
		t.Errorf("log missing underlying error: %q", out)
	}
	if !strings.Contains(out, "resolve credentials failed") {
		t.Errorf("log missing failure message: %q", out)
	}
}

// TestStart_CredentialSourceResolves_LogsCredentialsFound proves a
// CredentialSource that returns real credentials is actually consulted (not
// skipped) and its result is surfaced, even though the poll loop itself is
// not wired yet.
func TestStart_CredentialSourceResolves_LogsCredentialsFound(t *testing.T) {
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// A stand-in mastodon instance for the poller Start launches: empty
	// notifications on every call, so the background goroutine never logs
	// anything that could race the assertions below.
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer instance.Close()

	var gotProviders []string
	core := openTempStore(t)
	resp := &fakeResponder{}
	svc := New(core, resp, "", "", "support", logger).
		withCredentialSource(func(ctx context.Context, providerID string) (map[string]string, error) {
			gotProviders = append(gotProviders, providerID)
			if providerID == providerMastodon {
				return map[string]string{"base_url": instance.URL, "access_token": "tok"}, nil
			}
			return map[string]string{"pds_url": instance.URL, "identifier": "bot.example.social", "app_password": "app-pw"}, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"})

	if len(gotProviders) != 2 {
		t.Fatalf("credential source calls = %v, want [mastodon bluesky]", gotProviders)
	}
	out := buf.String()
	if !strings.Contains(out, "credentials resolved") {
		t.Errorf("log missing credentials-resolved line: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "provider=mastodon") || strings.Contains(line, "provider=bluesky") {
			if strings.Contains(line, "disabled") {
				t.Errorf("credential provider line wrongly reports disabled: %q", line)
			}
		}
	}
}
