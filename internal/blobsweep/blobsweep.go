// Package blobsweep deletes blobs a subscriber purge left unreferenced.
//
// A purge cannot delete a blob inside its transaction, and a trigger may be
// about to attach the same bytes (the blob store is content-addressed, so equal
// files are one object). The purge therefore queues the blob, and Sweep
// deletes it after a grace period, once nothing points at it any more.
package blobsweep

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/blobstore"
	"github.com/danmestas/notify/internal/store"
)

// Grace is how long a queued blob waits before the sweeper may delete it.
const Grace = time.Hour

// Sweep handles every queued blob older than grace as of now. For each it
// re-checks the references in every scope (attachment rows and inbound mail):
// an unreferenced blob is deleted and then dequeued; a referenced one is only
// dequeued. Delete is idempotent and the dequeue comes last, so a crash between
// the steps is repaired by the next sweep. It returns how many blobs it deleted.
func Sweep(ctx context.Context, st *store.Store, blobs blobstore.Store, now time.Time, grace time.Duration) (int, error) {
	atts := st.EmailAttachments()
	due, err := atts.DueBlobDeletions(ctx, now.Add(-grace))
	if err != nil {
		return 0, err
	}
	deleted := 0
	var errs []error
	for _, sha := range due {
		referenced, err := atts.BlobReferenced(ctx, sha)
		if err == nil && !referenced {
			if err = blobs.Delete(ctx, sha); err == nil {
				deleted++
			}
		}
		if err == nil {
			err = atts.DropBlobDeletion(ctx, sha)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("blobsweep: %s: %w", sha, err))
		}
	}
	return deleted, errors.Join(errs...)
}
