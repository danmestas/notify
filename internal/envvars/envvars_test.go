package envvars_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/envvars"
	"github.com/danmestas/notify/internal/store"

	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestService(t *testing.T) (*envvars.Service, *store.Store) {
	t.Helper()
	s := openTestStore(t)
	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	svc, err := envvars.New(s, masterKey)
	if err != nil {
		t.Fatalf("envvars.New: %v", err)
	}
	return svc, s
}

func TestEnvVars_EncryptDecryptRoundTrip(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	want := "super-secret-value"
	if _, err := svc.Set(ctx, scope, "MY_KEY", true, want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := svc.GetValue(ctx, scope, "MY_KEY")
	if err != nil {
		t.Fatalf("GetValue: %v", err)
	}
	if got != want {
		t.Errorf("GetValue = %q, want %q", got, want)
	}
}

func TestEnvVars_ListMaskedByDefault(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	for _, key := range []string{"KEY_A", "KEY_B", "KEY_C"} {
		if _, err := svc.Set(ctx, scope, key, true, "secret"+key); err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
	}

	items, _, _, err := svc.List(ctx, scope, "", "", "", 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("List len = %d, want 3", len(items))
	}
	// domain.EnvVar has no Value field — values are not exposed through List.
	for _, item := range items {
		if item.Key == "" {
			t.Errorf("item has empty key: %+v", item)
		}
	}
}

func TestEnvVars_Delete(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.Set(ctx, scope, "TO_DELETE", true, "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := svc.Delete(ctx, scope, "TO_DELETE"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := svc.Get(ctx, scope, "TO_DELETE"); err == nil {
		t.Fatal("Get after delete: want error, got nil")
	}
}

func TestEnvVars_UpsertReplaces(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.Set(ctx, scope, "REPLACED", true, "original"); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if _, err := svc.Set(ctx, scope, "REPLACED", false, "updated"); err != nil {
		t.Fatalf("second Set: %v", err)
	}

	got, err := svc.GetValue(ctx, scope, "REPLACED")
	if err != nil {
		t.Fatalf("GetValue: %v", err)
	}
	if got != "updated" {
		t.Errorf("GetValue = %q, want %q", got, "updated")
	}

	meta, err := svc.Get(ctx, scope, "REPLACED")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if meta.IsSecret {
		t.Error("IsSecret should be false after second Set")
	}
}

func TestEnvVars_ScopeIsolation(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	s1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	s2 := domain.Scope{OrgID: "o2", EnvID: "e2"}

	if _, err := svc.Set(ctx, s1, "ISOLATED", true, "s1-value"); err != nil {
		t.Fatalf("Set in s1: %v", err)
	}

	if _, err := svc.Get(ctx, s2, "ISOLATED"); err == nil {
		t.Fatal("Get in s2: want error (not found), got nil")
	}
}
