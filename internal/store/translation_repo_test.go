package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0008_AppliesTranslations(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 37 {
		t.Fatalf("expected 35 recorded migrations, got %d", n)
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

func TestMigrate_0015_AppliesTranslationSettings(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 37 {
		t.Fatalf("expected 35 recorded migrations, got %d", n)
	}

	cols := map[string]bool{}
	rows, err := s.reader.Query(`PRAGMA table_info(environments)`)
	if err != nil {
		t.Fatalf("pragma table_info(environments): %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	if !cols["translation_default_locale"] {
		t.Errorf("environments missing translation_default_locale column")
	}
	if !cols["translation_target_locales"] {
		t.Errorf("environments missing translation_target_locales column")
	}
}

func TestTranslationRepo_ListLocales(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Translations()
	scope1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scope2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	for _, loc := range []string{"fr", "en", "es"} {
		if err := repo.Upsert(ctx, scope1, loc, "greeting", "x"); err != nil {
			t.Fatalf("upsert %s: %v", loc, err)
		}
	}

	got, err := repo.ListLocales(ctx, scope1)
	if err != nil {
		t.Fatalf("list locales: %v", err)
	}
	want := []string{"en", "es", "fr"}
	if len(got) != len(want) {
		t.Fatalf("locales = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("locales = %v, want %v (ordered ASC)", got, want)
		}
	}

	empty, err := repo.ListLocales(ctx, scope2)
	if err != nil {
		t.Fatalf("list locales scope2: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("scope2 must see 0 locales, got %v", empty)
	}
}

func TestTranslationRepo_Delete(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Translations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if err := repo.Upsert(ctx, scope, "en", "greeting", "Hi"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.Delete(ctx, scope, "en", "greeting"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	m, err := repo.Map(ctx, scope, "en")
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map after delete, got %v", m)
	}

	if err := repo.Delete(ctx, scope, "en", "greeting"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
}

func TestTranslationRepo_DeleteLocale(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Translations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	for _, k := range []string{"a", "b", "c"} {
		if err := repo.Upsert(ctx, scope, "en", k, "v"); err != nil {
			t.Fatalf("upsert %s: %v", k, err)
		}
	}
	if err := repo.DeleteLocale(ctx, scope, "en"); err != nil {
		t.Fatalf("delete locale: %v", err)
	}
	m, err := repo.Map(ctx, scope, "en")
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("expected locale cleared, got %v", m)
	}

	if err := repo.DeleteLocale(ctx, scope, "en"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete empty locale err = %v, want ErrNotFound", err)
	}
}

func TestTranslationRepo_BulkReplace(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Translations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if err := repo.Upsert(ctx, scope, "en", "old1", "A"); err != nil {
		t.Fatalf("upsert old1: %v", err)
	}
	if err := repo.Upsert(ctx, scope, "en", "old2", "B"); err != nil {
		t.Fatalf("upsert old2: %v", err)
	}

	if err := repo.BulkReplace(ctx, scope, "en", map[string]string{"new": "C"}); err != nil {
		t.Fatalf("bulk replace: %v", err)
	}
	m, err := repo.Map(ctx, scope, "en")
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(m) != 1 || m["new"] != "C" {
		t.Fatalf("after bulk replace map = %v, want {new:C}", m)
	}
}

func TestTranslationRepo_LocaleStats(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Translations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if err := repo.Upsert(ctx, scope, "en", "a", "1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.Upsert(ctx, scope, "en", "b", "2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.Upsert(ctx, scope, "es", "a", "1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	stats, err := repo.LocaleStats(ctx, scope)
	if err != nil {
		t.Fatalf("locale stats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("stats = %v, want 2 locales", stats)
	}
	if stats[0].Locale != "en" || stats[0].KeyCount != 2 {
		t.Errorf("stats[0] = %+v, want en/2", stats[0])
	}
	if stats[1].Locale != "es" || stats[1].KeyCount != 1 {
		t.Errorf("stats[1] = %+v, want es/1", stats[1])
	}
}
