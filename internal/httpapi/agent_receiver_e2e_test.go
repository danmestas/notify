package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/danmestas/notify/internal/a2areceiver"
	agentsapi "github.com/danmestas/notify/internal/agents/api"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// The agent read says who answers: the configured runtime, or the live
// receiver once a harness has connected through the real HTTP stack.
func TestAgentReadReportsLiveReceiver(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	apiKey, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatal(err)
	}
	ags := agentstore.New(st)
	rcv := a2areceiver.New(st, ags, nil)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: audience.New(st), Trigger: trigger.New(audience.New(st), st, nil),
		Inbox: inboxapi.New(st), Store: st, Receiver: rcv,
		Agents: agentsapi.New(ags).WithLiveReceivers(rcv),
	}))
	t.Cleanup(srv.Close)
	agent, err := ags.Agents().Create(ctx, scope, agentdomain.Agent{
		Identifier: "bot", Name: "Bot", Runtime: agentdomain.RuntimeMesh, Active: true, MeshSubject: "agents.prompt.bot",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := rcv.Mint(ctx, scope, "bot", "laptop")
	if err != nil {
		t.Fatal(err)
	}

	receiver := func() (kind, since string) {
		t.Helper()
		_, body := doGet(t, srv.URL+"/v1/agents/bot", "ApiKey "+apiKey)
		var v struct {
			Receiver struct{ Kind, Since string } `json:"receiver"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		return v.Receiver.Kind, v.Receiver.Since
	}
	if kind, since := receiver(); kind != "mesh" || since != "" {
		t.Fatalf("before connect: %q %q", kind, since)
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/a2a/" + agent.ID + "/receive"
	ws, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if kind, since := receiver(); kind == "live" && since != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent read never reported the live receiver")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = ws.Close(websocket.StatusNormalClosure, "")
	for {
		if kind, _ := receiver(); kind == "mesh" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent read never fell back to the runtime")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
