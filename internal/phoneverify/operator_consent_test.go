package phoneverify_test

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/phoneverify"
)

func TestOperatorConsent_EnableDisable(t *testing.T) {
	ctx := context.Background()
	st, svc, scope := consentFixture(t)
	if _, err := st.ApiKeys().Create(ctx, scope, domain.APIKey{Hash: "h1", EncKey: []byte("x")}); err != nil {
		t.Fatalf("key: %v", err)
	}
	if err := svc.EnableOperatorConsent(ctx, scope, "http://shop.example/p"); !errors.Is(err, phoneverify.ErrPolicyURLRequired) {
		t.Fatalf("http url: %v, want ErrPolicyURLRequired", err)
	}
	if s, _ := svc.ConsentSettings(ctx, scope); s.Enabled {
		t.Fatal("refused enable changed the setting")
	}
	if err := svc.EnableOperatorConsent(ctx, scope, "https://shop.example/p"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if keys, _ := st.ApiKeys().List(ctx, scope); !keys[0].AttestSMSConsent {
		t.Fatal("key not permitted after enable")
	}
	if err := svc.DisableOperatorConsent(ctx, scope); err != nil {
		t.Fatalf("disable: %v", err)
	}
	s, _ := svc.ConsentSettings(ctx, scope)
	if s.Enabled || s.PolicyURL != "https://shop.example/p" {
		t.Fatalf("after disable = %+v", s)
	}
	if keys, _ := st.ApiKeys().List(ctx, scope); keys[0].AttestSMSConsent {
		t.Fatal("key still permitted after disable")
	}
}
