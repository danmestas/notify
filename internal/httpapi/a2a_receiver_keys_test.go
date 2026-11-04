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

	"github.com/danmestas/notify/internal/a2areceiver"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

func TestReceiverKeysAPI_Lifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	aud := audience.New(st)
	ags := agentstore.New(st)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil),
		Inbox: inboxapi.New(st), Store: st, Receiver: a2areceiver.New(st, ags, nil),
	}))
	t.Cleanup(srv.Close)
	apiKey, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ags.Agents().Create(ctx, scope, agentdomain.Agent{
		Identifier: "bot", Name: "Bot", Runtime: agentdomain.RuntimeMesh, Active: true, MeshSubject: "agents.prompt.bot",
	}); err != nil {
		t.Fatal(err)
	}
	admin := "ApiKey " + apiKey
	base := srv.URL + "/v1/agents/bot/receiver-keys"

	if code, _ := req(t, http.MethodGet, base, "Bearer x", ""); code != http.StatusUnauthorized {
		t.Fatalf("bearer on management route = %d, want 401", code)
	}
	code, body := req(t, http.MethodPost, base, admin, `{"name":"laptop"}`)
	if code != http.StatusCreated {
		t.Fatalf("mint = %d %s", code, body)
	}
	var minted struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(body), &minted); err != nil {
		t.Fatal(err)
	}
	if minted.ID == "" || !strings.HasPrefix(minted.Secret, a2areceiver.TokenPrefix) {
		t.Fatalf("bad mint response: %s", body)
	}
	code, body = req(t, http.MethodGet, base, admin, "")
	if code != http.StatusOK || !strings.Contains(body, minted.ID) || strings.Contains(body, "secret") {
		t.Fatalf("list = %d %s", code, body)
	}
	if code, _ := req(t, http.MethodPost, base, admin, `{"name":""}`); code != http.StatusUnprocessableEntity {
		t.Errorf("empty name = %d, want 422", code)
	}
	if code, _ := req(t, http.MethodPost, srv.URL+"/v1/agents/nope/receiver-keys", admin, `{"name":"x"}`); code != http.StatusNotFound {
		t.Errorf("unknown agent = %d, want 404", code)
	}
	if code, _ := req(t, http.MethodDelete, base+"/"+minted.ID, admin, ""); code != http.StatusNoContent {
		t.Errorf("revoke = %d, want 204", code)
	}
	if code, _ := req(t, http.MethodDelete, base+"/"+minted.ID, admin, ""); code != http.StatusNotFound {
		t.Errorf("second revoke = %d, want 404", code)
	}
}
