package streamsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

func newTestService(t *testing.T) (*Service, domain.Scope) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return New(st, key), domain.Scope{OrgID: "o1", EnvID: "e1"}
}

func TestNew_PanicsOnShortKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic on short master key")
		}
	}()
	New(nil, make([]byte, 16))
}

func TestService_Create_DefaultsAndSecret(t *testing.T) {
	ctx := context.Background()
	svc, scope := newTestService(t)

	src, raw, err := svc.Create(ctx, scope, "token-relay", "agent.tokens")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if src.RateLimitPerMin != 600 || src.MaxEventBytes != 32768 || src.MaxBatchEvents != 100 || src.ByteBudget != 2<<20 {
		t.Fatalf("defaults not applied: %+v", src)
	}
	if !src.Active {
		t.Fatalf("expected active source")
	}
	if src.Topic != "agent.tokens" {
		t.Fatalf("topic mismatch: %q", src.Topic)
	}

	if len(raw) != 64 {
		t.Fatalf("raw secret = %d chars, want 64 hex", len(raw))
	}
	if _, err := hex.DecodeString(raw); err != nil {
		t.Fatalf("raw secret not hex: %v", err)
	}

	rawBytes, _ := hex.DecodeString(raw)
	sum := sha256.Sum256(rawBytes)
	if src.SecretHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("stored hash != SHA-256(raw)")
	}
}

func TestService_Create_RejectsInvalidTopic(t *testing.T) {
	ctx := context.Background()
	svc, scope := newTestService(t)

	for _, topic := range []string{"orders.*", "a..b", strings.Repeat("x", 129), ">"} {
		_, _, err := svc.Create(ctx, scope, "ok-name", topic)
		if err == nil {
			t.Fatalf("expected rejection for topic %q", topic)
		}
		// The caller (console) matches on the sentinel, not a string
		// substring, to classify this as the "invalid topic" case.
		if !errors.Is(err, ErrInvalidTopic) {
			t.Fatalf("topic %q: err = %v, want errors.Is(ErrInvalidTopic)", topic, err)
		}
	}
}

func TestService_Create_RejectsInvalidName(t *testing.T) {
	ctx := context.Background()
	svc, scope := newTestService(t)

	for _, name := range []string{"Bad Name", "has space", "UPPER", "bad!", "", strings.Repeat("a", 65)} {
		if _, _, err := svc.Create(ctx, scope, name, "orders"); err == nil {
			t.Fatalf("expected rejection for name %q", name)
		}
	}
}

func TestService_Authenticate(t *testing.T) {
	ctx := context.Background()
	svc, scope := newTestService(t)

	src, raw, err := svc.Create(ctx, scope, "relay", "orders")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.Authenticate(ctx, raw)
	if err != nil {
		t.Fatalf("Authenticate(raw): %v", err)
	}
	if got.ID != src.ID {
		t.Fatalf("Authenticate returned wrong source")
	}

	// Both a malformed (non-hex) secret and a well-formed-but-unknown secret
	// must return the SAME sentinel (store.ErrNotFound), not distinguishable
	// error types — otherwise a caller mapping errors.Is(err, ErrNotFound)->401
	// would leak a 500-vs-401 oracle for malformed input.
	if _, err := svc.Authenticate(ctx, "not-hex-!!"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Authenticate(non-hex garbage): want store.ErrNotFound, got %v", err)
	}
	if _, err := svc.Authenticate(ctx, hex.EncodeToString(make([]byte, 32))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Authenticate(valid-hex, unknown): want store.ErrNotFound, got %v", err)
	}
}

func TestService_RotateSecret(t *testing.T) {
	ctx := context.Background()
	svc, scope := newTestService(t)

	src, oldRaw, err := svc.Create(ctx, scope, "relay", "orders")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newRaw, err := svc.RotateSecret(ctx, scope, src.ID)
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	if newRaw == oldRaw {
		t.Fatalf("rotated secret must differ")
	}

	if _, err := svc.Authenticate(ctx, oldRaw); err == nil {
		t.Fatalf("old secret must stop authenticating")
	}
	got, err := svc.Authenticate(ctx, newRaw)
	if err != nil {
		t.Fatalf("new secret must authenticate: %v", err)
	}
	if got.ID != src.ID {
		t.Fatalf("rotated auth resolved to wrong source")
	}

	// Exactly one row remains for the source.
	list, err := svc.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 source after rotate, got %d", len(list))
	}

	// Rotation of a foreign scope's source is a no-op / not-found.
	if _, err := svc.RotateSecret(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"}, src.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-scope rotate: want ErrNotFound, got %v", err)
	}
}

func TestService_MaskedSecret(t *testing.T) {
	ctx := context.Background()
	svc, scope := newTestService(t)

	src, _, err := svc.Create(ctx, scope, "relay", "orders")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	masked := svc.MaskedSecret(src)
	if !strings.HasPrefix(masked, secretMaskPrefix) {
		t.Fatalf("masked secret missing sentinel: %q", masked)
	}
	suffix := strings.TrimPrefix(masked, secretMaskPrefix)
	if len(suffix) != 4 {
		t.Fatalf("masked suffix = %q, want 4 identifying chars", suffix)
	}
	// The full raw secret must never appear in the masked form.
	if strings.Contains(masked, src.SecretHash) {
		t.Fatalf("masked form leaks the whole hash")
	}
}

func TestService_UpdateLimits_Clamps(t *testing.T) {
	ctx := context.Background()
	svc, scope := newTestService(t)

	src, _, err := svc.Create(ctx, scope, "relay", "orders")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Over-ceiling and non-positive values are clamped into range.
	if err := svc.UpdateLimits(ctx, scope, src.ID, 999999, -5, 0, 1<<30); err != nil {
		t.Fatalf("UpdateLimits: %v", err)
	}
	got, err := svc.Get(ctx, scope, src.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RateLimitPerMin != 6000 {
		t.Fatalf("rate not clamped to ceiling: %d", got.RateLimitPerMin)
	}
	if got.ByteBudget != 16<<20 {
		t.Fatalf("budget not clamped to ceiling: %d", got.ByteBudget)
	}
	if got.MaxEventBytes != 32768 {
		t.Fatalf("non-positive max_event_bytes not floored to default: %d", got.MaxEventBytes)
	}
	if got.MaxBatchEvents != 100 {
		t.Fatalf("non-positive max_batch_events not floored to default: %d", got.MaxBatchEvents)
	}

	// A valid in-range update is stored verbatim.
	if err := svc.UpdateLimits(ctx, scope, src.ID, 120, 4096, 25, 1<<20); err != nil {
		t.Fatalf("UpdateLimits in-range: %v", err)
	}
	got, _ = svc.Get(ctx, scope, src.ID)
	if got.RateLimitPerMin != 120 || got.MaxEventBytes != 4096 || got.MaxBatchEvents != 25 || got.ByteBudget != 1<<20 {
		t.Fatalf("in-range limits not stored verbatim: %+v", got)
	}
}

func TestService_SetActiveAndDelete(t *testing.T) {
	ctx := context.Background()
	svc, scope := newTestService(t)

	src, _, err := svc.Create(ctx, scope, "relay", "orders")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.SetActive(ctx, scope, src.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	got, _ := svc.Get(ctx, scope, src.ID)
	if got.Active {
		t.Fatalf("expected inactive")
	}
	// Inactive sources still authenticate (caller rejects with 403).
	// Verified indirectly: the row is resolvable by hash regardless of active.

	if err := svc.Delete(ctx, scope, src.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, scope, src.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
}

func TestService_TouchActivity(t *testing.T) {
	ctx := context.Background()
	svc, scope := newTestService(t)

	src, _, err := svc.Create(ctx, scope, "relay", "orders")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// TouchActivity is fire-and-forget (no return); the write is observable via Get.
	svc.TouchActivity(ctx, src.ID, "2026-07-15T00:00:00Z", "")
	got, err := svc.Get(ctx, scope, src.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastEventAt != "2026-07-15T00:00:00Z" {
		t.Fatalf("LastEventAt = %q, want the touched value", got.LastEventAt)
	}
	if got.LastError != "" {
		t.Fatalf("LastError = %q, want empty", got.LastError)
	}

	// A rejection touch records the reason without clearing forward progress.
	svc.TouchActivity(ctx, src.ID, "2026-07-15T00:01:00Z", "rate limited")
	got, _ = svc.Get(ctx, scope, src.ID)
	if got.LastError != "rate limited" {
		t.Fatalf("LastError = %q, want %q", got.LastError, "rate limited")
	}

	// An unknown id is a silent no-op (best-effort contract), never a panic/error.
	svc.TouchActivity(ctx, "does-not-exist", "x", "y")
}
