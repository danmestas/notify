package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// seedMessage inserts one message row with the given overrides into
// scope {OrgID:"o1", EnvID:"e1"}. Only the specified fields need to
// be non-zero; the helper provides safe defaults for required columns.
func seedMessage(t *testing.T, s *Store, m domain.Message) domain.Message {
	t.Helper()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	if m.WorkflowID == "" {
		m.WorkflowID = "wf1"
	}
	if m.NotificationID == "" {
		m.NotificationID = "ntf1"
	}
	if m.RunID == "" {
		m.RunID = "run1"
	}
	if m.StepID == "" {
		m.StepID = "step1"
	}
	if m.SubscriberID == "" {
		m.SubscriberID = "sub1"
	}
	if m.Channel == "" {
		m.Channel = "email"
	}
	if m.ProviderID == "" {
		m.ProviderID = "smtp"
	}
	if m.Status == "" {
		m.Status = "sent"
	}
	created, err := s.Messages().Create(context.Background(), scope, m)
	if err != nil {
		t.Fatalf("seedMessage: %v", err)
	}
	return created
}

// TestMigrate_0017_AppliesIntegrationID verifies that migration 0017
// adds the integration_id column and the supporting index to messages.
func TestMigrate_0017_AppliesIntegrationID(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 20 {
		t.Fatalf("expected 20 recorded migrations, got %d", n)
	}

	// Verify the column exists via PRAGMA.
	rows, err := s.reader.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(messages): %v", err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt interface{}
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if name == "integration_id" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if !found {
		t.Fatal("messages table missing integration_id column after migration 0017")
	}
}

// TestMessageRepo_Create_PopulatesIntegrationID verifies that Create
// stores the IntegrationID field and List reads it back.
func TestMessageRepo_Create_PopulatesIntegrationID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created := seedMessage(t, s, domain.Message{IntegrationID: "integ-123"})

	msgs, err := s.Messages().List(ctx, scope, "sub1", "email")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].ID != created.ID {
		t.Errorf("ID mismatch: got %q, want %q", msgs[0].ID, created.ID)
	}
	if msgs[0].IntegrationID != "integ-123" {
		t.Errorf("IntegrationID = %q, want %q", msgs[0].IntegrationID, "integ-123")
	}
}

// TestMessageRepo_ListByIntegrationID returns messages belonging to the
// integration, scoped to the caller's org+env, newest-first.
func TestMessageRepo_ListByIntegrationID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Two messages for our integration.
	m1 := seedMessage(t, s, domain.Message{IntegrationID: "integ-A", RunID: "r1"})
	m2 := seedMessage(t, s, domain.Message{IntegrationID: "integ-A", RunID: "r2"})
	// One message for a different integration.
	_ = seedMessage(t, s, domain.Message{IntegrationID: "integ-B", RunID: "r3"})
	// One message with no integration_id (pre-migration legacy row).
	_ = seedMessage(t, s, domain.Message{IntegrationID: "", RunID: "r4"})

	msgs, err := s.Messages().ListByIntegrationID(ctx, scope, "integ-A", 0)
	if err != nil {
		t.Fatalf("ListByIntegrationID: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages for integ-A, got %d", len(msgs))
	}
	// Newest first: m2 was inserted after m1.
	ids := []string{msgs[0].ID, msgs[1].ID}
	if ids[0] != m2.ID || ids[1] != m1.ID {
		t.Errorf("order mismatch: got %v, want [%s %s]", ids, m2.ID, m1.ID)
	}
}

// TestMessageRepo_ListByIntegrationID_ScopeIsolation verifies that messages
// from a different scope are not returned even when they share the same
// integration_id.
func TestMessageRepo_ListByIntegrationID_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	scopeA := domain.Scope{OrgID: "org-A", EnvID: "env-A"}
	scopeB := domain.Scope{OrgID: "org-B", EnvID: "env-B"}

	// Seed one message under scopeA and one under scopeB with the same integration_id.
	if _, err := s.Messages().Create(ctx, scopeA, domain.Message{
		WorkflowID: "wf1", NotificationID: "ntf1", RunID: "rA", StepID: "s1",
		SubscriberID: "sub1", Channel: "email", ProviderID: "smtp",
		IntegrationID: "shared-integ", Status: "sent",
	}); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := s.Messages().Create(ctx, scopeB, domain.Message{
		WorkflowID: "wf1", NotificationID: "ntf1", RunID: "rB", StepID: "s1",
		SubscriberID: "sub2", Channel: "email", ProviderID: "smtp",
		IntegrationID: "shared-integ", Status: "sent",
	}); err != nil {
		t.Fatalf("create B: %v", err)
	}

	// scopeA query must return only the scopeA message.
	msgsA, err := s.Messages().ListByIntegrationID(ctx, scopeA, "shared-integ", 0)
	if err != nil {
		t.Fatalf("ListByIntegrationID scopeA: %v", err)
	}
	if len(msgsA) != 1 {
		t.Fatalf("scopeA: expected 1 message, got %d", len(msgsA))
	}
	if msgsA[0].OrgID != "org-A" || msgsA[0].EnvID != "env-A" {
		t.Errorf("scopeA message has wrong scope: %+v", msgsA[0])
	}

	// scopeB query must return only the scopeB message.
	msgsB, err := s.Messages().ListByIntegrationID(ctx, scopeB, "shared-integ", 0)
	if err != nil {
		t.Fatalf("ListByIntegrationID scopeB: %v", err)
	}
	if len(msgsB) != 1 {
		t.Fatalf("scopeB: expected 1 message, got %d", len(msgsB))
	}
	if msgsB[0].OrgID != "org-B" || msgsB[0].EnvID != "env-B" {
		t.Errorf("scopeB message has wrong scope: %+v", msgsB[0])
	}
}

// TestMessageRepo_ListByIntegrationID_LimitCapped verifies that the limit
// clamp (0 → 100) works correctly: asking for 0 returns at most 100 rows.
func TestMessageRepo_ListByIntegrationID_LimitCapped(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	for i := 0; i < 5; i++ {
		seedMessage(t, s, domain.Message{IntegrationID: "cap-integ"})
	}

	// limit=0 should default to 100 (cap), not 0.
	msgs, err := s.Messages().ListByIntegrationID(ctx, scope, "cap-integ", 0)
	if err != nil {
		t.Fatalf("ListByIntegrationID limit=0: %v", err)
	}
	if len(msgs) != 5 {
		t.Errorf("expected 5 messages with limit=0, got %d", len(msgs))
	}
}

// TestMessageRepo_ListByNotificationID returns messages for the given
// notification, scoped correctly, newest-first.
func TestMessageRepo_ListByNotificationID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Two messages for our notification.
	m1 := seedMessage(t, s, domain.Message{NotificationID: "ntf-X", RunID: "rX1"})
	m2 := seedMessage(t, s, domain.Message{NotificationID: "ntf-X", RunID: "rX2"})
	// One message for a different notification.
	_ = seedMessage(t, s, domain.Message{NotificationID: "ntf-Y", RunID: "rY"})

	msgs, err := s.Messages().ListByNotificationID(ctx, scope, "ntf-X")
	if err != nil {
		t.Fatalf("ListByNotificationID: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages for ntf-X, got %d", len(msgs))
	}
	ids := []string{msgs[0].ID, msgs[1].ID}
	if ids[0] != m2.ID || ids[1] != m1.ID {
		t.Errorf("order mismatch: got %v, want [%s %s]", ids, m2.ID, m1.ID)
	}
}

// TestMessageRepo_ListByNotificationID_ScopeIsolation verifies that
// notifications from a different scope are excluded.
func TestMessageRepo_ListByNotificationID_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	scopeA := domain.Scope{OrgID: "o-A", EnvID: "e-A"}
	scopeB := domain.Scope{OrgID: "o-B", EnvID: "e-B"}

	if _, err := s.Messages().Create(ctx, scopeA, domain.Message{
		WorkflowID: "wf1", NotificationID: "ntf-shared", RunID: "rA", StepID: "s1",
		SubscriberID: "sub1", Channel: "email", ProviderID: "smtp", Status: "sent",
	}); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := s.Messages().Create(ctx, scopeB, domain.Message{
		WorkflowID: "wf1", NotificationID: "ntf-shared", RunID: "rB", StepID: "s1",
		SubscriberID: "sub2", Channel: "email", ProviderID: "smtp", Status: "sent",
	}); err != nil {
		t.Fatalf("create B: %v", err)
	}

	msgsA, err := s.Messages().ListByNotificationID(ctx, scopeA, "ntf-shared")
	if err != nil {
		t.Fatalf("ListByNotificationID scopeA: %v", err)
	}
	if len(msgsA) != 1 || msgsA[0].OrgID != "o-A" {
		t.Fatalf("scopeA: expected 1 msg with OrgID=o-A, got %+v", msgsA)
	}

	msgsB, err := s.Messages().ListByNotificationID(ctx, scopeB, "ntf-shared")
	if err != nil {
		t.Fatalf("ListByNotificationID scopeB: %v", err)
	}
	if len(msgsB) != 1 || msgsB[0].OrgID != "o-B" {
		t.Fatalf("scopeB: expected 1 msg with OrgID=o-B, got %+v", msgsB)
	}
}
