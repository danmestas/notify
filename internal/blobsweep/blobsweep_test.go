package blobsweep

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/blobstore"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

func TestSweep_DeletesUnreferencedBlobsAfterTheGracePeriod(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := blobstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := domain.Scope{OrgID: "o1", EnvID: "e1"}
	b := domain.Scope{OrgID: "o2", EnvID: "e2"}
	put := func(s string) string {
		ref, err := blobs.Put(ctx, bytes.NewReader([]byte(s)))
		if err != nil {
			t.Fatal(err)
		}
		return ref.SHA256
	}
	gone, reused, mail, other := put("gone"), put("reused later"), put("inbound mail"), put("kept by another scope")
	att := func(name, sha string) []domain.EmailAttachment {
		return []domain.EmailAttachment{{Filename: name, ContentType: "application/pdf", SHA256: sha, Size: 1}}
	}
	repo := st.EmailAttachments()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = st.Subscribers().Upsert(ctx, a, domain.Subscriber{SubscriberID: "sub-1"})
	must(err)
	for i, sha := range []string{gone, reused, mail, other} {
		must(repo.Add(ctx, a, "n"+string(rune('a'+i)), "sub-1", att("f.pdf", sha)))
	}
	must(repo.Add(ctx, b, "nx", "sub-b", att("f.pdf", other))) // another scope still uses it
	_, err = st.InboundMessages().Record(ctx, a, store.InboundMessage{SHA256: mail, Size: 1, Recipient: "r@x.io"})
	must(err)

	sub, err := st.Subscribers().GetByExternalID(ctx, a, "sub-1")
	must(err)
	must(st.Subscribers().SoftDelete(ctx, a, sub.ID))
	if n, err := st.Subscribers().PurgeDeletedByExternalID(ctx, a, "sub-1"); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	exists := func(sha string) bool { _, err := blobs.Head(ctx, sha); return err == nil }

	now := time.Now()
	// The purge queued the two blobs only it used; nothing is deleted yet.
	if n, err := Sweep(ctx, st, blobs, now.Add(time.Minute), Grace); err != nil || n != 0 {
		t.Fatalf("sweep inside the grace period = %d, %v", n, err)
	}
	for name, sha := range map[string]string{"gone": gone, "reused": reused} {
		if !exists(sha) {
			t.Fatalf("%s was deleted inside the grace period", name)
		}
	}

	// A new trigger attaches the same bytes inside the grace period.
	must(repo.Add(ctx, b, "ny", "sub-b", att("again.pdf", reused)))

	later := now.Add(2 * time.Hour)
	if n, err := Sweep(ctx, st, blobs, later, Grace); err != nil || n != 1 {
		t.Fatalf("sweep after the grace period = %d, %v; want 1 deleted (gone=%v reused=%v mail=%v other=%v)", n, err, exists(gone), exists(reused), exists(mail), exists(other))
	}
	for name, want := range map[string]bool{"gone": false, "reused": true, "mail": true, "other": true} {
		sha := map[string]string{"gone": gone, "reused": reused, "mail": mail, "other": other}[name]
		if exists(sha) != want {
			t.Errorf("blob %s exists = %v, want %v", name, exists(sha), want)
		}
	}
	if due, _ := repo.DueBlobDeletions(ctx, later.Add(24*time.Hour)); len(due) != 0 {
		t.Fatalf("queue not emptied: %v", due)
	}

	// A crash between the delete and the dequeue leaves a queued blob that is
	// already gone: the next sweep finishes the job without error.
	must(st.EmailAttachments().DropBlobDeletion(ctx, gone))
	w, _ := st.Pools()
	if _, err := w.ExecContext(ctx, `INSERT INTO blob_deletions (sha256, queued_at) VALUES (?, '2000-01-01T00:00:00.000000000Z')`, gone); err != nil {
		t.Fatal(err)
	}
	if n, err := Sweep(ctx, st, blobs, later, Grace); err != nil || n != 1 {
		t.Fatalf("resumed sweep = %d, %v", n, err)
	}
	if due, _ := repo.DueBlobDeletions(ctx, later.Add(24*time.Hour)); len(due) != 0 {
		t.Fatalf("queue not emptied after the resumed sweep: %v", due)
	}
}
