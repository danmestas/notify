package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// insertAttestation writes an audit row directly, so a test can place
// revoked_at at any age.
func insertAttestation(t *testing.T, s *Store, scope domain.Scope, id, subscriberID, revokedAt string) {
	t.Helper()
	reason := ""
	if revokedAt != "" {
		reason = RevokedByAPI
	}
	if _, err := s.writer.Exec(
		`INSERT INTO sms_consent_attestations
		   (id, org_id, environment_id, subscriber_id, phone, api_key_id, created_at, revoked_at, revoked_reason)
		 VALUES (?, ?, ?, ?, '+15551112222', 'k', '2020-01-01T00:00:00.000000000Z', ?, ?)`,
		id, scope.OrgID, scope.EnvID, subscriberID, revokedAt, reason,
	); err != nil {
		t.Fatalf("insert attestation: %v", err)
	}
}

// TestSMSConsent_HardPurgeClosesAndDetaches: a hard purge closes an open
// audit row as subscriber_purged, re-keys every row of that subscriber to one
// purged: tombstone, keeps the phone, and a new subscriber with the same id
// sees no history. Another environment's subscriber with that id is untouched.
func TestSMSConsent_HardPurgeClosesAndDetaches(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	a := domain.Scope{OrgID: "o1", EnvID: "e1"}
	b := domain.Scope{OrgID: "o1", EnvID: "e2"}
	subs, consents := s.Subscribers(), s.SMSConsents()
	var idA string
	for _, sc := range []domain.Scope{a, b} {
		sub, err := subs.Upsert(ctx, sc, domain.Subscriber{SubscriberID: "sub-1"})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if sc == a {
			idA = sub.ID
		}
		if _, err := consents.Attest(ctx, sc, SMSAttestation{SubscriberID: "sub-1", Phone: "+15551112222", APIKeyID: "k"}); err != nil {
			t.Fatalf("Attest: %v", err)
		}
	}
	// The soft delete closes the attestation; plant an open row to show the
	// purge closes strays too.
	if err := subs.SoftDelete(ctx, a, idA); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	insertAttestation(t, s, a, "stray-open", "sub-1", "")

	if n, err := subs.PurgeDeletedByExternalID(ctx, a, "sub-1"); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v; want 1", n, err)
	}
	if rows, _ := consents.List(ctx, a, "sub-1"); len(rows) != 0 {
		t.Fatalf("history after purge = %+v, want none", rows)
	}
	rows, err := s.reader.Query(
		`SELECT subscriber_id, phone, revoked_at, revoked_reason FROM sms_consent_attestations
		 WHERE environment_id = 'e1' ORDER BY id`,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	tombstones := map[string]bool{}
	var n int
	var sawPurged bool
	for rows.Next() {
		var sid, phone, at, reason string
		if err := rows.Scan(&sid, &phone, &at, &reason); err != nil {
			t.Fatal(err)
		}
		n++
		tombstones[sid] = true
		if !strings.HasPrefix(sid, "purged:") || len(sid) <= len("purged:") {
			t.Errorf("subscriber_id = %q, want a purged: tombstone", sid)
		}
		if phone != "+15551112222" || at == "" || reason == "" {
			t.Errorf("phone=%q revoked_at=%q reason=%q; want phone kept and row closed", phone, at, reason)
		}
		if reason == RevokedSubscriberPurged {
			sawPurged = true
		}
	}
	if n != 2 || len(tombstones) != 1 || !sawPurged {
		t.Fatalf("rows=%d tombstones=%v sawPurged=%v; want 2 rows, one tombstone, one subscriber_purged", n, tombstones, sawPurged)
	}

	// A new subscriber reusing the id starts clean.
	if _, err := subs.Upsert(ctx, a, domain.Subscriber{SubscriberID: "sub-1"}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if rows, _ := consents.List(ctx, a, "sub-1"); len(rows) != 0 {
		t.Fatalf("reused id inherited %+v", rows)
	}
	// The other environment's open attestation is untouched.
	got, _ := consents.List(ctx, b, "sub-1")
	if len(got) != 1 || got[0].RevokedAt != "" || got[0].SubscriberID != "sub-1" {
		t.Fatalf("env b history = %+v, want one open row", got)
	}
}

// TestSMSConsent_PurgeOfLiveSubscriberTouchesNothing: the purge deletes only a
// soft-deleted subscriber, so a live one keeps its history.
func TestSMSConsent_PurgeOfLiveSubscriberTouchesNothing(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SMSConsents().Attest(ctx, scope, SMSAttestation{SubscriberID: "sub-1", Phone: "+15551112222", APIKeyID: "k"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Subscribers().PurgeDeletedByExternalID(ctx, scope, "sub-1"); err != nil || n != 0 {
		t.Fatalf("purge = %d, %v; want 0", n, err)
	}
	if rows, _ := s.SMSConsents().List(ctx, scope, "sub-1"); len(rows) != 1 || rows[0].RevokedAt != "" {
		t.Fatalf("history = %+v, want one open row", rows)
	}
}

// TestSMSConsent_RetentionPurge: rows revoked more than the retention ago are
// deleted in every environment; younger rows and open rows are kept.
func TestSMSConsent_RetentionPurge(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	a := domain.Scope{OrgID: "o1", EnvID: "e1"}
	b := domain.Scope{OrgID: "o1", EnvID: "e2"}
	now := time.Now()
	old := timestamp.Format(now.AddDate(-SMSConsentRetentionYears, 0, -1))
	young := timestamp.Format(now.AddDate(-SMSConsentRetentionYears, 0, 1))
	insertAttestation(t, s, a, "old-a", "s1", old)
	insertAttestation(t, s, b, "old-b", "s1", old)
	insertAttestation(t, s, a, "young", "s1", young)
	insertAttestation(t, s, a, "open", "s2", "")

	n, err := s.SMSConsents().PurgeRetired(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("PurgeRetired = %d, %v; want 2", n, err)
	}
	rows, err := s.reader.Query(`SELECT id FROM sms_consent_attestations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var left []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if strings.Join(left, ",") != "open,young" {
		t.Fatalf("left = %v, want open and young", left)
	}
}
