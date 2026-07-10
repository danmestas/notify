package audience_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

func newSvc(t *testing.T) (*audience.Service, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return audience.New(s), ctx
}

// newSvcWithStore is newSvc but also hands back the underlying *store.Store so
// a test can seed a row directly through the store — bypassing every
// audience.Service validation — to set up legacy/pre-cap data.
func newSvcWithStore(t *testing.T) (*audience.Service, *store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return audience.New(s), s, ctx
}

func asValidationError(t *testing.T, err error) *audience.ValidationError {
	t.Helper()
	var verr *audience.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("err = %v (%T), want *audience.ValidationError", err, err)
	}
	return verr
}

func TestUpsertDedupesAndUpdates(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	first, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "ext-1",
		Email:        "a@example.com",
		FirstName:    "Ada",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if first.FirstName != "Ada" {
		t.Fatalf("first name = %q, want Ada", first.FirstName)
	}

	second, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "ext-1",
		Email:        "a@example.com",
		FirstName:    "Grace",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("upsert created new row: id %q != %q", second.ID, first.ID)
	}
	if second.FirstName != "Grace" {
		t.Fatalf("first name = %q, want Grace", second.FirstName)
	}

	got, err := svc.Get(ctx, scope, "ext-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FirstName != "Grace" {
		t.Fatalf("get first name = %q, want Grace", got.FirstName)
	}
}

func TestGetReturnsUpserted(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	if _, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "ext-2",
		Email:        "b@example.com",
		LastName:     "Lovelace",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := svc.Get(ctx, scope, "ext-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SubscriberID != "ext-2" || got.Email != "b@example.com" || got.LastName != "Lovelace" {
		t.Fatalf("get returned %+v", got)
	}
}

func TestScopeIsolation(t *testing.T) {
	svc, ctx := newSvc(t)
	e1 := domain.Scope{OrgID: "org1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "org1", EnvID: "e2"}

	if _, err := svc.Upsert(ctx, e1, audience.SubscriberInput{SubscriberID: "shared"}); err != nil {
		t.Fatalf("upsert e1: %v", err)
	}

	_, err := svc.Get(ctx, e2, "shared")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get under e2 = %v, want ErrNotFound", err)
	}
}

// TestNewlyCreatedSubscriberIsImmediatelyFindable is the Bug 5 investigation:
// a subscriber upserted via Upsert must be immediately returned by ListSubscribers
// and findable by name/email in the same process. There is no async indexing;
// the store uses SQLite WAL with a shared file and both reader and writer see
// the committed row synchronously.
func TestNewlyCreatedSubscriberIsImmediatelyFindable(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	sub, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "new-sub-findme",
		Email:        "findme@example.com",
		FirstName:    "Find",
		LastName:     "Me",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Must be in ListSubscribers immediately after create.
	page, err := svc.ListSubscribers(ctx, scope, "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.TotalCount != 1 {
		t.Errorf("TotalCount = %d, want 1", page.TotalCount)
	}
	if len(page.Items) != 1 {
		t.Fatalf("Items len = %d, want 1", len(page.Items))
	}
	if page.Items[0].ID != sub.ID {
		t.Errorf("item ID = %q, want %q", page.Items[0].ID, sub.ID)
	}

	// Must be findable by external ID immediately.
	got, err := svc.Get(ctx, scope, "new-sub-findme")
	if err != nil {
		t.Fatalf("Get by external id: %v", err)
	}
	if got.Email != "findme@example.com" {
		t.Errorf("email = %q, want findme@example.com", got.Email)
	}

	// Must be findable by internal ID immediately.
	gotByID, err := svc.GetSubscriberByID(ctx, scope, sub.ID)
	if err != nil {
		t.Fatalf("GetSubscriberByID: %v", err)
	}
	if gotByID.FirstName != "Find" {
		t.Errorf("first name = %q, want Find", gotByID.FirstName)
	}
}

// TestGetSubscriberByID_SoftDeletedReturnsNotFound guards Bug 2:
// GetSubscriberByID must return ErrNotFound for a soft-deleted subscriber so
// the console detail page 404s instead of rendering the deleted record.
func TestGetSubscriberByID_SoftDeletedReturnsNotFound(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	sub, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "del-ext-1",
		Email:        "delete-me@example.com",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := svc.DeleteSubscriber(ctx, scope, sub.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	_, err = svc.GetSubscriberByID(ctx, scope, sub.ID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSubscriberByID after delete = %v, want ErrNotFound", err)
	}
}

// TestCreateTopic_RejectsInvalidKey proves the same H2-class fix for topic
// keys: a key containing a character outside the URL-safe slug allowlist
// (^[a-z0-9][a-z0-9_-]*$) is rejected by CreateTopic before any store write.
// Previously such a key was persisted verbatim and used as a URL path
// segment by every console route (view/edit/delete), permanently orphaning
// the topic once it 404'd forever.
func TestCreateTopic_RejectsInvalidKey(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	cases := []string{"foo/bar", "a b", "Foo", "<script>", ""}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			_, err := svc.CreateTopic(ctx, scope, key, "Bad Topic")
			if !errors.Is(err, audience.ErrInvalidTopicKey) {
				t.Fatalf("CreateTopic(%q) error = %v, want audience.ErrInvalidTopicKey", key, err)
			}
			if _, gErr := svc.GetTopic(ctx, scope, key); !errors.Is(gErr, store.ErrNotFound) {
				t.Fatalf("CreateTopic(%q): topic row was written despite invalid key; want no row", key)
			}
		})
	}
}

// TestCreateTopic_AcceptsValidKey proves the allowlist isn't overly strict:
// lowercase alphanumerics plus hyphen/underscore, starting with a letter or
// digit, is accepted and reachable via GetTopic afterward.
func TestCreateTopic_AcceptsValidKey(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	cases := []string{"a", "weekly-digest", "topic_2", "digest123"}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			if _, err := svc.CreateTopic(ctx, scope, key, "Good Topic"); err != nil {
				t.Fatalf("CreateTopic(%q): %v", key, err)
			}
			if _, err := svc.GetTopic(ctx, scope, key); err != nil {
				t.Fatalf("GetTopic(%q) after create: %v", key, err)
			}
		})
	}
}

// ─── W5: email validation must live in the service layer, not just the
// console handler, so the /v1 API is covered too. ──────────────────────────

func TestUpsertRejectsMalformedEmail(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	_, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "bad-email-1",
		Email:        "not-an-email",
	})
	asValidationError(t, err)
}

func TestUpsertAllowsValidOrEmptyEmail(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	if _, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "good-email",
		Email:        "ok@example.com",
	}); err != nil {
		t.Fatalf("valid email rejected: %v", err)
	}
	if _, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "no-email",
	}); err != nil {
		t.Fatalf("empty email rejected: %v", err)
	}
}

func TestPatchSubscriberRejectsMalformedEmail(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	sub, err := svc.Upsert(ctx, scope, audience.SubscriberInput{SubscriberID: "patch-bad-email"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	_, err = svc.PatchSubscriber(ctx, scope, sub.ID, audience.SubscriberInput{Email: "still-not-an-email"})
	asValidationError(t, err)
}

func TestPatchSubscriberAllowsValidEmail(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	sub, err := svc.Upsert(ctx, scope, audience.SubscriberInput{SubscriberID: "patch-good-email"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := svc.PatchSubscriber(ctx, scope, sub.ID, audience.SubscriberInput{Email: "ok@example.com"})
	if err != nil {
		t.Fatalf("valid email rejected: %v", err)
	}
	if got.Email != "ok@example.com" {
		t.Fatalf("email = %q, want ok@example.com", got.Email)
	}
}

// ─── W3/W4: custom-field key/value length and empty-key validation. ───────

func TestUpsertRejectsOversizedCustomFieldValue(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	huge := make([]byte, 300)
	for i := range huge {
		huge[i] = 'x'
	}
	data, _ := json.Marshal(map[string]string{"bio": string(huge)})

	_, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "oversized-value",
		Data:         data,
	})
	asValidationError(t, err)
}

func TestUpsertRejectsEmptyCustomFieldKey(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	data, _ := json.Marshal(map[string]string{"": "val"})

	_, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "empty-key",
		Data:         data,
	})
	asValidationError(t, err)
}

func TestUpsertAllowsWellFormedCustomFields(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	data, _ := json.Marshal(map[string]string{"planTier": "gold"})

	got, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "wellformed",
		Data:         data,
	})
	if err != nil {
		t.Fatalf("well-formed custom fields rejected: %v", err)
	}
	var fields map[string]string
	if err := json.Unmarshal(got.Data, &fields); err != nil {
		t.Fatalf("unmarshal stored data: %v", err)
	}
	if fields["planTier"] != "gold" {
		t.Fatalf("planTier = %q, want gold", fields["planTier"])
	}
}

// ─── W2: server-side cap on custom-field count. ────────────────────────────

func TestUpsertRejectsMoreThanMaxCustomFields(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	fields := map[string]string{}
	for i := 0; i < audience.MaxSubscriberCustomFields+1; i++ {
		fields["k"+string(rune('a'+i))] = "v"
	}
	data, _ := json.Marshal(fields)

	_, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "too-many-fields",
		Data:         data,
	})
	asValidationError(t, err)
}

func TestUpsertAllowsExactlyMaxCustomFields(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	fields := map[string]string{}
	for i := 0; i < audience.MaxSubscriberCustomFields; i++ {
		fields["k"+string(rune('a'+i))] = "v"
	}
	data, _ := json.Marshal(fields)

	if _, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "exactly-max-fields",
		Data:         data,
	}); err != nil {
		t.Fatalf("at-cap custom fields rejected: %v", err)
	}
}

func TestPatchSubscriberRejectsGrowingPastCap(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	sub, err := svc.Upsert(ctx, scope, audience.SubscriberInput{SubscriberID: "grow-past-cap"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	fields := map[string]string{}
	for i := 0; i < audience.MaxSubscriberCustomFields+1; i++ {
		fields["k"+string(rune('a'+i))] = "v"
	}
	data, _ := json.Marshal(fields)

	_, err = svc.PatchSubscriber(ctx, scope, sub.ID, audience.SubscriberInput{Data: data})
	asValidationError(t, err)
}

// TestPatchSubscriberPreservesLegacyOversizedCustomFieldsOnUnchangedSave is
// the W1 make-or-break test: a subscriber that already carries more than
// MaxSubscriberCustomFields (seeded directly through the store, bypassing
// every audience.Service validation — this is how such a row could exist:
// written before the cap existed, or via a direct store edit) must NOT lose
// data when the console's edit form is re-submitted with the SAME data
// unchanged. The cap only blocks growth past what's already stored; it must
// never be the reason a legacy row gets truncated.
func TestPatchSubscriberPreservesLegacyOversizedCustomFieldsOnUnchangedSave(t *testing.T) {
	svc, s, ctx := newSvcWithStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	fields := map[string]string{}
	for i := 0; i < 12; i++ {
		fields["field"+string(rune('a'+i))] = "value" + string(rune('a'+i))
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal seed data: %v", err)
	}

	seeded, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{
		SubscriberID: "legacy-12-fields",
		Data:         data,
	})
	if err != nil {
		t.Fatalf("seed via store: %v", err)
	}

	var before map[string]string
	if err := json.Unmarshal(seeded.Data, &before); err != nil {
		t.Fatalf("unmarshal seeded data: %v", err)
	}
	if len(before) != 12 {
		t.Fatalf("seeded field count = %d, want 12", len(before))
	}

	// Simulate the edit form's "Save unchanged": re-submit the exact same 12
	// fields via PatchSubscriber, the code path the console's edit handler
	// calls.
	updated, err := svc.PatchSubscriber(ctx, scope, seeded.ID, audience.SubscriberInput{Data: data})
	if err != nil {
		t.Fatalf("unchanged save of legacy over-cap subscriber was rejected: %v", err)
	}

	var after map[string]string
	if err := json.Unmarshal(updated.Data, &after); err != nil {
		t.Fatalf("unmarshal updated data: %v", err)
	}
	if len(after) != 12 {
		t.Fatalf("field count after unchanged save = %d, want 12 (none dropped)", len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("field %q = %q after save, want %q (preserved)", k, after[k], v)
		}
	}

	// Re-fetch from the store independently to make sure the save actually
	// persisted 12 fields, not just what PatchSubscriber returned in memory.
	refetched, err := svc.GetSubscriberByID(ctx, scope, seeded.ID)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	var persisted map[string]string
	if err := json.Unmarshal(refetched.Data, &persisted); err != nil {
		t.Fatalf("unmarshal persisted data: %v", err)
	}
	if len(persisted) != 12 {
		t.Fatalf("persisted field count = %d, want 12", len(persisted))
	}
}
