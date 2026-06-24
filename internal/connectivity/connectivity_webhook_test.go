package connectivity

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/webhook"
)

func TestCreateEmailWebhook_ThenResolveEmail(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := EmailWebhookConfig{
		WebhookURL: "https://hooks.example.com/email",
		HMACSecret: "sekret",
	}
	if _, err := svc.CreateEmailWebhook(ctx, scope, "email-wh", cfg, true); err != nil {
		t.Fatalf("CreateEmailWebhook: %v", err)
	}

	prov, from, providerID, integrationID, err := svc.ResolveEmail(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if providerID != "email-webhook" {
		t.Errorf("providerID = %q, want email-webhook", providerID)
	}
	if from != "" {
		t.Errorf("from = %q, want empty (webhook has no configured sender)", from)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateEmailWebhook")
	}
	client, ok := prov.(*webhook.EmailClient)
	if !ok {
		t.Fatalf("provider type = %T, want *webhook.EmailClient", prov)
	}
	if client.URL != cfg.WebhookURL {
		t.Errorf("URL = %q, want %q", client.URL, cfg.WebhookURL)
	}
	if client.HMACSecret != cfg.HMACSecret {
		t.Errorf("HMACSecret = %q, want %q", client.HMACSecret, cfg.HMACSecret)
	}
}

func TestCreatePushWebhook_ThenResolvePush(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := PushWebhookConfig{
		WebhookURL: "https://hooks.example.com/push",
		HMACSecret: "",
	}
	if _, err := svc.CreatePushWebhook(ctx, scope, "push-wh", cfg, true); err != nil {
		t.Fatalf("CreatePushWebhook: %v", err)
	}

	prov, providerID, integrationID, err := svc.ResolvePush(ctx, scope)
	if err != nil {
		t.Fatalf("ResolvePush: %v", err)
	}
	if providerID != "push-webhook" {
		t.Errorf("providerID = %q, want push-webhook", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreatePushWebhook")
	}
	client, ok := prov.(*webhook.PushClient)
	if !ok {
		t.Fatalf("provider type = %T, want *webhook.PushClient", prov)
	}
	if client.URL != cfg.WebhookURL {
		t.Errorf("URL = %q, want %q", client.URL, cfg.WebhookURL)
	}
	if client.HMACSecret != "" {
		t.Errorf("HMACSecret = %q, want empty", client.HMACSecret)
	}
}

func TestCreateChatWebhook_ThenResolveChat(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := ChatWebhookConfig{
		WebhookURL: "https://hooks.example.com/chat",
		HMACSecret: "chatsekret",
	}
	if _, err := svc.CreateChatWebhook(ctx, scope, "chat-wh", cfg, true); err != nil {
		t.Fatalf("CreateChatWebhook: %v", err)
	}

	prov, providerID, integrationID, err := svc.ResolveChat(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveChat: %v", err)
	}
	if providerID != "chat-webhook" {
		t.Errorf("providerID = %q, want chat-webhook", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateChatWebhook")
	}
	client, ok := prov.(*webhook.ChatClient)
	if !ok {
		t.Fatalf("provider type = %T, want *webhook.ChatClient", prov)
	}
	if client.URL != cfg.WebhookURL {
		t.Errorf("URL = %q, want %q", client.URL, cfg.WebhookURL)
	}
	if client.HMACSecret != cfg.HMACSecret {
		t.Errorf("HMACSecret = %q, want %q", client.HMACSecret, cfg.HMACSecret)
	}
}
