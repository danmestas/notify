package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func openChannelStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSubscriberChannels_UpsertThenGet(t *testing.T) {
	ctx := context.Background()
	s := openChannelStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := s.SubscriberChannels().Upsert(ctx, scope, domain.SubscriberChannel{
		SubscriberID: "user_1",
		ProviderID:   "ntfy",
		Type:         "push",
		Credentials:  []byte(`{"topic":"agent-alerts"}`),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if created.ID == "" {
		t.Fatal("Upsert returned empty id")
	}
	if created.OrgID != "o1" || created.EnvID != "e1" {
		t.Fatalf("scope not stamped: %+v", created)
	}

	got, err := s.SubscriberChannels().GetByProvider(ctx, scope, "user_1", "ntfy")
	if err != nil {
		t.Fatalf("GetByProvider: %v", err)
	}
	if got.Type != "push" {
		t.Errorf("Type = %q, want %q", got.Type, "push")
	}
	if !bytes.Equal(got.Credentials, []byte(`{"topic":"agent-alerts"}`)) {
		t.Errorf("Credentials = %s, want the stored topic JSON", got.Credentials)
	}

	// Re-upsert replaces credentials in place (same dedup key -> one row, same id).
	if _, err := s.SubscriberChannels().Upsert(ctx, scope, domain.SubscriberChannel{
		SubscriberID: "user_1",
		ProviderID:   "ntfy",
		Type:         "push",
		Credentials:  []byte(`{"topic":"ops"}`),
	}); err != nil {
		t.Fatalf("re-Upsert: %v", err)
	}
	got, err = s.SubscriberChannels().GetByProvider(ctx, scope, "user_1", "ntfy")
	if err != nil {
		t.Fatalf("GetByProvider after re-upsert: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("re-upsert minted a new row: id %q -> %q", created.ID, got.ID)
	}
	if !bytes.Equal(got.Credentials, []byte(`{"topic":"ops"}`)) {
		t.Errorf("Credentials not updated: got %s", got.Credentials)
	}
}

func TestSubscriberChannels_NotFound(t *testing.T) {
	ctx := context.Background()
	s := openChannelStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := s.SubscriberChannels().GetByProvider(ctx, scope, "ghost", "ntfy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByProvider on missing row: err = %v, want ErrNotFound", err)
	}
}

func TestSubscriberChannels_CrossEnvIsolation(t *testing.T) {
	ctx := context.Background()
	s := openChannelStore(t)

	if _, err := s.SubscriberChannels().Upsert(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"}, domain.SubscriberChannel{
		SubscriberID: "user_1",
		ProviderID:   "ntfy",
		Type:         "push",
		Credentials:  []byte(`{"topic":"alpha"}`),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, err := s.SubscriberChannels().GetByProvider(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"}, "user_1", "ntfy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-env read: err = %v, want ErrNotFound", err)
	}
}
