package connectivity

import (
	"bytes"
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/signalwire"
	"github.com/danmestas/notify/internal/providers/twilio"
)

func TestCreateTwilio_ThenResolveSMS(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := TwilioConfig{
		AccountSID: "AC_live_sid",
		AuthToken:  "tok_live_secret",
		From:       "+15550001111",
	}
	if _, err := svc.CreateTwilio(ctx, scope, "primary-twilio", cfg, true); err != nil {
		t.Fatalf("CreateTwilio: %v", err)
	}

	provider, providerID, integrationID, err := svc.ResolveSMS(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveSMS: %v", err)
	}
	if providerID != "twilio" {
		t.Errorf("providerID = %q, want %q", providerID, "twilio")
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateTwilio")
	}
	client, ok := provider.(*twilio.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *twilio.Client", provider)
	}
	if client.AccountSID != cfg.AccountSID || client.AuthToken != cfg.AuthToken || client.From != cfg.From {
		t.Errorf("client = %+v, want creds round-tripped from cfg %+v", client, cfg)
	}
}

func TestCreateSignalWire_ThenResolveSMS(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := SignalWireConfig{
		ProjectID: "proj_live",
		Token:     "tok_live_secret",
		SpaceURL:  "example.signalwire.com",
		From:      "+15550001111",
	}
	if _, err := svc.CreateSignalWire(ctx, scope, "primary-signalwire", cfg, true); err != nil {
		t.Fatalf("CreateSignalWire: %v", err)
	}

	provider, providerID, _, err := svc.ResolveSMS(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveSMS: %v", err)
	}
	if providerID != "signalwire" {
		t.Errorf("providerID = %q, want %q", providerID, "signalwire")
	}
	client, ok := provider.(*signalwire.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *signalwire.Client", provider)
	}
	if client.ProjectID != cfg.ProjectID || client.Token != cfg.Token || client.SpaceURL != cfg.SpaceURL || client.From != cfg.From {
		t.Errorf("client = %+v, want creds round-tripped from cfg %+v", client, cfg)
	}
}

func TestResolveSMS_NoIntegration(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, _, _, err := svc.ResolveSMS(ctx, scope); err == nil {
		t.Fatal("ResolveSMS with no integration: expected error, got nil")
	}
}

// TestResolveSMS_ScopeIsolation: an integration created in one scope is not
// resolved in another scope.
func TestResolveSMS_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o2", EnvID: "e2"}

	if _, err := svc.CreateTwilio(ctx, scopeA, "twilio-a", TwilioConfig{
		AccountSID: "AC", AuthToken: "t", From: "+1",
	}, true); err != nil {
		t.Fatalf("CreateTwilio: %v", err)
	}

	if _, _, _, err := svc.ResolveSMS(ctx, scopeB); err == nil {
		t.Fatal("ResolveSMS in foreign scope: expected error, got nil")
	}
}

func TestCreateTwilio_EncryptsCredentials(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := TwilioConfig{AccountSID: "AC", AuthToken: "tok_live_secret", From: "+1"}
	in, err := svc.CreateTwilio(ctx, scope, "twilio", cfg, true)
	if err != nil {
		t.Fatalf("CreateTwilio: %v", err)
	}
	if len(in.EncCredentials) == 0 {
		t.Fatal("EncCredentials is empty")
	}
	if bytes.Contains(in.EncCredentials, []byte(cfg.AuthToken)) {
		t.Fatal("EncCredentials contains plaintext auth token bytes")
	}
}
