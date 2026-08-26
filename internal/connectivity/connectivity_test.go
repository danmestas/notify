package connectivity

import (
	"bytes"
	"context"
	"errors"
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

	prov, from, providerID, integrationID, err := svc.ResolveEmail(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if from != cfg.From {
		t.Fatalf("From = %q, want %q", from, cfg.From)
	}
	if providerID != "smtp" {
		t.Errorf("providerID = %q, want smtp", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateSMTP")
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

// TestCreateIntegration_RejectsInvalidIdentifier proves the same H2-class
// fix for integration identifiers: an identifier containing a character
// outside the URL-safe slug allowlist (^[a-z0-9][a-z0-9_-]*$) is rejected
// before any store write, across every Create* provider constructor (they
// all route through the shared createIntegration helper). Previously such an
// identifier was persisted verbatim and used as a URL path segment by every
// console route (view/edit/delete), permanently orphaning the integration
// once it 404'd forever.
func TestCreateIntegration_RejectsInvalidIdentifier(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cases := []string{"foo/bar", "a b", "Foo", "<script>", ""}
	for _, id := range cases {
		t.Run("smtp/"+id, func(t *testing.T) {
			svc := New(openTemp(t), testKey())
			if _, err := svc.CreateSMTP(ctx, scope, id, SMTPConfig{}, false); !errors.Is(err, ErrInvalidIdentifier) {
				t.Fatalf("CreateSMTP(%q) error = %v, want ErrInvalidIdentifier", id, err)
			}
			rows, err := svc.store.Integrations().List(ctx, scope)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("CreateSMTP(%q): integration row was written despite invalid identifier; want no row", id)
			}
		})
		t.Run("discord/"+id, func(t *testing.T) {
			svc := New(openTemp(t), testKey())
			if _, err := svc.CreateDiscord(ctx, scope, id, DiscordConfig{}, false); !errors.Is(err, ErrInvalidIdentifier) {
				t.Fatalf("CreateDiscord(%q) error = %v, want ErrInvalidIdentifier", id, err)
			}
			rows, err := svc.store.Integrations().List(ctx, scope)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("CreateDiscord(%q): integration row was written despite invalid identifier; want no row", id)
			}
		})
	}
}

// TestCreateIntegration_AcceptsValidIdentifier proves the allowlist isn't
// overly strict: lowercase alphanumerics plus hyphen/underscore, starting
// with a letter or digit, is accepted.
func TestCreateIntegration_AcceptsValidIdentifier(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	svc := New(openTemp(t), testKey())

	cases := []string{"a", "smtp-1", "primary_smtp", "smtp123"}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			if _, err := svc.CreateSMTP(ctx, scope, id, SMTPConfig{}, false); err != nil {
				t.Fatalf("CreateSMTP(%q): %v", id, err)
			}
		})
	}
}

func TestResolveEmail_NoIntegration(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, _, _, _, err := svc.ResolveEmail(ctx, scope); err == nil {
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

	prov, from, providerID2, integrationID2, err := svc.ResolveEmail(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if from != cfg.From {
		t.Errorf("From = %q, want %q", from, cfg.From)
	}
	if providerID2 != "cloudflare-email" {
		t.Errorf("providerID = %q, want cloudflare-email", providerID2)
	}
	if integrationID2 == "" {
		t.Error("integrationID must be non-empty after CreateCloudflareEmail")
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

	_, _, _, _, err = svc.ResolveEmail(ctx, scope)
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

// --- Wave 2: Update + ErrPrimaryConflict tests ---

func TestUpdate_SMTPCredentialMerge_SentinelKeepsStored(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	orig := SMTPConfig{
		Addr:     "smtp.example.com:587",
		Host:     "smtp.example.com",
		Username: "user@example.com",
		Password: "original-secret",
		From:     "orig@example.com",
	}
	row, err := svc.CreateSMTP(ctx, scope, "smtp-1", orig, false)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}

	// Update with a new From but leave password as sentinel (unchanged).
	_, err = svc.Update(ctx, scope, row.ID, UpdateInput{
		Identifier: "smtp-1",
		Primary:    false,
		Credentials: map[string]string{
			"from":     "new@example.com",
			"password": CredentialMaskSentinel, // must keep stored value
		},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// ResolveEmail must still work (password preserved).
	prov, from, _, _, err := svc.ResolveEmail(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveEmail after Update: %v", err)
	}
	if from != "new@example.com" {
		t.Errorf("From = %q, want new@example.com", from)
	}
	smtp, ok := prov.(*providers.SMTP)
	if !ok {
		t.Fatalf("provider type = %T", prov)
	}
	if smtp.Password != orig.Password {
		t.Errorf("Password = %q, want original %q (sentinel must preserve stored value)", smtp.Password, orig.Password)
	}
}

func TestUpdate_CredentialsNeverLeakedInReturnValue(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	row, err := svc.CreateSMTP(ctx, scope, "smtp-2", SMTPConfig{
		Addr: "smtp.example.com:587", Host: "smtp.example.com",
		Password: "very-secret-do-not-leak", From: "noreply@example.com",
	}, false)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}

	updated, err := svc.Update(ctx, scope, row.ID, UpdateInput{
		Identifier:  "smtp-2",
		Credentials: map[string]string{"password": CredentialMaskSentinel},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(updated.EncCredentials) != 0 {
		t.Error("Update returned EncCredentials — must be nil for safety")
	}
}

func TestUpdate_ErrPrimaryConflict_WhenAnotherPrimaryExists(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Create a primary row.
	_, err := svc.CreateSMTP(ctx, scope, "smtp-primary", SMTPConfig{
		Addr: "smtp.example.com:587", Host: "smtp.example.com",
		From: "primary@example.com",
	}, true)
	if err != nil {
		t.Fatalf("CreateSMTP primary: %v", err)
	}

	// Create a non-primary row.
	row2, err := svc.CreateSMTP(ctx, scope, "smtp-secondary", SMTPConfig{
		Addr: "smtp2.example.com:587", Host: "smtp2.example.com",
		From: "secondary@example.com",
	}, false)
	if err != nil {
		t.Fatalf("CreateSMTP secondary: %v", err)
	}

	// Attempt to promote secondary to primary — must return ErrPrimaryConflict.
	_, err = svc.Update(ctx, scope, row2.ID, UpdateInput{
		Identifier:  "smtp-secondary",
		Primary:     true,
		Credentials: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected ErrPrimaryConflict, got nil")
	}
	var pc ErrPrimaryConflict
	if !isErrPrimaryConflict(err, &pc) {
		t.Fatalf("expected ErrPrimaryConflict, got %T: %v", err, err)
	}
	if pc.ExistingID == "" {
		t.Error("ErrPrimaryConflict.ExistingID must be populated")
	}
}

func TestUpdate_NoPrimaryConflict_WhenAlreadyPrimary(t *testing.T) {
	// Updating a row that is already primary with primary=true must NOT return
	// ErrPrimaryConflict — it's a no-op on primary_provider.
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	row, err := svc.CreateSMTP(ctx, scope, "smtp-p", SMTPConfig{
		Addr: "smtp.example.com:587", Host: "smtp.example.com",
		From: "p@example.com",
	}, true)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}

	_, err = svc.Update(ctx, scope, row.ID, UpdateInput{
		Identifier:  "smtp-p",
		Primary:     true,
		Credentials: map[string]string{},
	})
	if err != nil {
		t.Errorf("Update already-primary with primary=true: want nil, got %v", err)
	}
}

func TestUpdate_ScopeIsolation_WrongEnvReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	e1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	row, err := svc.CreateSMTP(ctx, e1, "smtp-e1", SMTPConfig{
		Addr: "smtp.example.com:587", Host: "smtp.example.com",
		From: "e1@example.com",
	}, false)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}

	_, err = svc.Update(ctx, e2, row.ID, UpdateInput{
		Identifier:  "hack",
		Credentials: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for cross-environment update, got nil")
	}
}

// TestDecryptedCredentials_ReturnsProviderFields proves chatloop's inbound
// pollers can resolve a Mastodon integration's decrypted creds by provider id
// even though "chat" also has an active Discord integration in the same
// scope — GetActiveByChannel would only ever return one of them.
func TestDecryptedCredentials_ReturnsProviderFields(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.CreateDiscord(ctx, scope, "discord-primary", DiscordConfig{
		WebhookURL: "https://discord.example/webhook",
	}, true); err != nil {
		t.Fatalf("CreateDiscord: %v", err)
	}

	cfg := MastodonConfig{
		BaseURL:     "https://mastodon.example",
		AccessToken: "tok-123",
		Visibility:  "unlisted",
	}
	if _, err := svc.CreateMastodon(ctx, scope, "mastodon-1", cfg, false); err != nil {
		t.Fatalf("CreateMastodon: %v", err)
	}

	fields, err := svc.DecryptedCredentials(ctx, scope, "mastodon")
	if err != nil {
		t.Fatalf("DecryptedCredentials: %v", err)
	}
	if fields["base_url"] != cfg.BaseURL {
		t.Errorf("base_url = %q, want %q", fields["base_url"], cfg.BaseURL)
	}
	if fields["access_token"] != cfg.AccessToken {
		t.Errorf("access_token = %q, want %q", fields["access_token"], cfg.AccessToken)
	}
	if fields["visibility"] != cfg.Visibility {
		t.Errorf("visibility = %q, want %q", fields["visibility"], cfg.Visibility)
	}
}

// TestDecryptedCredentials_NoActiveIntegration_ReturnsErrNotFound proves the
// "provider unconfigured" case fails closed with store.ErrNotFound so
// chatloop's CredentialSource seam can treat it as "poller disabled" rather
// than crashing Start.
func TestDecryptedCredentials_NoActiveIntegration_ReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.DecryptedCredentials(ctx, scope, "bluesky"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DecryptedCredentials: err = %v, want store.ErrNotFound", err)
	}
}

// TestDecryptedCredentials_InactiveIntegrationIgnored proves a deactivated
// row for the requested provider is treated the same as "no integration" —
// SetActive(false) must actually disable resolution, not just cosmetically
// mark the row.
func TestDecryptedCredentials_InactiveIntegrationIgnored(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	row, err := svc.CreateBluesky(ctx, scope, "bsky-1", BlueskyConfig{
		PDSURL: "https://bsky.social", Identifier: "user.bsky.social", AppPassword: "app-pass",
	}, false)
	if err != nil {
		t.Fatalf("CreateBluesky: %v", err)
	}
	if err := svc.Store().Integrations().SetActive(ctx, scope, row.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	if _, err := svc.DecryptedCredentials(ctx, scope, "bluesky"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DecryptedCredentials: err = %v, want store.ErrNotFound", err)
	}
}

// isErrPrimaryConflict unwraps ErrPrimaryConflict from err using errors.As and
// writes it into out. errors.As works on value receivers when the target is a
// non-nil pointer to the value type, so *ErrPrimaryConflict is the correct form.
func isErrPrimaryConflict(err error, out *ErrPrimaryConflict) bool {
	var pc ErrPrimaryConflict
	if errors.As(err, &pc) {
		*out = pc
		return true
	}
	return false
}
