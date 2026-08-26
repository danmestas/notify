package connectivity

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/bluesky"
	"github.com/danmestas/notify/internal/providers/mastodon"
	"github.com/danmestas/notify/internal/providers/resend"
	"github.com/danmestas/notify/internal/providers/slack"
	"github.com/danmestas/notify/internal/providers/whatsapp"
)

// TestCreateResend_ThenResolveEmail proves the Resend credentials survive
// seal/unseal and the resolved provider carries the api key and From.
func TestCreateResend_ThenResolveEmail(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := ResendConfig{APIKey: "re_secret_key", From: "no-reply@example.com"}
	if _, err := svc.CreateResend(ctx, scope, "primary-resend", cfg, true); err != nil {
		t.Fatalf("CreateResend: %v", err)
	}

	prov, from, providerID, integrationID, err := svc.ResolveEmail(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if from != cfg.From {
		t.Errorf("From = %q, want %q", from, cfg.From)
	}
	if providerID != "resend" {
		t.Errorf("providerID = %q, want resend", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateResend")
	}
	c, ok := prov.(*resend.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *resend.Client", prov)
	}
	if c.APIKey != cfg.APIKey {
		t.Errorf("APIKey = %q, want %q", c.APIKey, cfg.APIKey)
	}
	if c.From != cfg.From {
		t.Errorf("From = %q, want %q", c.From, cfg.From)
	}
}

// TestCreateSlack_ThenResolveChat proves the Slack bot token survives
// seal/unseal and the resolved provider carries it.
func TestCreateSlack_ThenResolveChat(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := SlackConfig{BotToken: "xoxb-secret"}
	if _, err := svc.CreateSlack(ctx, scope, "primary-slack", cfg, true); err != nil {
		t.Fatalf("CreateSlack: %v", err)
	}

	prov, providerID, integrationID, err := svc.ResolveChat(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveChat: %v", err)
	}
	if providerID != "slack" {
		t.Errorf("providerID = %q, want slack", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateSlack")
	}
	c, ok := prov.(*slack.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *slack.Client", prov)
	}
	if c.BotToken != cfg.BotToken {
		t.Errorf("BotToken = %q, want %q", c.BotToken, cfg.BotToken)
	}
}

// TestCreateWhatsApp_ThenResolveChat proves the WhatsApp credentials survive
// seal/unseal and the resolved provider carries the access token and phone id.
func TestCreateWhatsApp_ThenResolveChat(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := WhatsAppConfig{AccessToken: "EAAG_secret", PhoneNumberID: "109999"}
	if _, err := svc.CreateWhatsApp(ctx, scope, "primary-wa", cfg, true); err != nil {
		t.Fatalf("CreateWhatsApp: %v", err)
	}

	prov, providerID, integrationID, err := svc.ResolveChat(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveChat: %v", err)
	}
	if providerID != "whatsapp" {
		t.Errorf("providerID = %q, want whatsapp", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateWhatsApp")
	}
	c, ok := prov.(*whatsapp.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *whatsapp.Client", prov)
	}
	if c.AccessToken != cfg.AccessToken {
		t.Errorf("AccessToken = %q, want %q", c.AccessToken, cfg.AccessToken)
	}
	if c.PhoneNumberID != cfg.PhoneNumberID {
		t.Errorf("PhoneNumberID = %q, want %q", c.PhoneNumberID, cfg.PhoneNumberID)
	}
}

// TestCreateMastodon_ThenResolveChat proves the Mastodon credentials survive
// seal/unseal and the resolved provider carries the base url, access token,
// and visibility.
func TestCreateMastodon_ThenResolveChat(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := MastodonConfig{BaseURL: "https://mastodon.social", AccessToken: "secret-token", Visibility: "unlisted"}
	if _, err := svc.CreateMastodon(ctx, scope, "primary-mastodon", cfg, true); err != nil {
		t.Fatalf("CreateMastodon: %v", err)
	}

	prov, providerID, integrationID, err := svc.ResolveChat(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveChat: %v", err)
	}
	if providerID != "mastodon" {
		t.Errorf("providerID = %q, want mastodon", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateMastodon")
	}
	c, ok := prov.(*mastodon.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *mastodon.Client", prov)
	}
	if c.BaseURL != cfg.BaseURL {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, cfg.BaseURL)
	}
	if c.AccessToken != cfg.AccessToken {
		t.Errorf("AccessToken = %q, want %q", c.AccessToken, cfg.AccessToken)
	}
	if c.Visibility != cfg.Visibility {
		t.Errorf("Visibility = %q, want %q", c.Visibility, cfg.Visibility)
	}
}

// TestCreateBluesky_ThenResolveChat proves the Bluesky credentials survive
// seal/unseal and the resolved provider carries the PDS URL, identifier, and
// app password.
func TestCreateBluesky_ThenResolveChat(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := BlueskyConfig{PDSURL: "https://bsky.social", Identifier: "alice.bsky.social", AppPassword: "secret-app-pw"}
	if _, err := svc.CreateBluesky(ctx, scope, "primary-bluesky", cfg, true); err != nil {
		t.Fatalf("CreateBluesky: %v", err)
	}

	prov, providerID, integrationID, err := svc.ResolveChat(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveChat: %v", err)
	}
	if providerID != "bluesky" {
		t.Errorf("providerID = %q, want bluesky", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateBluesky")
	}
	c, ok := prov.(*bluesky.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *bluesky.Client", prov)
	}
	if c.PDSURL != cfg.PDSURL {
		t.Errorf("PDSURL = %q, want %q", c.PDSURL, cfg.PDSURL)
	}
	if c.Identifier != cfg.Identifier {
		t.Errorf("Identifier = %q, want %q", c.Identifier, cfg.Identifier)
	}
	if c.AppPassword != cfg.AppPassword {
		t.Errorf("AppPassword = %q, want %q", c.AppPassword, cfg.AppPassword)
	}
}
