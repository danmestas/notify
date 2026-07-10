package envvars_test

import (
	"bytes"
	"context"
	"errors"
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

// TestSet_RejectsInvalidKey proves the same H2-class fix for env var keys: a
// key containing a character outside the URL-safe slug allowlist
// (^[A-Za-z0-9][A-Za-z0-9_-]*$) is rejected by Set before any store write.
// Previously such a key was persisted verbatim and used as a URL path
// segment by the console variable detail route, permanently orphaning it
// once it 404'd forever.
func TestSet_RejectsInvalidKey(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cases := []string{"foo/bar", "a b", "<script>", ""}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			_, err := svc.Set(ctx, scope, key, false, "val")
			if !errors.Is(err, envvars.ErrInvalidKey) {
				t.Fatalf("Set(%q) error = %v, want envvars.ErrInvalidKey", key, err)
			}
			if _, gErr := svc.Get(ctx, scope, key); gErr == nil {
				t.Fatalf("Set(%q): env var row was written despite invalid key; want no row", key)
			}
		})
	}
}

// TestSet_AcceptsValidKey proves the allowlist isn't overly strict:
// uppercase/lowercase alphanumerics plus hyphen/underscore, starting with a
// letter or digit, is accepted — matching the existing "KEY_A"-style
// convention used throughout this package's other tests.
func TestSet_AcceptsValidKey(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	cases := []string{"API_KEY", "my-key", "key_2", "a"}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			if _, err := svc.Set(ctx, scope, key, false, "val"); err != nil {
				t.Fatalf("Set(%q): %v", key, err)
			}
			if _, err := svc.Get(ctx, scope, key); err != nil {
				t.Fatalf("Get(%q) after Set: %v", key, err)
			}
		})
	}
}
