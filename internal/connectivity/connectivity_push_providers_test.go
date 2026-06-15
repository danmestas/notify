package connectivity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/expo"
	"github.com/danmestas/notify/internal/providers/fcm"
)

// testServiceAccountJSON builds a syntactically valid Google service-account
// JSON with a freshly generated RSA private key so google.JWTConfigFromJSON (and
// thus fcm.NewClient, exercised by ResolvePush) parses it without reaching the
// network.
func testServiceAccountJSON(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	sa := map[string]string{
		"type":         "service_account",
		"project_id":   "test-proj",
		"private_key":  string(pemKey),
		"client_email": "notify@test-proj.iam.gserviceaccount.com",
		"token_uri":    "https://oauth2.googleapis.com/token",
	}
	b, err := json.Marshal(sa)
	if err != nil {
		t.Fatalf("marshal service account: %v", err)
	}
	return string(b)
}

func TestCreateExpo_ThenResolvePush(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := ExpoConfig{AccessToken: "expo_live_secret"}
	if _, err := svc.CreateExpo(ctx, scope, "primary-expo", cfg, true); err != nil {
		t.Fatalf("CreateExpo: %v", err)
	}

	provider, providerID, integrationID, err := svc.ResolvePush(ctx, scope)
	if err != nil {
		t.Fatalf("ResolvePush: %v", err)
	}
	if providerID != "expo" {
		t.Errorf("providerID = %q, want %q", providerID, "expo")
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateExpo")
	}
	client, ok := provider.(*expo.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *expo.Client", provider)
	}
	if client.AccessToken != cfg.AccessToken {
		t.Errorf("AccessToken = %q, want %q", client.AccessToken, cfg.AccessToken)
	}
}

func TestCreateExpo_EncryptsCredentials(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := ExpoConfig{AccessToken: "expo_live_secret"}
	in, err := svc.CreateExpo(ctx, scope, "primary-expo", cfg, true)
	if err != nil {
		t.Fatalf("CreateExpo: %v", err)
	}
	if len(in.EncCredentials) == 0 {
		t.Fatal("EncCredentials is empty")
	}
	if bytes.Contains(in.EncCredentials, []byte(cfg.AccessToken)) {
		t.Fatal("EncCredentials contains plaintext access token bytes")
	}
}

func TestCreateFCM_ThenResolvePush(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := FCMConfig{ServiceAccount: testServiceAccountJSON(t), ProjectID: "test-proj"}
	if _, err := svc.CreateFCM(ctx, scope, "primary-fcm", cfg, true); err != nil {
		t.Fatalf("CreateFCM: %v", err)
	}

	provider, providerID, integrationID, err := svc.ResolvePush(ctx, scope)
	if err != nil {
		t.Fatalf("ResolvePush: %v", err)
	}
	if providerID != "fcm" {
		t.Errorf("providerID = %q, want %q", providerID, "fcm")
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateFCM")
	}
	client, ok := provider.(*fcm.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *fcm.Client", provider)
	}
	if client.ProjectID != "test-proj" {
		t.Errorf("ProjectID = %q, want %q", client.ProjectID, "test-proj")
	}
	if client.Tokens == nil {
		t.Error("fcm client has no token source after ResolvePush")
	}
}

func TestCreateFCM_EncryptsServiceAccount(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := FCMConfig{ServiceAccount: testServiceAccountJSON(t), ProjectID: "test-proj"}
	in, err := svc.CreateFCM(ctx, scope, "primary-fcm", cfg, true)
	if err != nil {
		t.Fatalf("CreateFCM: %v", err)
	}
	if len(in.EncCredentials) == 0 {
		t.Fatal("EncCredentials is empty")
	}
	// The private-key marker must not survive in the sealed blob.
	if bytes.Contains(in.EncCredentials, []byte("PRIVATE KEY")) {
		t.Fatal("EncCredentials contains plaintext service-account private key")
	}
}

// TestResolvePush_ScopeIsolation proves an Expo integration in one scope is
// invisible to another scope (no cross-tenant leak).
func TestResolvePush_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o2", EnvID: "e2"}

	if _, err := svc.CreateExpo(ctx, scopeA, "a-expo", ExpoConfig{AccessToken: "a_secret"}, true); err != nil {
		t.Fatalf("CreateExpo(scopeA): %v", err)
	}

	if _, _, _, err := svc.ResolvePush(ctx, scopeB); err == nil {
		t.Fatal("ResolvePush(scopeB) resolved scopeA's integration: cross-scope leak")
	}
}
