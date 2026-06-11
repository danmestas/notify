package connectivity

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/providers/cloudflareemail"
	"github.com/danmestas/notify/internal/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestCreateSMTP_ThenResolveEmail(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := SMTPConfig{
		Addr:     "smtp.example.com:587",
		Host:     "smtp.example.com",
		Username: "user@example.com",
		Password: "s3cret-pass",
		From:     "noreply@example.com",
	}
	if _, err := svc.CreateSMTP(ctx, scope, "primary-smtp", cfg, true); err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}

	prov, from, err := svc.ResolveEmail(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if from != cfg.From {
		t.Fatalf("From = %q, want %q", from, cfg.From)
	}
	smtp, ok := prov.(*providers.SMTP)
	if !ok {
		t.Fatalf("provider type = %T, want *providers.SMTP", prov)
	}
	if smtp.Addr != cfg.Addr {
		t.Errorf("Addr = %q, want %q", smtp.Addr, cfg.Addr)
	}
	if smtp.Host != cfg.Host {
		t.Errorf("Host = %q, want %q", smtp.Host, cfg.Host)
	}
	if smtp.Username != cfg.Username {
		t.Errorf("Username = %q, want %q", smtp.Username, cfg.Username)
	}
	if smtp.Password != cfg.Password {
		t.Errorf("Password = %q, want %q", smtp.Password, cfg.Password)
	}
}

func TestResolveEmail_NoIntegration(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, _, err := svc.ResolveEmail(ctx, scope); err == nil {
		t.Fatal("ResolveEmail with no integration: expected error, got nil")
	}
}

func TestCreateSMTP_EncryptsCredentials(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := SMTPConfig{
		Addr:     "smtp.example.com:587",
		Host:     "smtp.example.com",
		Username: "user@example.com",
		Password: "plaintext-password-leak-check",
		From:     "noreply@example.com",
	}
	in, err := svc.CreateSMTP(ctx, scope, "primary-smtp", cfg, true)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}
	if len(in.EncCredentials) == 0 {
		t.Fatal("EncCredentials is empty")
	}
	if bytes.Contains(in.EncCredentials, []byte(cfg.Password)) {
		t.Fatal("EncCredentials contains plaintext password bytes")
	}
}

func TestCreateCloudflareEmail_ThenResolveEmail(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := CloudflareEmailConfig{
		AccountID: "cf-acct-001",
		APIToken:  "cf-token-secret",
		From:      "notify@example.com",
	}
	in, err := svc.CreateCloudflareEmail(ctx, scope, "primary-cf", cfg, true)
	if err != nil {
		t.Fatalf("CreateCloudflareEmail: %v", err)
	}
	if in.ProviderID != "cloudflare-email" {
		t.Errorf("ProviderID = %q, want cloudflare-email", in.ProviderID)
	}
	if in.Channel != "email" {
		t.Errorf("Channel = %q, want email", in.Channel)
	}

	prov, from, err := svc.ResolveEmail(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if from != cfg.From {
		t.Errorf("From = %q, want %q", from, cfg.From)
	}
	cfClient, ok := prov.(*cloudflareemail.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *cloudflareemail.Client", prov)
	}
	if cfClient.AccountID != cfg.AccountID {
		t.Errorf("AccountID = %q, want %q", cfClient.AccountID, cfg.AccountID)
	}
	if cfClient.APIToken != cfg.APIToken {
		t.Errorf("APIToken = %q, want %q", cfClient.APIToken, cfg.APIToken)
	}
	if cfClient.From != cfg.From {
		t.Errorf("From = %q, want %q", cfClient.From, cfg.From)
	}
}

func TestResolveEmail_UnknownProviderFailsClosed(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Insert an email integration with an unrecognised ProviderID directly via
	// the store (no Create* helper exists for it, by design).
	enc, err := svc.encrypt([]byte(`{"some":"opaque-config"}`))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := svc.store.Integrations().Create(ctx, scope, domain.Integration{
		ProviderID:     "bogus",
		Channel:        "email",
		EncCredentials: enc,
		Active:         true,
		Primary:        true,
		Identifier:     "bogus-email",
	}); err != nil {
		t.Fatalf("Integrations().Create: %v", err)
	}

	_, _, err = svc.ResolveEmail(ctx, scope)
	if err == nil {
		t.Fatal("ResolveEmail with unknown ProviderID: expected error, got nil")
	}
	if want := `connectivity: unknown email provider "bogus"`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestCreateCloudflareEmail_EncryptsCredentials(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := CloudflareEmailConfig{
		AccountID: "cf-acct-001",
		APIToken:  "plaintext-token-leak-check",
		From:      "notify@example.com",
	}
	in, err := svc.CreateCloudflareEmail(ctx, scope, "primary-cf", cfg, true)
	if err != nil {
		t.Fatalf("CreateCloudflareEmail: %v", err)
	}
	if len(in.EncCredentials) == 0 {
		t.Fatal("EncCredentials is empty")
	}
	if bytes.Contains(in.EncCredentials, []byte(cfg.APIToken)) {
		t.Fatal("EncCredentials contains plaintext API token bytes")
	}
}
