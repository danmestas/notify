package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestApiKeyRepo_ListAndDeleteByScope(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scope2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	// List on empty scope returns empty slice, not an error.
	keys, err := s.ApiKeys().List(ctx, scope)
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("List empty: got %d keys, want 0", len(keys))
	}

	// Create two keys in scope.
	k1, err := s.ApiKeys().Create(ctx, scope, domain.APIKey{
		Hash: "hash1", EncKey: []byte("fake-enc-1"),
	})
	if err != nil {
		t.Fatalf("Create k1: %v", err)
	}
	_, err = s.ApiKeys().Create(ctx, scope, domain.APIKey{
		Hash: "hash2", EncKey: []byte("fake-enc-2"),
	})
	if err != nil {
		t.Fatalf("Create k2: %v", err)
	}

	// Create one key in a different env (scope isolation check).
	_, err = s.ApiKeys().Create(ctx, scope2, domain.APIKey{
		Hash: "hash3", EncKey: []byte("fake-enc-3"),
	})
	if err != nil {
		t.Fatalf("Create k3 in scope2: %v", err)
	}

	// List for scope returns exactly 2.
	keys, err = s.ApiKeys().List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("List: got %d keys, want 2", len(keys))
	}
	if keys[0].Hash != k1.Hash && keys[1].Hash != k1.Hash {
		t.Errorf("List: key hash1 missing from result")
	}

	// DeleteByScope removes only scope's keys.
	if err := s.ApiKeys().DeleteByScope(ctx, scope); err != nil {
		t.Fatalf("DeleteByScope: %v", err)
	}

	keys, err = s.ApiKeys().List(ctx, scope)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("List after delete: got %d keys, want 0", len(keys))
	}

	// scope2's key survives the delete-by-scope on scope.
	keys2, err := s.ApiKeys().List(ctx, scope2)
	if err != nil {
		t.Fatalf("List scope2 after scope delete: %v", err)
	}
	if len(keys2) != 1 {
		t.Errorf("scope2 key count: got %d, want 1 (scope isolation broken)", len(keys2))
	}
}
