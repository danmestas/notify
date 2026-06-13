package connectivity

import (
	"bytes"
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/ntfy"
)

func TestCreateNtfy_ThenResolvePush(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := NtfyConfig{
		BaseURL: "http://localhost:2586",
		Token:   "tk_live_secret",
	}
	if _, err := svc.CreateNtfy(ctx, scope, "primary-ntfy", cfg, true); err != nil {
		t.Fatalf("CreateNtfy: %v", err)
	}

	provider, providerID, integrationID, err := svc.ResolvePush(ctx, scope)
	if err != nil {
		t.Fatalf("ResolvePush: %v", err)
	}
	if providerID != "ntfy" {
		t.Errorf("providerID = %q, want %q", providerID, "ntfy")
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateNtfy")
	}
	client, ok := provider.(*ntfy.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *ntfy.Client", provider)
	}
	if client.BaseURL != cfg.BaseURL {
		t.Errorf("BaseURL = %q, want %q", client.BaseURL, cfg.BaseURL)
	}
	if client.Token != cfg.Token {
		t.Errorf("Token = %q, want %q", client.Token, cfg.Token)
	}
}

func TestResolvePush_NoIntegration(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, _, _, err := svc.ResolvePush(ctx, scope); err == nil {
		t.Fatal("ResolvePush with no integration: expected error, got nil")
	}
}

func TestCreateNtfy_EncryptsCredentials(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := NtfyConfig{
		BaseURL: "http://localhost:2586",
		Token:   "tk_live_secret",
	}
	in, err := svc.CreateNtfy(ctx, scope, "primary-ntfy", cfg, true)
	if err != nil {
		t.Fatalf("CreateNtfy: %v", err)
	}
	if len(in.EncCredentials) == 0 {
		t.Fatal("EncCredentials is empty")
	}
	if bytes.Contains(in.EncCredentials, []byte(cfg.Token)) {
		t.Fatal("EncCredentials contains plaintext token bytes")
	}
}
