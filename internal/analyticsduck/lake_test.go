//go:build duckdb

package analyticsduck

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/danmestas/dagnats/lake/writer"

	"github.com/danmestas/notify/internal/analyticslake"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/store"
)

var _ store.AnalyticsRepo = (*LakeRepo)(nil)

// seedSQLite writes a spread of rows across scopes, days, workflows,
// subscribers and runs, some with event keys and some without (pre-0052).
func seedSQLite(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	channels := []string{"email", "sms", "in_app"}
	statuses := []string{"sent", "delivered", "failed"}
	scopes := []domain.Scope{{OrgID: "o1", EnvID: "e1"}, {OrgID: "o1", EnvID: "e2"}, {OrgID: "o2", EnvID: "e1"}}
	for i := 0; i < 90; i++ {
		d := domain.AnalyticsDelivery{
			Channel:    channels[i%3],
			Status:     statuses[(i/3)%3],
			WorkflowID: fmt.Sprintf("wf%d", i%2),
			CreatedAt:  fmt.Sprintf("2026-11-0%dT%02d:00:00.000000000Z", 1+i%5, i%24),
		}
		if i%4 != 0 {
			d.SubscriberID = fmt.Sprintf("sub%d", i%7)
		}
		if i%5 != 0 {
			d.RunID = fmt.Sprintf("run%d", i%11)
		}
		if i%3 != 0 {
			d.EventKey = fmt.Sprintf("msg%d:%s", i, d.Status)
		}
		if err := st.Analytics().Insert(ctx, scopes[i%3], d); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
}

// writeLivePart writes one event as Lode would drain it from NOTIFY_ANALYTICS,
// duplicating an imported event id: the reader must count it once.
func writeLivePart(t *testing.T, root string, seq int64, eventID string, ev eventbus.AnalyticsEvent) {
	t.Helper()
	body, err := ev.Encode()
	if err != nil {
		t.Fatal(err)
	}
	w, err := writer.New(writer.NewLocalStore(root), analyticslake.Program, writer.Config{
		Table: analyticslake.Table, Stream: "NOTIFY_ANALYTICS", Schema: analyticslake.Schema(),
	})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC)
	if err := w.Append(writer.Row{
		EventID: eventID, EventTime: at, StreamSeq: seq, PayloadBytes: body,
		Values: map[string]any{"subject": eventbus.AnalyticsSubject(ev.EnvID), "time_ns": at.UnixNano()},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestLakeRepo_MatchesSQLite imports SQLite rows with the import writer into a
// local lake, then answers every analytics query from the lake and from SQLite
// and requires the same numbers.
func TestLakeRepo_MatchesSQLite(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	seedSQLite(t, st)

	root := t.TempDir()
	cfg := analyticslake.Config{Dir: root}
	plan, err := analyticslake.NewPlan(ctx, st, analyticslake.DefaultCutoff)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Rows() != 90 {
		t.Fatalf("plan rows: got %d want 90", plan.Rows())
	}
	lakeStore, err := cfg.Store()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Write(ctx, lakeStore); err != nil {
		t.Fatalf("write: %v", err)
	}
	// msg1 is imported row 1 (o1/e2, sms, sent); the live stream holds it too.
	writeLivePart(t, root, 1, "msg1:sent", eventbus.AnalyticsEvent{
		V: eventbus.AnalyticsEventVersion, MessageID: "msg1", OrgID: "o1", EnvID: "e2",
		WorkflowID: "wf1", RunID: "run1", SubscriberID: "sub1", Channel: "sms", Status: "sent",
		CreatedAt: "2026-11-02T01:00:00.000000000Z", At: "2026-11-08T18:00:00.000000000Z",
	})

	// A live-only event: published (event_time, dt, at) on Nov 8 for a message
	// created Nov 3. SQLite files it under Nov 3, so the lake must too.
	late := domain.AnalyticsDelivery{
		Channel: "email", Status: "delivered", SubscriberID: "sub9", WorkflowID: "wf1",
		RunID: "run9", CreatedAt: "2026-11-03T05:00:00.000000000Z", EventKey: "late1:delivered",
	}
	if err := st.Analytics().Insert(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"}, late); err != nil {
		t.Fatalf("insert late: %v", err)
	}
	writeLivePart(t, root, 2, late.EventKey, eventbus.AnalyticsEvent{
		V: eventbus.AnalyticsEventVersion, MessageID: "late1", OrgID: "o1", EnvID: "e1",
		WorkflowID: late.WorkflowID, RunID: late.RunID, SubscriberID: late.SubscriberID,
		Channel: late.Channel, Status: late.Status, CreatedAt: late.CreatedAt,
		At: "2026-11-08T18:00:00.000000000Z",
	})

	repo, err := OpenLake(ctx, cfg, st.Analytics())
	if err != nil {
		t.Fatalf("OpenLake: %v", err)
	}
	defer func() { _ = repo.Close() }()
	sqlite := st.Analytics()

	since := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	scopes := []domain.Scope{
		{OrgID: "o1", EnvID: "e1"},
		{OrgID: "o1", EnvID: "e2"},
		{OrgID: "o2", EnvID: "e1"},
		{OrgID: "o2", EnvID: "e2"}, // empty
	}
	filters := []store.AnalyticsFilter{{}, {WorkflowID: "wf1"}}
	for _, scope := range scopes {
		for _, f := range filters {
			name := fmt.Sprintf("%s/%s wf=%q", scope.OrgID, scope.EnvID, f.WorkflowID)
			compare(t, name+" CountByChannelStatus",
				func(r store.AnalyticsRepo) (any, error) { return r.CountByChannelStatus(ctx, scope, f) }, sqlite, repo)
			compare(t, name+" DeliveriesByDay",
				func(r store.AnalyticsRepo) (any, error) { return r.DeliveriesByDay(ctx, scope, f) }, sqlite, repo)
			compare(t, name+" RunsByDay",
				func(r store.AnalyticsRepo) (any, error) { return r.RunsByDay(ctx, scope, f) }, sqlite, repo)
			for _, s := range []time.Time{{}, since} {
				compare(t, name+" CountActiveSubscribers",
					func(r store.AnalyticsRepo) (any, error) { return r.CountActiveSubscribers(ctx, scope, s, f) }, sqlite, repo)
				compare(t, name+" AverageMessagesPerSubscriber",
					func(r store.AnalyticsRepo) (any, error) {
						return r.AverageMessagesPerSubscriber(ctx, scope, s, f)
					}, sqlite, repo)
			}
		}
	}

	// A non-empty result, so the comparison is not vacuous.
	got, err := repo.CountByChannelStatus(ctx, scopes[0], store.AnalyticsFilter{})
	if err != nil || len(got) == 0 {
		t.Fatalf("lake counts empty: %v %v", got, err)
	}
}

func compare(t *testing.T, name string, query func(store.AnalyticsRepo) (any, error), want, got store.AnalyticsRepo) {
	t.Helper()
	w, err := query(want)
	if err != nil {
		t.Fatalf("%s sqlite: %v", name, err)
	}
	g, err := query(got)
	if err != nil {
		t.Fatalf("%s lake: %v", name, err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Errorf("%s:\n sqlite %v\n lake   %v", name, w, g)
	}
}

// TestLakeRepo_EmptyLake answers from a lake with no parts, and Insert goes to
// the wrapped repo.
func TestLakeRepo_EmptyLake(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	repo, err := OpenLake(ctx, analyticslake.Config{Dir: t.TempDir()}, st.Analytics())
	if err != nil {
		t.Fatalf("OpenLake: %v", err)
	}
	defer func() { _ = repo.Close() }()
	scope := domain.Scope{OrgID: "o", EnvID: "e"}
	got, err := repo.CountByChannelStatus(ctx, scope, store.AnalyticsFilter{})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty lake: %v %v", got, err)
	}
	days, err := repo.DeliveriesByDay(ctx, scope, store.AnalyticsFilter{})
	if err != nil || len(days) != 0 {
		t.Fatalf("empty lake days: %v %v", days, err)
	}
	if err := repo.Insert(ctx, scope, domain.AnalyticsDelivery{Channel: "email", Status: "sent"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	inSQLite, err := st.Analytics().CountByChannelStatus(ctx, scope, store.AnalyticsFilter{})
	if err != nil || inSQLite["email"]["sent"] != 1 {
		t.Fatalf("insert did not reach the wrapped repo: %v %v", inSQLite, err)
	}
}
