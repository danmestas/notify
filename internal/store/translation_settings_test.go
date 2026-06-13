package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// mintEnvScope creates an org + environment and returns its Scope so the
// translation-settings UPDATE has a real row to touch.
func mintEnvScope(t *testing.T, s *Store) domain.Scope {
	t.Helper()
	ctx := context.Background()
	org, err := s.Organizations().Create(ctx, "Acme")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	_, scope, err := s.Environments().Create(ctx, org.ID, domain.Environment{
		Name:       "Prod",
		Identifier: "prod",
		Type:       "prod",
	})
	if err != nil {
		t.Fatalf("create env: %v", err)
	}
	return scope
}

func TestTranslationSettings_GetSet(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := mintEnvScope(t, s)
	repo := s.Environments()

	in := TranslationSettings{DefaultLocale: "en", TargetLocales: []string{"es", "fr"}}
	if err := repo.SetTranslationSettings(ctx, scope, in); err != nil {
		t.Fatalf("set settings: %v", err)
	}

	got, err := repo.GetTranslationSettings(ctx, scope)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if got.DefaultLocale != "en" {
		t.Errorf("default locale = %q, want en", got.DefaultLocale)
	}
	if len(got.TargetLocales) != 2 || got.TargetLocales[0] != "es" || got.TargetLocales[1] != "fr" {
		t.Errorf("target locales = %v, want [es fr]", got.TargetLocales)
	}
}

func TestTranslationSettings_DefaultValues(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := mintEnvScope(t, s)
	repo := s.Environments()

	got, err := repo.GetTranslationSettings(ctx, scope)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if got.DefaultLocale != "" {
		t.Errorf("default locale = %q, want empty", got.DefaultLocale)
	}
	if len(got.TargetLocales) != 0 {
		t.Errorf("target locales = %v, want empty", got.TargetLocales)
	}
}
