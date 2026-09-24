package connectivity

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	webpushprovider "github.com/danmestas/notify/internal/providers/webpush"
)

func TestValidateVAPIDSubject(t *testing.T) {
	good := []string{"mailto:ops@example.com", "https://notify.example.com", "https://notify.example.com/path", "mailto:Ops <ops@example.co.uk>"}
	for _, s := range good {
		if err := ValidateVAPIDSubject(s); err != nil {
			t.Errorf("ValidateVAPIDSubject(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{
		"", "mailto:notify@localhost", "https://localhost", "https://localhost:3030",
		"https://127.0.0.1", "https://[::1]", "mailto:ops@10.0.0.1", "http://example.com",
		"ops@example.com", "https://intranet", "mailto:x@box.local", "https://app.localhost",
	}
	for _, s := range bad {
		if err := ValidateVAPIDSubject(s); !errors.Is(err, ErrInvalidVAPIDSubject) {
			t.Errorf("ValidateVAPIDSubject(%q) = %v, want ErrInvalidVAPIDSubject", s, err)
		}
	}
}

// resolvedSubject builds the scope's webpush client and returns the subject
// it will sign with.
func resolvedSubject(t *testing.T, svc *Service, scope domain.Scope) (string, error) {
	t.Helper()
	in, err := svc.store.Integrations().GetByProviderID(context.Background(), scope, webPushProviderID)
	if err != nil {
		t.Fatalf("webpush integration: %v", err)
	}
	p, err := svc.PushProviderFromIntegration(context.Background(), in)
	if err != nil {
		return "", err
	}
	return p.(*webpushprovider.Client).Subject, nil
}

// TestWebPushSubject_DefaultOverrideAndClear is #358: the client signs with
// the environment's subject when one is set, the public-URL default
// otherwise, and never with a localhost contact.
func TestWebPushSubject_DefaultOverrideAndClear(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey(), WithPublicBaseURL("https://notify.example.com/"))
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	if _, _, err := svc.EnsureWebPushKeys(ctx, scope); err != nil {
		t.Fatalf("EnsureWebPushKeys: %v", err)
	}

	if got, err := resolvedSubject(t, svc, scope); err != nil || got != "https://notify.example.com" {
		t.Fatalf("default subject = %q, %v; want https://notify.example.com", got, err)
	}
	if sub, isDefault, _ := svc.WebPushSubject(ctx, scope); sub != "https://notify.example.com" || !isDefault {
		t.Fatalf("WebPushSubject = %q default=%v", sub, isDefault)
	}

	if err := svc.SetWebPushSubject(ctx, scope, "mailto:ops@example.org"); err != nil {
		t.Fatalf("SetWebPushSubject: %v", err)
	}
	if got, _ := resolvedSubject(t, svc, scope); got != "mailto:ops@example.org" {
		t.Fatalf("override subject = %q", got)
	}
	// The keypair survives the rewrite of the sealed config.
	pub1, _, _ := svc.EnsureWebPushKeys(ctx, scope)
	if pub1 == "" {
		t.Fatal("public key lost after setting the subject")
	}

	if err := svc.SetWebPushSubject(ctx, scope, "mailto:notify@localhost"); !errors.Is(err, ErrInvalidVAPIDSubject) {
		t.Fatalf("localhost subject accepted: %v", err)
	}
	if got, _ := resolvedSubject(t, svc, scope); got != "mailto:ops@example.org" {
		t.Fatalf("a refused subject changed the stored one: %q", got)
	}

	if err := svc.SetWebPushSubject(ctx, scope, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := resolvedSubject(t, svc, scope); got != "https://notify.example.com" {
		t.Fatalf("after clear subject = %q, want the default", got)
	}
}

// TestWebPushSubject_NoneIsLoud: with no public URL and no override, the
// provider cannot be built, so a push fails with a message naming the fix
// instead of being signed with a contact Apple rejects.
func TestWebPushSubject_NoneIsLoud(t *testing.T) {
	ctx := context.Background()
	for _, base := range []string{"", "http://localhost:3030", "http://127.0.0.1:3030"} {
		svc := New(openTemp(t), testKey(), WithPublicBaseURL(base))
		scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
		if _, _, err := svc.EnsureWebPushKeys(ctx, scope); err != nil {
			t.Fatalf("EnsureWebPushKeys: %v", err)
		}
		if _, err := resolvedSubject(t, svc, scope); !errors.Is(err, ErrNoVAPIDSubject) {
			t.Fatalf("base %q: err = %v, want ErrNoVAPIDSubject", base, err)
		}
	}
}
