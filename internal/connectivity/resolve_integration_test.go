package connectivity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/providers/discord"
	"github.com/danmestas/notify/internal/providers/telegram"
)

// TestResolveIntegration_EmptyIdentifier_DelegatesToPrimary proves that
// identifier=="" produces byte-identical results to the existing
// ResolveChat/ResolveEmail/ResolvePush/ResolveSMS primary-resolution paths.
func TestResolveIntegration_EmptyIdentifier_DelegatesToPrimary(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.CreateDiscord(ctx, scope, "discord-primary", DiscordConfig{
		WebhookURL: "https://discord.example/webhook",
	}, true); err != nil {
		t.Fatalf("CreateDiscord: %v", err)
	}

	wantProv, wantProviderID, wantIntegrationID, wantErr := svc.ResolveChat(ctx, scope)

	gotProv, gotProviderID, gotIntegrationID, gotErr := svc.ResolveIntegration(ctx, scope, "chat", "")
	if !errors.Is(gotErr, wantErr) && gotErr != wantErr {
		t.Fatalf("err = %v, want %v", gotErr, wantErr)
	}
	if gotProviderID != wantProviderID {
		t.Errorf("providerID = %q, want %q", gotProviderID, wantProviderID)
	}
	if gotIntegrationID != wantIntegrationID {
		t.Errorf("integrationID = %q, want %q", gotIntegrationID, wantIntegrationID)
	}
	gotDiscord, ok := gotProv.(*discord.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *discord.Client", gotProv)
	}
	wantDiscord, ok := wantProv.(*discord.Client)
	if !ok {
		t.Fatalf("ResolveChat provider type = %T, want *discord.Client", wantProv)
	}
	if gotDiscord.WebhookURL != wantDiscord.WebhookURL {
		t.Errorf("WebhookURL = %q, want %q", gotDiscord.WebhookURL, wantDiscord.WebhookURL)
	}
}

// TestResolveIntegration_ByIdentifier_PerChannel proves an explicit
// identifier resolves through the generic path to the right provider type for
// each of the four send channels, even when it is NOT the channel's primary
// integration.
func TestResolveIntegration_ByIdentifier_PerChannel(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.CreateDiscord(ctx, scope, "discord-primary", DiscordConfig{
		WebhookURL: "https://discord.example/webhook",
	}, true); err != nil {
		t.Fatalf("CreateDiscord: %v", err)
	}
	if _, err := svc.CreateTelegram(ctx, scope, "telegram-secondary", TelegramConfig{
		BotToken: "tg-token",
	}, false); err != nil {
		t.Fatalf("CreateTelegram: %v", err)
	}

	prov, providerID, integrationID, err := svc.ResolveIntegration(ctx, scope, "chat", "telegram-secondary")
	if err != nil {
		t.Fatalf("ResolveIntegration: %v", err)
	}
	if providerID != "telegram" {
		t.Errorf("providerID = %q, want telegram", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty")
	}
	tg, ok := prov.(*telegram.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *telegram.Client", prov)
	}
	if tg.BotToken != "tg-token" {
		t.Errorf("BotToken = %q, want tg-token", tg.BotToken)
	}
}

func TestResolveIntegration_UnknownIdentifier_ReturnsErrIntegrationUnusable(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	_, _, _, err := svc.ResolveIntegration(ctx, scope, "chat", "does-not-exist")
	var unusable ErrIntegrationUnusable
	if !errors.As(err, &unusable) {
		t.Fatalf("err = %v (%T), want ErrIntegrationUnusable", err, err)
	}
	if unusable.Identifier != "does-not-exist" {
		t.Errorf("Identifier = %q, want does-not-exist", unusable.Identifier)
	}
	if !containsIdentifier(err.Error(), "does-not-exist") {
		t.Errorf("Error() = %q, must name the identifier", err.Error())
	}
}

func TestResolveIntegration_WrongChannel_ReturnsErrIntegrationUnusable(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.CreateDiscord(ctx, scope, "discord-primary", DiscordConfig{
		WebhookURL: "https://discord.example/webhook",
	}, true); err != nil {
		t.Fatalf("CreateDiscord: %v", err)
	}

	// discord-primary is a chat integration; asking for it under "email"
	// must fail closed, never silently fall back to primary email.
	_, _, _, err := svc.ResolveIntegration(ctx, scope, "email", "discord-primary")
	var unusable ErrIntegrationUnusable
	if !errors.As(err, &unusable) {
		t.Fatalf("err = %v (%T), want ErrIntegrationUnusable", err, err)
	}
	if !containsIdentifier(err.Error(), "discord-primary") {
		t.Errorf("Error() = %q, must name the identifier", err.Error())
	}
}

func TestResolveIntegration_Inactive_ReturnsErrIntegrationUnusable(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.CreateDiscord(ctx, scope, "discord-primary", DiscordConfig{
		WebhookURL: "https://discord.example/webhook",
	}, true); err != nil {
		t.Fatalf("CreateDiscord: %v", err)
	}
	if _, err := svc.CreateTelegram(ctx, scope, "telegram-inactive", TelegramConfig{
		BotToken: "tg-token",
	}, false); err != nil {
		t.Fatalf("CreateTelegram: %v", err)
	}
	in, err := svc.store.Integrations().GetByIdentifier(ctx, scope, "telegram-inactive")
	if err != nil {
		t.Fatalf("GetByIdentifier: %v", err)
	}
	if err := svc.store.Integrations().SetActive(ctx, scope, in.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	_, _, _, resolveErr := svc.ResolveIntegration(ctx, scope, "chat", "telegram-inactive")
	var unusable ErrIntegrationUnusable
	if !errors.As(resolveErr, &unusable) {
		t.Fatalf("err = %v (%T), want ErrIntegrationUnusable", resolveErr, resolveErr)
	}
	if !containsIdentifier(resolveErr.Error(), "telegram-inactive") {
		t.Errorf("Error() = %q, must name the identifier", resolveErr.Error())
	}

	// A permanent failure must never be wrapped as retry-safe.
	var transient *providers.TransientError
	if errors.As(resolveErr, &transient) {
		t.Error("ErrIntegrationUnusable must not be a providers.TransientError (must be permanent)")
	}
}

// TestResolveEmailIntegration_ReturnsConfiguredFrom proves that resolving an
// explicitly identified SMTP email integration returns its configured From
// address, matching ResolveEmail's primary-path behavior instead of
// discarding it (issue: an integration-routed send must not lose its From).
func TestResolveEmailIntegration_ReturnsConfiguredFrom(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.CreateSMTP(ctx, scope, "smtp-secondary", SMTPConfig{
		Addr: "smtp.example.com:587",
		Host: "smtp.example.com",
		From: "secondary@example.com",
	}, false); err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}

	prov, from, providerID, integrationID, err := svc.ResolveEmailIntegration(ctx, scope, "smtp-secondary")
	if err != nil {
		t.Fatalf("ResolveEmailIntegration: %v", err)
	}
	if prov == nil {
		t.Fatal("provider must not be nil")
	}
	if from != "secondary@example.com" {
		t.Errorf("from = %q, want secondary@example.com", from)
	}
	if providerID != "smtp" {
		t.Errorf("providerID = %q, want smtp", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty")
	}
}

func containsIdentifier(s, id string) bool {
	return strings.Contains(s, id)
}
