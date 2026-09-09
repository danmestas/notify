package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/store"
)

// signingServer is an API with (or without) a secretbox for endpoint signing.
func signingServer(t *testing.T, withBox bool) (*httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	key, _, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	deps := httpapi.Deps{Identity: id, Store: st}
	if withBox {
		box, err := secretbox.New(masterKey)
		if err != nil {
			t.Fatalf("secretbox.New: %v", err)
		}
		deps.Secrets = box
	}
	srv := httptest.NewServer(httpapi.New(deps))
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key
}

type endpointResp struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Signed bool   `json:"signed"`
	Secret string `json:"secret"`
}

func decodeEndpoint(t *testing.T, body string) endpointResp {
	t.Helper()
	var e endpointResp
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return e
}

// TestWebhookEndpoint_SecretShownOnceThenMasked: create returns the secret;
// every read afterwards reports only that the endpoint is signed.
func TestWebhookEndpoint_SecretShownOnceThenMasked(t *testing.T) {
	srv, auth := signingServer(t, true)

	status, body := doPost(t, srv.URL+"/v1/webhooks/endpoints", auth, `{"url":"https://quarry.example/notify","active":true}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	created := decodeEndpoint(t, body)
	if len(created.Secret) != 64 || !created.Signed {
		t.Fatalf("create response = %+v; want a 64-hex secret and signed=true", created)
	}

	status, body = doGet(t, srv.URL+"/v1/webhooks/endpoints/"+created.ID, auth)
	if status != http.StatusOK {
		t.Fatalf("get: %d %s", status, body)
	}
	got := decodeEndpoint(t, body)
	if got.Secret != "" || !got.Signed {
		t.Fatalf("GET response = %+v; the secret must not be readable back, signed must be true", got)
	}
	if _, list := doGet(t, srv.URL+"/v1/webhooks/endpoints", auth); bytes.Contains([]byte(list), []byte(created.Secret)) {
		t.Fatal("the list response leaks the secret")
	}
}

// TestWebhookEndpoint_RotateSecret: rotate returns a fresh secret once; an
// unknown id is 404.
func TestWebhookEndpoint_RotateSecret(t *testing.T) {
	srv, auth := signingServer(t, true)
	_, body := doPost(t, srv.URL+"/v1/webhooks/endpoints", auth, `{"url":"https://quarry.example/notify","active":true}`)
	created := decodeEndpoint(t, body)

	status, body := doPost(t, srv.URL+"/v1/webhooks/endpoints/"+created.ID+"/secret", auth, "")
	if status != http.StatusOK {
		t.Fatalf("rotate: %d %s", status, body)
	}
	rotated := decodeEndpoint(t, body)
	if len(rotated.Secret) != 64 || rotated.Secret == created.Secret {
		t.Fatalf("rotated = %+v; want a different 64-hex secret", rotated)
	}
	if status, _ := doPost(t, srv.URL+"/v1/webhooks/endpoints/nope/secret", auth, ""); status != http.StatusNotFound {
		t.Fatalf("rotate unknown = %d, want 404", status)
	}
}

// TestWebhookEndpoint_NoSecretboxIsLoud: without a secretbox an endpoint is
// created unsigned and says so, and rotate refuses rather than pretending.
func TestWebhookEndpoint_NoSecretboxIsLoud(t *testing.T) {
	srv, auth := signingServer(t, false)
	status, body := doPost(t, srv.URL+"/v1/webhooks/endpoints", auth, `{"url":"https://quarry.example/notify","active":true}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	created := decodeEndpoint(t, body)
	if created.Secret != "" || created.Signed {
		t.Fatalf("create without a box = %+v; want no secret and signed=false", created)
	}
	if status, _ := doPost(t, srv.URL+"/v1/webhooks/endpoints/"+created.ID+"/secret", auth, ""); status != http.StatusServiceUnavailable {
		t.Fatalf("rotate without a box = %d, want 503", status)
	}
}
