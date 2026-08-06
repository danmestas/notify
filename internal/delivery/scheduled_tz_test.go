package delivery

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// subscriberLocation is the seam where a stored subscriber timezone becomes the
// zone a scheduled delay fires in. Every failure path must yield UTC rather than
// an error: this decides WHEN a send fires, and a lookup problem must not turn a
// scheduled delivery into a dropped one.

func tzStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedRunForSubscriber creates a subscriber with the given timezone and a
// notification pointing at it, returning the run id.
func seedRunForSubscriber(t *testing.T, st *store.Store, timezone string) string {
	t.Helper()
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := st.Subscribers().Upsert(ctx, scope, domain.Subscriber{
		SubscriberID: "user_tz", FirstName: "Ada", Timezone: timezone,
	}); err != nil {
		t.Fatalf("Subscribers.Upsert: %v", err)
	}
	if _, err := st.Notifications().Create(ctx, scope, domain.Notification{
		ID: "ntf_tz", WorkflowID: "wf_tz", SubscriberID: "user_tz",
		RunID: "run_tz", Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Notifications.Create: %v", err)
	}
	return "run_tz"
}

// TestSubscriberLocation_UsesStoredTimezone proves the stored value actually
// reaches the scheduler -- the whole point of the change.
func TestSubscriberLocation_UsesStoredTimezone(t *testing.T) {
	st := tzStore(t)
	runID := seedRunForSubscriber(t, st, "Asia/Tokyo")

	got := NewDelayWait(st).subscriberLocation(context.Background(), runID)
	if got == time.UTC {
		t.Fatal("a subscriber with Asia/Tokyo resolved to UTC: the stored timezone is not reaching the scheduler")
	}
	if got.String() != "Asia/Tokyo" {
		t.Errorf("location = %q, want Asia/Tokyo", got)
	}
}

// TestSubscriberLocation_EmptyTimezoneIsUTC is the backward-compatibility case
// and covers most existing subscribers: the column has defaulted to empty since
// the first migration, so their scheduling must be unchanged.
func TestSubscriberLocation_EmptyTimezoneIsUTC(t *testing.T) {
	st := tzStore(t)
	runID := seedRunForSubscriber(t, st, "")

	if got := NewDelayWait(st).subscriberLocation(context.Background(), runID); got != time.UTC {
		t.Errorf("empty timezone = %v, want UTC", got)
	}
}

// TestSubscriberLocation_BadTimezoneIsUTC keeps a malformed profile value from
// killing a delivery.
func TestSubscriberLocation_BadTimezoneIsUTC(t *testing.T) {
	st := tzStore(t)
	runID := seedRunForSubscriber(t, st, "Mars/Olympus_Mons")

	if got := NewDelayWait(st).subscriberLocation(context.Background(), runID); got != time.UTC {
		t.Errorf("unparseable timezone = %v, want UTC", got)
	}
}

// TestSubscriberLocation_UnknownRunIsUTC covers the lookup failing entirely --
// the send should still be scheduled, on the previous UTC basis.
func TestSubscriberLocation_UnknownRunIsUTC(t *testing.T) {
	st := tzStore(t)

	if got := NewDelayWait(st).subscriberLocation(context.Background(), "no-such-run"); got != time.UTC {
		t.Errorf("unknown run = %v, want UTC", got)
	}
}
