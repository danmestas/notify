package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func sampleStreamSource(name, topic, hash string) domain.StreamSource {
	return domain.StreamSource{
		Name:            name,
		Topic:           topic,
		SecretHash:      hash,
		EncSecret:       []byte("enc-" + hash),
		RateLimitPerMin: 600,
		MaxEventBytes:   32768,
		MaxBatchEvents:  100,
		ByteBudget:      2 << 20,
		Active:          true,
		CreatedAt:       "2026-07-15T00:00:00Z",
	}
}

func TestMigrate_0030_AppliesStreamSources(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 40 {
		t.Fatalf("expected 35 recorded migrations, got %d", n)
	}

	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='stream_sources'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("stream_sources table missing: %v", err)
	}
}

func TestStreamSourceRepo_CRUDRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.StreamSources()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := repo.Create(ctx, scope, sampleStreamSource("src-a", "orders", "hash-a"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("expected minted ID")
	}
	if created.OrgID != "o1" || created.EnvID != "e1" {
		t.Fatalf("scope not stamped: %+v", created)
	}

	got, err := repo.GetByID(ctx, scope, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "src-a" || got.Topic != "orders" || got.SecretHash != "hash-a" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.RateLimitPerMin != 600 || got.MaxEventBytes != 32768 || got.MaxBatchEvents != 100 || got.ByteBudget != 2<<20 {
		t.Fatalf("limit columns not persisted: %+v", got)
	}
	if !got.Active {
		t.Fatalf("expected active")
	}

	list, err := repo.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List = %d rows, want 1", len(list))
	}
	if len(list[0].EncSecret) != 0 {
		t.Fatalf("List returned EncSecret — must be omitted for security")
	}
}

func TestStreamSourceRepo_GetByID_CrossScopeNotFound(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.StreamSources()
	e1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	row, err := repo.Create(ctx, e1, sampleStreamSource("src-a", "orders", "hash-a"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.GetByID(ctx, e2, row.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-scope GetByID: want ErrNotFound, got %v", err)
	}
	if _, err := repo.GetByID(ctx, e1, "nosuch"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing GetByID: want ErrNotFound, got %v", err)
	}
}

func TestStreamSourceRepo_UniqueSecretHash(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.StreamSources()
	e1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "o2", EnvID: "e2"}

	if _, err := repo.Create(ctx, e1, sampleStreamSource("src-a", "orders", "dup-hash")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Even under a different scope, the secret hash is globally unique.
	if _, err := repo.Create(ctx, e2, sampleStreamSource("src-b", "orders", "dup-hash")); err == nil {
		t.Fatalf("expected UNIQUE(secret_hash) violation, got nil")
	}
}

func TestStreamSourceRepo_GetBySecretHash(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.StreamSources()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := repo.Create(ctx, scope, sampleStreamSource("src-a", "orders", "hash-a"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetBySecretHash(ctx, "hash-a")
	if err != nil {
		t.Fatalf("GetBySecretHash hit: %v", err)
	}
	if got.ID != created.ID || got.OrgID != "o1" || got.EnvID != "e1" {
		t.Fatalf("GetBySecretHash returned wrong row: %+v", got)
	}

	if _, err := repo.GetBySecretHash(ctx, "no-such-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBySecretHash miss: want ErrNotFound, got %v", err)
	}
}

func TestStreamSourceRepo_TouchActivity(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.StreamSources()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := repo.Create(ctx, scope, sampleStreamSource("src-a", "orders", "hash-a"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := repo.TouchActivity(ctx, created.ID, "2026-07-15T01:02:03Z", "budget exceeded"); err != nil {
		t.Fatalf("TouchActivity: %v", err)
	}

	got, err := repo.GetByID(ctx, scope, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.LastEventAt != "2026-07-15T01:02:03Z" || got.LastError != "budget exceeded" {
		t.Fatalf("activity columns not updated: %+v", got)
	}
	// Only the two activity columns move; the rest is untouched.
	if got.Name != "src-a" || got.Topic != "orders" || got.RateLimitPerMin != 600 {
		t.Fatalf("TouchActivity mutated non-activity columns: %+v", got)
	}
}

func TestStreamSourceRepo_UpdateAndSetActiveAndDelete(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.StreamSources()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}

	created, err := repo.Create(ctx, scope, sampleStreamSource("src-a", "orders", "hash-a"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	upd := created
	upd.Name = "src-a2"
	upd.Topic = "orders.eu"
	upd.RateLimitPerMin = 42
	if err := repo.Update(ctx, scope, upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := repo.GetByID(ctx, scope, created.ID)
	if got.Name != "src-a2" || got.Topic != "orders.eu" || got.RateLimitPerMin != 42 {
		t.Fatalf("Update did not persist: %+v", got)
	}
	// secret_hash is immutable via Update.
	if got.SecretHash != "hash-a" {
		t.Fatalf("Update must not touch secret_hash: %+v", got)
	}

	if err := repo.Update(ctx, other, upd); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-scope Update: want ErrNotFound, got %v", err)
	}

	if err := repo.SetActive(ctx, scope, created.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	got, _ = repo.GetByID(ctx, scope, created.ID)
	if got.Active {
		t.Fatalf("expected inactive after SetActive(false)")
	}
	if err := repo.SetActive(ctx, other, created.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-scope SetActive: want ErrNotFound, got %v", err)
	}

	if err := repo.Delete(ctx, other, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-scope Delete: want ErrNotFound, got %v", err)
	}
	if err := repo.Delete(ctx, scope, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, scope, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID after delete: want ErrNotFound, got %v", err)
	}
}
