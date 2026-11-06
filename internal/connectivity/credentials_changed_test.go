package connectivity

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestOnCredentialsChanged_CreateAndCredentialUpdate(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	var heard []string
	svc.OnCredentialsChanged(func(_ context.Context, s domain.Scope, id string) {
		if s != scope {
			t.Errorf("scope = %+v", s)
		}
		heard = append(heard, id)
	})

	row, err := svc.CreateCloudflareEmail(ctx, scope, "cf-mail", CloudflareEmailConfig{
		AccountID: "acct", APIToken: "old-token", From: "a@example.com",
	}, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(heard) != 1 || heard[0] != row.ID {
		t.Fatalf("after create heard %v, want [%s]", heard, row.ID)
	}

	// Renaming, or a form that leaves every secret masked, is no credential
	// change.
	if _, err := svc.Update(ctx, scope, row.ID, UpdateInput{
		Identifier: "cf-mail-2", Primary: true,
		Credentials: map[string]string{"api_token": CredentialMaskSentinel},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(heard) != 1 {
		t.Fatalf("a non-credential update was reported: %v", heard)
	}

	if _, err := svc.Update(ctx, scope, row.ID, UpdateInput{
		Identifier: "cf-mail-2", Primary: true,
		Credentials: map[string]string{"api_token": "new-token"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(heard) != 2 || heard[1] != row.ID {
		t.Fatalf("after credential update heard %v", heard)
	}
}

func TestVerifyByID_UnsupportedProviderSaysSo(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	row, err := svc.CreateTwilio(ctx, scope, "sms", TwilioConfig{AccountSID: "AC1", AuthToken: "t", From: "+1"}, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := svc.VerifyByID(ctx, scope, row.ID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.OK || !res.Unsupported {
		t.Fatalf("result = %+v, want unsupported", res)
	}
}
