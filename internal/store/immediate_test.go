package store_test

// Methodology: a real store on a temp file. WithImmediateTx pins the single
// writer connection, so a transaction left open after fn panics would hand
// that connection back mid-transaction and deadlock every later write.
// Positive: after a panicking fn, the panic still reaches the caller, the
// fn's write is rolled back, and a new transaction commits within a bounded
// time. Negative: a normal fn's write commits.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/store"
)

func TestWithImmediateTx_PanicRollsBackAndFreesWriter(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic inside fn did not reach the caller")
			}
		}()
		_ = st.WithImmediateTx(ctx, func(tx *store.Tx) error {
			if _, err := tx.Organizations().Create(ctx, "doomed"); err != nil {
				t.Errorf("create inside tx: %v", err)
			}
			panic("boom")
		})
	}()

	done := make(chan error, 1)
	go func() {
		done <- st.WithImmediateTx(ctx, func(tx *store.Tx) error {
			_, err := tx.Organizations().Create(ctx, "kept")
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("transaction after a panic: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("writer still held after a panic inside WithImmediateTx: deadlock")
	}

	doomed, err := st.Organizations().FindByName(ctx, "doomed")
	if err != nil || len(doomed) != 0 {
		t.Fatalf("panicked tx's write survived: %d orgs, err %v", len(doomed), err)
	}
	kept, err := st.Organizations().FindByName(ctx, "kept")
	if err != nil || len(kept) != 1 {
		t.Fatalf("committed write missing: %d orgs, err %v", len(kept), err)
	}
}
