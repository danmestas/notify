package httpapi_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// lockedBuf is a log sink safe for the handler goroutines.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestAgentConfigRelay covers PATCH /v1/agents/{identifier}/config against a
// fake agent on agents.config.<tail>.
func TestAgentConfigRelay(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	keyA, scopeA, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatal(err)
	}
	keyB, _, err := id.Bootstrap(ctx, "Other", "dev")
	if err != nil {
		t.Fatal(err)
	}

	natsURL := dagtest.StartSidecar(t)
	agentConn, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agentConn.Close)

	var (
		mu       sync.Mutex
		gotBody  []byte
		reply    = `{"ok":true,"card":{"name":"Claude","zeta":1}}`
		silent   bool
		received = map[string]int{}
	)
	serve := func(subject string) {
		sub, err := agentConn.Subscribe(subject, func(m *nats.Msg) {
			mu.Lock()
			gotBody = append([]byte(nil), m.Data...)
			received[subject]++
			r, quiet := reply, silent
			mu.Unlock()
			if !quiet {
				_ = m.Respond([]byte(r))
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	serve("agents.config.cc.dmestas.claude")
	serve("agents.config.silent")
	if err := agentConn.Flush(); err != nil {
		t.Fatal(err)
	}

	mc, err := mesh.Dial(natsURL, mesh.WithConfigTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mc.Close)

	logs := &lockedBuf{}
	ags := agentstore.New(st)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: audience.New(st), Trigger: trigger.New(audience.New(st), st, nil),
		Inbox: inboxapi.New(st),
		Agents: agentsapi.New(ags).WithConfigurer(mc).
			WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
	}))
	t.Cleanup(srv.Close)

	mk := func(a agentdomain.Agent) {
		t.Helper()
		a.Name = a.Identifier
		if _, err := ags.Agents().Create(ctx, scopeA, a); err != nil {
			t.Fatal(err)
		}
	}
	mk(agentdomain.Agent{Identifier: "claude", Runtime: "mesh", MeshSubject: "agents.prompt.cc.dmestas.claude"})
	mk(agentdomain.Agent{Identifier: "silent", Runtime: "mesh", MeshSubject: "agents.prompt.silent"})
	mk(agentdomain.Agent{Identifier: "gone", Runtime: "mesh", MeshSubject: "agents.prompt.gone"})
	mk(agentdomain.Agent{Identifier: "elsewhere", Runtime: "mesh", MeshSubject: "agents.other.claude"})

	const providerKey = "sk-PROVIDER-KEY-987"
	body := `{"model":"m1",  "providerKey":"` + providerKey + `"}`
	patch := func(key, ident, payload string, hdr ...string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1/agents/"+ident+"/config", strings.NewReader(payload))
		req.Header.Set("Authorization", "ApiKey "+key)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	t.Run("ok returns the card and the body arrives byte for byte", func(t *testing.T) {
		status, out := patch(keyA, "claude", body, "Idempotency-Key", "k-1")
		if status != http.StatusOK || strings.TrimSpace(out) != `{"card":{"name":"Claude","zeta":1}}` {
			t.Fatalf("%d %s", status, out)
		}
		mu.Lock()
		defer mu.Unlock()
		if string(gotBody) != body {
			t.Errorf("agent got %q, want %q", gotBody, body)
		}
	})
	t.Run("invalid is 400 with the agent's message", func(t *testing.T) {
		mu.Lock()
		reply = `{"ok":false,"error":{"code":"invalid","message":"unknown model m9"}}`
		mu.Unlock()
		status, out := patch(keyA, "claude", `{"model":"m9"}`)
		if status != http.StatusBadRequest || !strings.Contains(out, "unknown model m9") {
			t.Fatalf("%d %s", status, out)
		}
	})
	t.Run("forbidden is 403", func(t *testing.T) {
		mu.Lock()
		reply = `{"ok":false,"error":{"code":"forbidden","message":"read only"}}`
		mu.Unlock()
		if status, out := patch(keyA, "claude", `{"model":"m"}`); status != http.StatusForbidden {
			t.Fatalf("%d %s", status, out)
		}
	})
	t.Run("unavailable code is 503", func(t *testing.T) {
		mu.Lock()
		reply = `{"ok":false,"error":{"code":"unavailable","message":"busy"}}`
		mu.Unlock()
		if status, out := patch(keyA, "claude", `{"model":"m"}`); status != http.StatusServiceUnavailable {
			t.Fatalf("%d %s", status, out)
		}
	})
	t.Run("no responders is 503", func(t *testing.T) {
		if status, out := patch(keyA, "gone", body); status != http.StatusServiceUnavailable {
			t.Fatalf("%d %s", status, out)
		}
	})
	t.Run("timeout is 503", func(t *testing.T) {
		mu.Lock()
		silent = true
		mu.Unlock()
		if status, out := patch(keyA, "silent", body); status != http.StatusServiceUnavailable {
			t.Fatalf("%d %s", status, out)
		}
	})
	t.Run("out of scope is 404 and nothing is relayed", func(t *testing.T) {
		mu.Lock()
		before := received["agents.config.cc.dmestas.claude"]
		mu.Unlock()
		if status, _ := patch(keyB, "claude", body); status != http.StatusNotFound {
			t.Fatalf("status %d", status)
		}
		if status, _ := patch(keyA, "nope", body); status != http.StatusNotFound {
			t.Fatalf("status %d", status)
		}
		mu.Lock()
		defer mu.Unlock()
		if received["agents.config.cc.dmestas.claude"] != before {
			t.Error("out-of-scope request reached the agent")
		}
	})
	t.Run("non-prompt subject is 409", func(t *testing.T) {
		for _, ident := range []string{"elsewhere"} {
			if status, out := patch(keyA, ident, body); status != http.StatusConflict || out == "" {
				t.Errorf("%s: %d %s", ident, status, out)
			}
		}
	})
	t.Run("bad bodies and keys are rejected before relay", func(t *testing.T) {
		for _, p := range []string{``, `[]`, `"x"`, `{bad`, `{"a":"` + strings.Repeat("x", 70<<10) + `"}`} {
			if status, _ := patch(keyA, "claude", p); status != http.StatusBadRequest {
				t.Errorf("body %.20q: status %d", p, status)
			}
		}
		if status, _ := patch(keyA, "claude", body, "Idempotency-Key", "bad key!"); status != http.StatusUnprocessableEntity {
			t.Errorf("bad key: status %d", status)
		}
	})
	t.Run("the provider key never reaches the logs", func(t *testing.T) {
		out := logs.String()
		if !strings.Contains(out, "agent config") || !strings.Contains(out, "outcome=ok") {
			t.Fatalf("no audit line: %s", out)
		}
		if strings.Contains(out, providerKey) {
			t.Fatalf("provider key logged: %s", out)
		}
	})
}
