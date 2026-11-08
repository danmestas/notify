package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	"github.com/danmestas/notify/internal/agents/cards"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

type fakeCards struct {
	mu sync.Mutex
	m  map[string]cards.Card
}

func (f *fakeCards) Card(key string) (cards.Card, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.m[key]
	return c, ok
}

func (f *fakeCards) set(key string, raw string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = cards.Card{Raw: []byte(raw), UpdatedAt: at}
}

// TestAgentCards covers the card on GET /v1/agents[/{id}].
func TestAgentCards(t *testing.T) {
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
	ags := agentstore.New(st)
	src := &fakeCards{m: map[string]cards.Card{}}
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: audience.New(st), Trigger: trigger.New(audience.New(st), st, nil),
		Inbox: inboxapi.New(st), Agents: agentsapi.New(ags).WithCards(src),
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
	mk(agentdomain.Agent{Identifier: "nocard", Runtime: "mesh", MeshSubject: "agents.prompt.nocard"})
	mk(agentdomain.Agent{Identifier: "elsewhere", Runtime: "mesh", MeshSubject: "agents.other.claude"})

	authA, authB := "ApiKey "+keyA, "ApiKey "+keyB
	// Unusual key order: it must pass through unchanged (encoding/json compacts whitespace, nothing else).
	const raw = `{"protocolVersion":"0.3.0","name":"Claude","zeta":1}`
	at := time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)
	src.set("cc.dmestas.claude", raw, at)

	type view struct {
		Card          json.RawMessage `json:"card"`
		CardUpdatedAt string          `json:"cardUpdatedAt"`
	}
	get := func(auth, ident string) (int, string, view) {
		t.Helper()
		status, body := doGet(t, srv.URL+"/v1/agents/"+ident, auth)
		var v view
		_ = json.Unmarshal([]byte(body), &v)
		return status, body, v
	}

	t.Run("present is passed through byte for byte", func(t *testing.T) {
		status, body, v := get(authA, "claude")
		if status != http.StatusOK || string(v.Card) != raw {
			t.Fatalf("%d card=%s", status, v.Card)
		}
		if !strings.Contains(body, `"card":`+raw) {
			t.Errorf("card not embedded verbatim: %s", body)
		}
		if v.CardUpdatedAt != "2026-11-04T01:02:03.000000000Z" {
			t.Errorf("cardUpdatedAt = %q", v.CardUpdatedAt)
		}
	})
	t.Run("absent is null", func(t *testing.T) {
		for _, ident := range []string{"nocard", "elsewhere"} {
			status, body, v := get(authA, ident)
			if status != http.StatusOK || string(v.Card) != "null" || strings.Contains(body, "cardUpdatedAt") {
				t.Errorf("%s: %d %s", ident, status, body)
			}
		}
	})
	t.Run("out of scope is 404 with no card", func(t *testing.T) {
		status, body, _ := get(authB, "claude")
		if status != http.StatusNotFound || strings.Contains(body, "Claude") {
			t.Fatalf("%d %s", status, body)
		}
		status, body = doGet(t, srv.URL+"/v1/agents", authB)
		if status != http.StatusOK || strings.Contains(body, "Claude") || strings.Contains(body, "claude") {
			t.Fatalf("list leaked: %d %s", status, body)
		}
	})
	t.Run("a watch update is reflected", func(t *testing.T) {
		src.set("cc.dmestas.claude", `{"name":"Claude v2"}`, at.Add(time.Hour))
		_, _, v := get(authA, "claude")
		if string(v.Card) != `{"name":"Claude v2"}` || v.CardUpdatedAt != "2026-11-04T02:02:03.000000000Z" {
			t.Errorf("%s %s", v.Card, v.CardUpdatedAt)
		}
		_, list := doGet(t, srv.URL+"/v1/agents", authA)
		if !strings.Contains(list, `{"name":"Claude v2"}`) {
			t.Errorf("list: %s", list)
		}
	})
}
