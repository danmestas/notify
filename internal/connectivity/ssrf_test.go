package connectivity

import (
	"context"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
)

// TestAuthedGET_BlocksPrivateIP proves authedGET (the shared credential-probe
// GET helper used by probeNtfy and probeCloudflareEmail) routes through the
// SSRF guard: a tenant-supplied BaseURL pointing at an RFC1918 address must be
// rejected at dial time with the guard's "ssrf guard:" error, not silently
// dialed via a plain client.
func TestAuthedGET_BlocksPrivateIP(t *testing.T) {
	client := httpaction.NewSSRFSafeClientWithTimeout(probeTimeout)
	_, _, err := authedGET(context.Background(), client, "http://10.0.0.1/v1/health", "")
	if err == nil {
		t.Fatal("authedGET: expected SSRF guard error for RFC1918 10.0.0.1, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard:") {
		t.Errorf("authedGET error = %v, want it to contain %q", err, "ssrf guard:")
	}
}

// TestAuthedGET_BlocksLoopback proves the same guard fires for a loopback
// target such as an internal admin port a tenant might probe for.
func TestAuthedGET_BlocksLoopback(t *testing.T) {
	client := httpaction.NewSSRFSafeClientWithTimeout(probeTimeout)
	_, _, err := authedGET(context.Background(), client, "http://127.0.0.1:9999/", "")
	if err == nil {
		t.Fatal("authedGET: expected SSRF guard error for loopback 127.0.0.1, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard:") {
		t.Errorf("authedGET error = %v, want it to contain %q", err, "ssrf guard:")
	}
}

// TestVerifyByID_Ntfy_BlocksPrivateIP proves the production wiring: VerifyByID
// routes the ntfy probe through svc.probeClient(), which defaults to the
// SSRF-safe client. A tenant-supplied BaseURL resolving to an RFC1918 address
// must be rejected, surfacing as a negative (not error) VerifyResult per
// VerifyByID's "reached the provider but rejected" contract — the SSRF block
// and an auth rejection both flow through the same ok=false path.
func TestVerifyByID_Ntfy_BlocksPrivateIP(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	in, err := svc.CreateNtfy(ctx, scope, "ntfy-1", NtfyConfig{BaseURL: "http://10.0.0.1", Token: "tok"}, true)
	if err != nil {
		t.Fatalf("CreateNtfy: %v", err)
	}

	res, err := svc.VerifyByID(ctx, scope, in.ID)
	if err != nil {
		t.Fatalf("VerifyByID: %v", err)
	}
	if res.OK {
		t.Error("ntfy verify OK = true for RFC1918 BaseURL, want false (SSRF-blocked)")
	}
	if !strings.Contains(res.Detail, "ssrf guard:") {
		t.Errorf("detail = %q, want it to contain %q", res.Detail, "ssrf guard:")
	}
}

// TestChatWebhook_TestSend_BlocksPrivateIP proves the production wiring: a
// chat-webhook integration whose tenant-supplied WebhookURL resolves to an
// RFC1918 address is rejected by the SSRF guard when actually sent, not just
// when the client struct is constructed. This exercises the full path —
// ChatProviderFromIntegration builds the client via svc.deliveryClient(), which
// defaults to the guarded client unless a test overrides
// svc.deliveryHTTPClient (see testsend_test.go for the loopback-server tests
// that intentionally opt out).
func TestChatWebhook_TestSend_BlocksPrivateIP(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := svc.CreateChatWebhook(ctx, scope, "chat-wh",
		ChatWebhookConfig{WebhookURL: "http://10.0.0.1/hook"}, true)
	if err != nil {
		t.Fatalf("CreateChatWebhook: %v", err)
	}

	_, err = svc.TestSend(ctx, scope, created.ID, "", "hello")
	if err == nil {
		t.Fatal("TestSend: expected SSRF guard error for RFC1918 webhook URL, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard:") {
		t.Errorf("TestSend error = %v, want it to contain %q", err, "ssrf guard:")
	}
}

// TestNtfy_TestSend_BlocksPrivateIP proves the ntfy PUSH DELIVERY path is
// guarded: a tenant-supplied BaseURL resolving to an RFC1918 address is
// rejected by the SSRF guard when TestSend actually POSTs the push, not just
// on the VerifyByID probe. PushProviderFromIntegration builds ntfy.Client with
// svc.deliveryClient().
func TestNtfy_TestSend_BlocksPrivateIP(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := svc.CreateNtfy(ctx, scope, "ntfy-1",
		NtfyConfig{BaseURL: "http://10.0.0.1", Token: "tok"}, true)
	if err != nil {
		t.Fatalf("CreateNtfy: %v", err)
	}

	// destination is the ntfy topic; the guard fires at dial time regardless.
	_, err = svc.TestSend(ctx, scope, created.ID, "test-topic", "hello")
	if err == nil {
		t.Fatal("TestSend: expected SSRF guard error for RFC1918 ntfy BaseURL, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard:") {
		t.Errorf("TestSend error = %v, want it to contain %q", err, "ssrf guard:")
	}
}

// TestDiscord_TestSend_BlocksLinkLocal proves the discord CHAT DELIVERY path is
// guarded: a tenant-supplied WebhookURL pointing at the cloud-metadata
// link-local address (169.254.169.254) is rejected by the SSRF guard when
// TestSend actually POSTs the message. ChatProviderFromIntegration builds
// discord.Client with svc.deliveryClient().
func TestDiscord_TestSend_BlocksLinkLocal(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := svc.CreateDiscord(ctx, scope, "discord-1",
		DiscordConfig{WebhookURL: "http://169.254.169.254/api/webhooks/1/tok"}, true)
	if err != nil {
		t.Fatalf("CreateDiscord: %v", err)
	}

	_, err = svc.TestSend(ctx, scope, created.ID, "", "hello")
	if err == nil {
		t.Fatal("TestSend: expected SSRF guard error for link-local discord WebhookURL, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard:") {
		t.Errorf("TestSend error = %v, want it to contain %q", err, "ssrf guard:")
	}
}

// TestSignalWire_TestSend_BlocksPrivateIP proves the signalwire SMS DELIVERY
// path is guarded: SpaceURL is a tenant-supplied per-account host, so one
// resolving to an RFC1918 address must be rejected by the SSRF guard when
// TestSend actually POSTs the message. SMSProviderFromIntegration builds
// signalwire.Client with svc.deliveryClient().
func TestSignalWire_TestSend_BlocksPrivateIP(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := svc.CreateSignalWire(ctx, scope, "sw-1", SignalWireConfig{
		ProjectID: "proj",
		Token:     "tok",
		SpaceURL:  "http://10.0.0.1",
		From:      "+15550001111",
	}, true)
	if err != nil {
		t.Fatalf("CreateSignalWire: %v", err)
	}

	_, err = svc.TestSend(ctx, scope, created.ID, "+15558675309", "hello")
	if err == nil {
		t.Fatal("TestSend: expected SSRF guard error for RFC1918 signalwire SpaceURL, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard:") {
		t.Errorf("TestSend error = %v, want it to contain %q", err, "ssrf guard:")
	}
}
