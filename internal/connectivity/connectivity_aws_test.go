package connectivity

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/ses"
	"github.com/danmestas/notify/internal/providers/sns"
)

// TestCreateSES_ThenResolveEmail proves the SES credentials survive seal/unseal
// and the resolved provider carries the access key, secret, region, and From.
func TestCreateSES_ThenResolveEmail(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := SESConfig{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		Region:          "us-east-1",
		From:            "no-reply@example.com",
	}
	if _, err := svc.CreateSES(ctx, scope, "primary-ses", cfg, true); err != nil {
		t.Fatalf("CreateSES: %v", err)
	}

	prov, from, providerID, integrationID, err := svc.ResolveEmail(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if from != cfg.From {
		t.Errorf("From = %q, want %q", from, cfg.From)
	}
	if providerID != "ses" {
		t.Errorf("providerID = %q, want ses", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateSES")
	}
	c, ok := prov.(*ses.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *ses.Client", prov)
	}
	if c.AccessKeyID != cfg.AccessKeyID {
		t.Errorf("AccessKeyID = %q, want %q", c.AccessKeyID, cfg.AccessKeyID)
	}
	if c.SecretAccessKey != cfg.SecretAccessKey {
		t.Errorf("SecretAccessKey = %q, want %q", c.SecretAccessKey, cfg.SecretAccessKey)
	}
	if c.Region != cfg.Region {
		t.Errorf("Region = %q, want %q", c.Region, cfg.Region)
	}
	if c.From != cfg.From {
		t.Errorf("From = %q, want %q", c.From, cfg.From)
	}
}

// TestCreateSNS_ThenResolveSMS proves the SNS credentials survive seal/unseal
// and the resolved provider carries the access key, secret, and region.
func TestCreateSNS_ThenResolveSMS(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cfg := SNSConfig{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		Region:          "us-east-1",
	}
	if _, err := svc.CreateSNS(ctx, scope, "primary-sns", cfg, true); err != nil {
		t.Fatalf("CreateSNS: %v", err)
	}

	prov, providerID, integrationID, err := svc.ResolveSMS(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveSMS: %v", err)
	}
	if providerID != "sns" {
		t.Errorf("providerID = %q, want sns", providerID)
	}
	if integrationID == "" {
		t.Error("integrationID must be non-empty after CreateSNS")
	}
	c, ok := prov.(*sns.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *sns.Client", prov)
	}
	if c.AccessKeyID != cfg.AccessKeyID {
		t.Errorf("AccessKeyID = %q, want %q", c.AccessKeyID, cfg.AccessKeyID)
	}
	if c.SecretAccessKey != cfg.SecretAccessKey {
		t.Errorf("SecretAccessKey = %q, want %q", c.SecretAccessKey, cfg.SecretAccessKey)
	}
	if c.Region != cfg.Region {
		t.Errorf("Region = %q, want %q", c.Region, cfg.Region)
	}
}
