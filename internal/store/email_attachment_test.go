package store

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

func attach(name, sha string) domain.EmailAttachment {
	return domain.EmailAttachment{Filename: name, ContentType: "application/pdf", SHA256: sha, Size: 12}
}

func TestEmailAttachments_AddListAndRetry(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	sc := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.EmailAttachments()
	atts := []domain.EmailAttachment{attach("a.pdf", "aa"), attach("b.pdf", "bb")}
	for i := 0; i < 2; i++ { // the second call is a retried trigger: no duplicate rows
		if err := repo.Add(ctx, sc, "n1", "sub-1", atts); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.ListByNotification(ctx, sc, "n1")
	if err != nil || len(got) != 2 || got[0].Filename != "a.pdf" || got[1].Filename != "b.pdf" || got[1].Position != 1 {
		t.Fatalf("list = %+v, %v", got, err)
	}
	if other, _ := repo.ListByNotification(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"}, "n1"); len(other) != 0 {
		t.Fatalf("another environment sees %+v", other)
	}
	m, err := repo.ListByNotifications(ctx, sc, []string{"n1", "n2"})
	if err != nil || len(m["n1"]) != 2 || len(m) != 1 {
		t.Fatalf("by notifications = %+v, %v", m, err)
	}
}

// TestEmailAttachments_PurgeDeletesRowsAndKeepsSharedBlobs: a hard purge drops
// the subscriber's rows only; BlobReferenced still sees a blob another
// subscriber's row, or an inbound mail, points at.
func TestEmailAttachments_PurgeDeletesRowsAndKeepsSharedBlobs(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	sc := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.EmailAttachments()
	for _, id := range []string{"sub-1", "sub-2"} {
		if _, err := s.Subscribers().Upsert(ctx, sc, domain.Subscriber{SubscriberID: id}); err != nil {
			t.Fatal(err)
		}
	}
	// sub-1 and sub-2 share blob "shared"; sub-1 alone has "own".
	if err := repo.Add(ctx, sc, "n1", "sub-1", []domain.EmailAttachment{attach("a.pdf", "shared"), attach("b.pdf", "own"), attach("c.pdf", "elsewhere")}); err != nil {
		t.Fatal(err)
	}
	// Another environment's row points at "elsewhere": references count in
	// every scope, because the blob store is shared.
	if err := repo.Add(ctx, domain.Scope{OrgID: "o2", EnvID: "e9"}, "n3", "sub-9", []domain.EmailAttachment{attach("c.pdf", "elsewhere")}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Add(ctx, sc, "n2", "sub-2", []domain.EmailAttachment{attach("a.pdf", "shared")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writer.Exec(
		`INSERT INTO inbound_messages (id, org_id, environment_id, sha256, size, recipient, received_at)
		 VALUES ('i1', 'o1', 'e1', 'mail', 5, 'r', '2020-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	shas, err := repo.SHA256sBySubscriber(ctx, sc, "sub-1")
	if err != nil || len(shas) != 3 {
		t.Fatalf("shas = %v, %v", shas, err)
	}

	sub, err := s.Subscribers().GetByExternalID(ctx, sc, "sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Subscribers().SoftDelete(ctx, sc, sub.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Subscribers().PurgeDeletedByExternalID(ctx, sc, "sub-1"); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if got, _ := repo.ListByNotification(ctx, sc, "n1"); len(got) != 0 {
		t.Fatalf("purged subscriber still has %+v", got)
	}
	if got, _ := repo.ListByNotification(ctx, sc, "n2"); len(got) != 1 {
		t.Fatalf("the other subscriber lost its row: %+v", got)
	}
	// Only the blob nothing else points at is queued, and not yet due.
	if due, err := repo.DueBlobDeletions(ctx, time.Now().Add(-time.Hour)); err != nil || len(due) != 0 {
		t.Fatalf("due inside the grace period = %v, %v", due, err)
	}
	if due, err := repo.DueBlobDeletions(ctx, time.Now().Add(time.Hour)); err != nil || len(due) != 1 || due[0] != "own" {
		t.Fatalf("queued = %v, %v; want [own]", due, err)
	}
	for sha, want := range map[string]bool{"own": false, "shared": true, "mail": true, "elsewhere": true, "nothing": false} {
		if got, err := repo.BlobReferenced(ctx, sha); err != nil || got != want {
			t.Errorf("BlobReferenced(%q) = %v, %v; want %v", sha, got, err, want)
		}
	}
}
