// Package blobstore keeps immutable byte objects addressed by their SHA-256:
// raw inbound mail and attachments, kept exactly as received. Content
// addressing makes Put idempotent and lets a reader check what it got.
//
// The interface is the only thing callers see. Adapters (a local directory
// today; an S3-API store such as Garage, or a JetStream object store, later)
// are chosen by configuration, so the storage choice never reaches the code
// that stores mail.
package blobstore

import (
	"context"
	"errors"
	"io"
	"regexp"
)

// ErrNotFound is returned by Get and Head for an address with no object.
var ErrNotFound = errors.New("blobstore: not found")

// Ref names a stored object: the hex SHA-256 of its bytes, and its size.
type Ref struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Store keeps objects by content address.
type Store interface {
	// Put stores everything read from r and returns its address. The store
	// computes the hash itself, so an object can never sit under the wrong
	// address. Storing bytes that are already present is a no-op.
	Put(ctx context.Context, r io.Reader) (Ref, error)
	// Get opens the object at sha256 for reading. The caller closes it.
	Get(ctx context.Context, sha256 string) (io.ReadCloser, error)
	// Head reports whether the object exists, and its size.
	Head(ctx context.Context, sha256 string) (Ref, error)
}

var addrPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validAddr reports whether s is a lower-case hex SHA-256, the only form an
// address takes. Checking it keeps a malformed address from reaching an
// adapter's path or key space.
func validAddr(s string) bool { return addrPattern.MatchString(s) }
