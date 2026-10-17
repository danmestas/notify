package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// conformance is the behaviour every adapter must have. Each adapter's test
// calls it with a constructor for a fresh, empty store.
func conformance(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()

	t.Run("bytes come back exactly", func(t *testing.T) {
		s := newStore(t)
		// Raw mail as received: CRLF line ends, a header, 8-bit body bytes.
		raw := []byte("Message-ID: <a@b>\r\nSubject: Caf\xc3\xa9\r\n\r\nbody\x00\xff\r\n.\r\n")
		ref, err := s.Put(ctx, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if ref.SHA256 != hex.EncodeToString(sum[:]) || ref.Size != int64(len(raw)) {
			t.Fatalf("ref = %+v", ref)
		}
		rc, err := s.Get(ctx, ref.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(got, raw) {
			t.Fatalf("got %q, want %q", got, raw)
		}
		head, err := s.Head(ctx, ref.SHA256)
		if err != nil || head != ref {
			t.Fatalf("head = %+v, %v; want %+v", head, err, ref)
		}
	})

	t.Run("empty object", func(t *testing.T) {
		s := newStore(t)
		ref, err := s.Put(ctx, strings.NewReader(""))
		if err != nil || ref.Size != 0 {
			t.Fatalf("ref = %+v, %v", ref, err)
		}
		if _, err := s.Head(ctx, ref.SHA256); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("put is idempotent, also concurrently", func(t *testing.T) {
		s := newStore(t)
		var wg sync.WaitGroup
		refs := make([]Ref, 8)
		errs := make([]error, 8)
		for i := range refs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				refs[i], errs[i] = s.Put(ctx, strings.NewReader("same bytes"))
			}(i)
		}
		wg.Wait()
		for i := range refs {
			if errs[i] != nil || refs[i] != refs[0] {
				t.Fatalf("put %d = %+v, %v; want %+v", i, refs[i], errs[i], refs[0])
			}
		}
	})

	t.Run("missing object", func(t *testing.T) {
		s := newStore(t)
		missing := strings.Repeat("ab", 32)
		if _, err := s.Get(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Fatalf("get: %v, want ErrNotFound", err)
		}
		if _, err := s.Head(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Fatalf("head: %v, want ErrNotFound", err)
		}
	})

	t.Run("malformed address is refused", func(t *testing.T) {
		s := newStore(t)
		for _, addr := range []string{"", "../../etc/passwd", strings.Repeat("AB", 32), strings.Repeat("ab", 31)} {
			if _, err := s.Get(ctx, addr); err == nil || errors.Is(err, ErrNotFound) {
				t.Errorf("get %q: %v, want an invalid-address error", addr, err)
			}
			if _, err := s.Head(ctx, addr); err == nil || errors.Is(err, ErrNotFound) {
				t.Errorf("head %q: %v, want an invalid-address error", addr, err)
			}
		}
	})
}

func TestFS_Conformance(t *testing.T) {
	conformance(t, func(t *testing.T) Store {
		s, err := NewFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// TestFS_LeavesNoTemporaryFiles: after a put, and after a put of bytes
// already stored, only the object itself remains under the root.
func TestFS_LeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Put(context.Background(), strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 1 {
		t.Fatalf("files under root = %v, want one object", files)
	}
}
