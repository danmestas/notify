package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0008_AppliesTranslations(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 13 {
		t.Fatalf("expected 13 recorded migrations, got %d", n)
	}

	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "translations",
	).Scan(&name)
	if err != nil {
		t.Fatalf("translations table missing: %v", err)
	}
}

func TestTranslationRepo_UpsertMap(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Translations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if err := repo.Upsert(ctx, scope, "en", "greeting", "Hello"); err != nil {
		t.Fatalf("upsert en greeting: %v", err)
	}
	if err := repo.Upsert(ctx, scope, "es", "greeting", "Hola"); err != nil {
		t.Fatalf("upsert es greeting: %v", err)
	}
	if err := repo.Upsert(ctx, scope, "en", "bye", "Bye"); err != nil {
		t.Fatalf("upsert en bye: %v", err)
	}

	en, err := repo.Map(ctx, scope, "en")
	if err != nil {
		t.Fatalf("map en: %v", err)
	}
	if len(en) != 2 || en["greeting"] != "Hello" || en["bye"] != "Bye" {
		t.Fatalf("unexpected en map: %v", en)
	}

	es, err := repo.Map(ctx, scope, "es")
	if err != nil {
		t.Fatalf("map es: %v", err)
	}
	if len(es) != 1 || es["greeting"] != "Hola" {
		t.Fatalf("unexpected es map: %v", es)
	}

	// Upsert on the same (env, locale, key) updates value in place.
	if err := repo.Upsert(ctx, scope, "es", "greeting", "Buenas"); err != nil {
		t.Fatalf("re-upsert es greeting: %v", err)
	}
	es, err = repo.Map(ctx, scope, "es")
	if err != nil {
		t.Fatalf("map es after update: %v", err)
	}
	if len(es) != 1 || es["greeting"] != "Buenas" {
		t.Fatalf("expected updated es map, got %v", es)
	}

	// Cross-env isolation: e1 translations invisible under e2.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	got, err := repo.Map(ctx, other, "en")
	if err != nil {
		t.Fatalf("map e2: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("e2 must not see e1 translations, got %v", got)
	}
}
