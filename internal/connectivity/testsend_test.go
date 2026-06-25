package connectivity

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/webhook"
)

// TestEmailProviderFromIntegration_ConstructsClient proves the factored helper
// builds the same provider the inline Resolve switch did. We seed via the
// Create* method (which encrypts), reload the full row via GetByID (the only
// path that returns EncCredentials), then call the helper directly.
func TestEmailProviderFromIntegration_ConstructsClient(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := EmailWebhookConfig{WebhookURL: "https://hooks.example.com/email", HMACSecret: "sek"}
	created, err := svc.CreateEmailWebhook(ctx, scope, "email-wh", cfg, true)
	if err != nil {
		t.Fatalf("CreateEmailWebhook: %v", err)
	}
	in, err := svc.store.Integrations().GetByID(ctx, scope, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	prov, from, err := svc.EmailProviderFromIntegration(in)
	if err != nil {
		t.Fatalf("EmailProviderFromIntegration: %v", err)
	}
	if from != "" {
		t.Errorf("from = %q, want empty (webhook has no configured sender)", from)
	}
	client, ok := prov.(*webhook.EmailClient)
	if !ok {
		t.Fatalf("provider type = %T, want *webhook.EmailClient", prov)
	}
	if client.URL != cfg.WebhookURL {
		t.Errorf("URL = %q, want %q", client.URL, cfg.WebhookURL)
	}
}

// TestChatProviderFromIntegration_ConstructsClient mirrors the above for chat.
func TestChatProviderFromIntegration_ConstructsClient(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := ChatWebhookConfig{WebhookURL: "https://hooks.example.com/chat", HMACSecret: ""}
	created, err := svc.CreateChatWebhook(ctx, scope, "chat-wh", cfg, true)
	if err != nil {
		t.Fatalf("CreateChatWebhook: %v", err)
	}
	in, err := svc.store.Integrations().GetByID(ctx, scope, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	prov, err := svc.ChatProviderFromIntegration(in)
	if err != nil {
		t.Fatalf("ChatProviderFromIntegration: %v", err)
	}
	if _, ok := prov.(*webhook.ChatClient); !ok {
		t.Fatalf("provider type = %T, want *webhook.ChatClient", prov)
	}
}

// TestSend_ChatWebhook_EndToEnd proves the whole path: GetByID → decrypt →
// provider → Send. We point a chat-webhook integration at an httptest.Server
// and assert the server received a POST carrying the message content.
func TestSend_ChatWebhook_EndToEnd(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	type capture struct {
		method string
		body   string
	}
	got := make(chan capture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- capture{method: r.Method, body: string(b)}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"srv-1"}`))
	}))
	defer srv.Close()

	created, err := svc.CreateChatWebhook(ctx, scope, "chat-wh",
		ChatWebhookConfig{WebhookURL: srv.URL}, true)
	if err != nil {
		t.Fatalf("CreateChatWebhook: %v", err)
	}

	res, err := svc.TestSend(ctx, scope, created.ID, "", "hello from a notify test")
	if err != nil {
		t.Fatalf("TestSend: %v", err)
	}
	if res.ProviderMessageID != "srv-1" {
		t.Errorf("ProviderMessageID = %q, want srv-1", res.ProviderMessageID)
	}

	c := <-got
	if c.method != http.MethodPost {
		t.Errorf("method = %q, want POST", c.method)
	}
	var payload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(c.body), &payload); err != nil {
		t.Fatalf("unmarshal body %q: %v", c.body, err)
	}
	if payload.Content != "hello from a notify test" {
		t.Errorf("content = %q, want %q", payload.Content, "hello from a notify test")
	}
}

// TestSend_DefaultMessage proves an empty message falls back to the canned
// default rather than sending an empty body.
func TestSend_DefaultMessage(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	created, err := svc.CreateChatWebhook(ctx, scope, "chat-wh",
		ChatWebhookConfig{WebhookURL: srv.URL}, true)
	if err != nil {
		t.Fatalf("CreateChatWebhook: %v", err)
	}

	if _, err := svc.TestSend(ctx, scope, created.ID, "", ""); err != nil {
		t.Fatalf("TestSend: %v", err)
	}
	body := <-got
	if !strings.Contains(body, "test notification") {
		t.Errorf("body %q does not contain the canned default message", body)
	}
}

// TestSend_UnknownIntegration returns an error (no panic) for a missing id.
func TestSend_UnknownIntegration(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.TestSend(ctx, scope, "does-not-exist", "x@example.com", "hi"); err == nil {
		t.Fatal("TestSend with unknown id: want error, got nil")
	}
}
