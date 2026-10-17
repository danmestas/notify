package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// FS keeps objects as files under a root directory, fanned out by the first
// two bytes of the address (ab/cd/abcd…) so no directory grows unbounded. It
// is the default for a single node and the adapter tests run against.
type FS struct {
	root string
}

// NewFS returns a store rooted at dir, creating it if needed.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, errors.New("blobstore: empty directory")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("blobstore: create %s: %w", dir, err)
	}
	return &FS{root: dir}, nil
}

func (s *FS) path(addr string) string {
	return filepath.Join(s.root, addr[0:2], addr[2:4], addr)
}

// Put writes to a temporary file in the root, hashing as it goes, then
// renames it into place. A crash leaves at most a stray temporary file, never
// a partial object under a real address.
func (s *FS) Put(ctx context.Context, r io.Reader) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	tmp, err := os.CreateTemp(s.root, ".put-*")
	if err != nil {
		return Ref{}, fmt.Errorf("blobstore: temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once renamed

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Ref{}, fmt.Errorf("blobstore: write: %w", err)
	}
	ref := Ref{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}

	dst := s.path(ref.SHA256)
	if _, err := os.Stat(dst); err == nil {
		return ref, nil // already stored: content addressing makes this the same object
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return Ref{}, fmt.Errorf("blobstore: mkdir: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return Ref{}, fmt.Errorf("blobstore: store %s: %w", ref.SHA256, err)
	}
	return ref, nil
}

// Get opens the object at addr.
func (s *FS) Get(ctx context.Context, addr string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validAddr(addr) {
		return nil, fmt.Errorf("blobstore: invalid address %q", addr)
	}
	f, err := os.Open(s.path(addr))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("blobstore: open %s: %w", addr, err)
	}
	return f, nil
}

// Head reports the object's size.
func (s *FS) Head(ctx context.Context, addr string) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	if !validAddr(addr) {
		return Ref{}, fmt.Errorf("blobstore: invalid address %q", addr)
	}
	fi, err := os.Stat(s.path(addr))
	if errors.Is(err, fs.ErrNotExist) {
		return Ref{}, ErrNotFound
	}
	if err != nil {
		return Ref{}, fmt.Errorf("blobstore: stat %s: %w", addr, err)
	}
	return Ref{SHA256: addr, Size: fi.Size()}, nil
}
