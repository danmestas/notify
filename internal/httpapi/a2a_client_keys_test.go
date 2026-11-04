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

	"github.com/danmestas/notify/internal/a2aauth"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

func TestA2AClientKeysAPI_Lifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	aud := audience.New(st)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil),
		Inbox: inboxapi.New(st), Store: st, A2AAuth: a2aauth.New(st, id, nil),
	}))
	t.Cleanup(srv.Close)
	apiKey, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "alice"}); err != nil {
		t.Fatal(err)
	}
	admin := "ApiKey " + apiKey

	// Bearer credentials cannot manage keys: the routes sit behind the API-key group.
	if code, _ := req(t, http.MethodGet, srv.URL+"/v1/a2a/client-keys", "Bearer x", ""); code != http.StatusUnauthorized {
		t.Fatalf("bearer on management route = %d, want 401", code)
	}

	code, body := req(t, http.MethodPost, srv.URL+"/v1/a2a/client-keys", admin, `{"name":"billing","subscriberId":"alice"}`)
	if code != http.StatusCreated {
		t.Fatalf("mint = %d %s", code, body)
	}
	var minted struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		SubscriberID string `json:"subscriberId"`
		Secret       string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(body), &minted); err != nil {
		t.Fatal(err)
	}
	if minted.ID == "" || !strings.HasPrefix(minted.Secret, a2aauth.TokenPrefix) || minted.SubscriberID != "alice" {
		t.Fatalf("bad mint response: %s", body)
	}

	// The secret is shown once: list never returns it.
	code, body = req(t, http.MethodGet, srv.URL+"/v1/a2a/client-keys", admin, "")
	if code != http.StatusOK || !strings.Contains(body, minted.ID) {
		t.Fatalf("list = %d %s", code, body)
	}
	if strings.Contains(body, minted.Secret) || strings.Contains(body, "secret") {
		t.Errorf("list leaks the secret: %s", body)
	}

	if code, body := req(t, http.MethodPost, srv.URL+"/v1/a2a/client-keys", admin, `{"name":""}`); code != http.StatusUnprocessableEntity {
		t.Errorf("empty name = %d %s, want 422", code, body)
	}
	if code, _ := req(t, http.MethodPost, srv.URL+"/v1/a2a/client-keys", admin, `{"name":"x","subscriberId":"nobody"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown subscriber = %d, want 422", code)
	}

	if code, _ := req(t, http.MethodDelete, srv.URL+"/v1/a2a/client-keys/"+minted.ID, admin, ""); code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", code)
	}
	if code, _ := req(t, http.MethodDelete, srv.URL+"/v1/a2a/client-keys/"+minted.ID, admin, ""); code != http.StatusNotFound {
		t.Errorf("re-revoke = %d, want 404", code)
	}
	code, body = req(t, http.MethodGet, srv.URL+"/v1/a2a/client-keys", admin, "")
	if code != http.StatusOK || !strings.Contains(body, "revokedAt") {
		t.Errorf("list after revoke = %d %s, want revokedAt", code, body)
	}
}
