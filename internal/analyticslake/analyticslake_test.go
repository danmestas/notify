package analyticslake

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/dagnats/lake"
	"github.com/danmestas/dagnats/lake/writer"

	"github.com/danmestas/notify/internal/domain"
)

// fakeRows is a Rows source in rowid order.
type fakeRows []domain.AnalyticsDelivery

func (f fakeRows) EachAnalyticsDelivery(_ context.Context, fn func(domain.AnalyticsDelivery) error) error {
	for _, d := range f {
		if err := fn(d); err != nil {
			return err
		}
	}
	return nil
}

func sampleRows() fakeRows {
	return fakeRows{
		{
			ID: "01AAA", OrgID: "o1", EnvID: "e1", Channel: "email", Status: "sent",
			SubscriberID: "sub1", WorkflowID: "wf1", TransactionID: "tx1", RunID: "run1",
			CreatedAt: "2026-11-02T10:00:00.000000000Z", EventKey: "01MSG:sent",
		},
		// Pre-0052: no event key, so no message id.
		{
			ID: "01BBB", OrgID: "o1", EnvID: "e1", Channel: "sms", Status: "failed",
			CreatedAt: "2026-11-01T23:59:59.999999999Z",
		},
		// After the cutoff: left to the live stream.
		{
			ID: "01CCC", OrgID: "o1", EnvID: "e1", Channel: "email", Status: "sent",
			CreatedAt: "2026-11-08T17:09:48.000000000Z", EventKey: "01LIVE:sent",
		},
		{
			ID: "01DDD", OrgID: "o1", EnvID: "e2", Channel: "push", Status: "sent",
			CreatedAt: "2026-11-02T11:00:00.5Z", EventKey: "01MSG2:sent",
		},
	}
}

func TestPlan_RowsBeforeCutoffInRowidOrder(t *testing.T) {
	plan, err := NewPlan(context.Background(), sampleRows(), DefaultCutoff)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if got, want := plan.Days(), []Day{{Dt: "2026-11-01", Rows: 1}, {Dt: "2026-11-02", Rows: 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("days: got %+v want %+v", got, want)
	}
	if plan.Rows() != 3 {
		t.Fatalf("rows: got %d want 3", plan.Rows())
	}

	byID := map[string]writer.Row{}
	for _, rows := range plan.byDay {
		for _, r := range rows {
			byID[r.EventID] = r
		}
	}
	keyed, ok := byID["01MSG:sent"]
	if !ok {
		t.Fatalf("keyed row's event id is not its event_key; have %v", byID)
	}
	if keyed.StreamSeq != 1 {
		t.Fatalf("keyed seq: got %d want 1", keyed.StreamSeq)
	}
	wantTime := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	if !keyed.EventTime.Equal(wantTime) || keyed.EventTime.Location() != time.UTC {
		t.Fatalf("event time: got %v want %v", keyed.EventTime, wantTime)
	}
	if got, want := keyed.Values, map[string]any{"subject": "notify.analytics.e1", "time_ns": wantTime.UnixNano()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("values: got %v want %v", got, want)
	}
	if keyed.Payload != "" {
		t.Fatalf("payload string must be empty in a PayloadBytes table")
	}
	var body map[string]any
	if err := json.Unmarshal(keyed.PayloadBytes, &body); err != nil {
		t.Fatalf("payload: %v", err)
	}
	wantBody := map[string]any{
		"v": float64(1), "messageId": "01MSG", "orgId": "o1", "envId": "e1",
		"workflowId": "wf1", "notificationId": "", "runId": "run1", "stepId": "",
		"transactionId": "tx1", "integrationId": "", "subscriberId": "sub1",
		"channel": "email", "providerId": "", "status": "sent", "errorClass": "",
		"createdAt": "2026-11-02T10:00:00.000000000Z", "at": "",
	}
	if !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("payload:\n got %v\nwant %v", body, wantBody)
	}

	old, ok := byID["analytics-row:01BBB"]
	if !ok {
		t.Fatalf("old row's event id: have %v", byID)
	}
	if old.StreamSeq != 2 {
		t.Fatalf("old seq: got %d want 2", old.StreamSeq)
	}
	var oldBody map[string]any
	if err := json.Unmarshal(old.PayloadBytes, &oldBody); err != nil {
		t.Fatalf("old payload: %v", err)
	}
	if oldBody["messageId"] != "" || oldBody["status"] != "failed" {
		t.Fatalf("old payload: %v", oldBody)
	}
	// The row after the cutoff takes no seq: the next row is 3, not 4.
	if got := byID["01MSG2:sent"].StreamSeq; got != 3 {
		t.Fatalf("seq after the skipped row: got %d want 3", got)
	}
	if _, ok := byID["01LIVE:sent"]; ok {
		t.Fatalf("a row at the cutoff was imported")
	}
}

func TestPlan_BadCreatedAtFails(t *testing.T) {
	rows := fakeRows{{ID: "x", OrgID: "o", EnvID: "e", Channel: "email", Status: "sent", CreatedAt: "yesterday"}}
	if _, err := NewPlan(context.Background(), rows, DefaultCutoff); err == nil {
		t.Fatalf("want an error for an unparseable created_at")
	}
}

func TestEventID(t *testing.T) {
	cases := []struct {
		name string
		d    domain.AnalyticsDelivery
		id   string
		msg  string
	}{
		{"keyed", domain.AnalyticsDelivery{ID: "r", Status: "sent", EventKey: "01M:sent"}, "01M:sent", "01M"},
		{"pre-0052", domain.AnalyticsDelivery{ID: "r", Status: "sent"}, "analytics-row:r", ""},
		{"hashed payload", domain.AnalyticsDelivery{ID: "r", Status: "sent", EventKey: "ab12"}, "ab12", ""},
	}
	for _, c := range cases {
		id, msg := eventID(c.d)
		if id != c.id || msg != c.msg {
			t.Errorf("%s: got (%q, %q) want (%q, %q)", c.name, id, msg, c.id, c.msg)
		}
	}
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(files)
	return files
}

func TestPlan_WriteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write := func() []lake.Commit {
		plan, err := NewPlan(ctx, sampleRows(), DefaultCutoff)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		commits, err := plan.Write(ctx, writer.NewLocalStore(root))
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		return commits
	}
	first := write()
	files := listFiles(t, root)
	second := write()
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("commits: first %d second %d, want 2 each", len(first), len(second))
	}
	for i := range first {
		if first[i].Key != second[i].Key || first[i].Digest != second[i].Digest {
			t.Fatalf("rerun commit %d differs: %+v vs %+v", i, first[i], second[i])
		}
		if first[i].Stream != ImportStream {
			t.Fatalf("stream: got %q want %q", first[i].Stream, ImportStream)
		}
	}
	if got := listFiles(t, root); !reflect.DeepEqual(got, files) {
		t.Fatalf("rerun changed the lake:\n before %v\n after %v", files, got)
	}
	for _, f := range files {
		if filepath.Dir(filepath.Dir(f)) != "notify/stream_notify_analytics" {
			t.Fatalf("file outside notify/stream_notify_analytics: %s", f)
		}
	}

	listing, err := lake.ListCommitted(root, Program, Table)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listing.Commits) != 2 || len(listing.Rejected) != 0 {
		t.Fatalf("listing: %+v", listing)
	}
}

// TestPlan_RowidOrderAcrossDatesOverlaps pins a property of numbering in rowid
// order while partitioning by created_at date: when a later row carries an
// earlier date (a late status event of an older message), the dates' seq
// ranges interleave, and lake.ListCommitted reports the parts as Overlaps,
// its signature of a duplicate. Readers dedupe on event_id, so counts are
// unaffected; whether to number in (created_at, rowid) order instead is open
// on #119.
func TestPlan_RowidOrderAcrossDatesOverlaps(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	plan, err := NewPlan(ctx, sampleRows(), DefaultCutoff)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := plan.Write(ctx, writer.NewLocalStore(root)); err != nil {
		t.Fatalf("write: %v", err)
	}
	listing, err := lake.ListCommitted(root, Program, Table)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// dt=2026-11-02 holds seqs 1 and 3, dt=2026-11-01 holds seq 2.
	if len(listing.Overlaps) != 1 {
		t.Fatalf("overlaps: got %d want 1: %+v", len(listing.Overlaps), listing.Overlaps)
	}
}

// TestPlan_WriteChunksPerDate: a date past the chunk size becomes several
// parts with contiguous seq ranges, and the split is the same on a rerun.
func TestPlan_WriteChunksPerDate(t *testing.T) {
	ctx := context.Background()
	var rows fakeRows
	for i := 0; i < 5; i++ {
		rows = append(rows, domain.AnalyticsDelivery{
			ID: string(rune('a' + i)), OrgID: "o", EnvID: "e", Channel: "email", Status: "sent",
			CreatedAt: "2026-11-03T00:00:00.000000000Z",
		})
	}
	plan, err := NewPlan(ctx, rows, DefaultCutoff)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan.chunkRows = 2
	commits, err := plan.Write(ctx, writer.NewLocalStore(t.TempDir()))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	var ranges [][2]int64
	for _, c := range commits {
		ranges = append(ranges, [2]int64{c.SeqMin, c.SeqMax})
	}
	if want := [][2]int64{{1, 2}, {3, 4}, {5, 5}}; !reflect.DeepEqual(ranges, want) {
		t.Fatalf("seq ranges: got %v want %v", ranges, want)
	}
}

func TestLiveParts_Local(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg := Config{Dir: root}
	parts, err := cfg.LiveParts(ctx)
	if err != nil || len(parts) != 0 {
		t.Fatalf("empty lake: got %v, %v", parts, err)
	}
	plan, err := NewPlan(ctx, sampleRows(), DefaultCutoff)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	st, err := cfg.Store()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := plan.Write(ctx, st); err != nil {
		t.Fatalf("write: %v", err)
	}
	// An orphan part (no commit record) is never read.
	orphan := filepath.Join(root, "notify", "stream_notify_analytics", "dt=2026-11-05", "part-0000000000000000.parquet")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	parts, err = cfg.LiveParts(ctx)
	if err != nil {
		t.Fatalf("parts: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts: got %v want 2", parts)
	}
	for _, p := range parts {
		if p == orphan {
			t.Fatalf("orphan listed")
		}
	}
}

func TestLiveKeys_SkipsUncommittedAndSuperseded(t *testing.T) {
	keys := []string{
		"notify/stream_notify_analytics/dt=2026-11-01/part-aaaa.parquet",
		"notify/stream_notify_analytics/dt=2026-11-01/part-aaaa.json",
		"notify/stream_notify_analytics/dt=2026-11-01/part-bbbb.parquet",
		"notify/stream_notify_analytics/dt=2026-11-02/part-cccc.parquet",
		"notify/stream_notify_analytics/dt=2026-11-02/part-cccc.json",
		"notify/stream_notify_analytics/dt=2026-11-02/part-cccc.json.superseded",
		"notify/stream_notify_analytics/dt=2026-11-02/part-dddd.json",
	}
	got := liveKeys(keys)
	want := []string{"notify/stream_notify_analytics/dt=2026-11-01/part-aaaa.parquet"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"NOTIFY_LAKE_R2_ENDPOINT":          "https://acct.r2.cloudflarestorage.com",
		"NOTIFY_LAKE_R2_ACCESS_KEY_ID":     "AKID-TEST-1",
		"NOTIFY_LAKE_R2_SECRET_ACCESS_KEY": "SECRET-TEST-2",
	}
	cfg, err := ConfigFromEnv("r2", func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("r2: %v", err)
	}
	want := Config{
		Endpoint: "https://acct.r2.cloudflarestorage.com", Bucket: "lode", Prefix: "mesh",
		AccessKey: "AKID-TEST-1", SecretKey: "SECRET-TEST-2",
	}
	if cfg != want {
		t.Fatalf("r2 config: got %+v want %+v", cfg, want)
	}
	if _, err := ConfigFromEnv("r2", func(string) string { return "" }); err == nil {
		t.Fatalf("r2 without credentials must fail")
	}
	local, err := ConfigFromEnv("/var/lake", func(string) string { return "" })
	if err != nil || local != (Config{Dir: "/var/lake"}) {
		t.Fatalf("local: got %+v, %v", local, err)
	}
	off, err := ConfigFromEnv("", func(string) string { return "" })
	if err != nil || off.Enabled() {
		t.Fatalf("off: got %+v, %v", off, err)
	}
	if s := cfg.String(); s == "" || strings.Contains(s, "AKID-TEST-1") || strings.Contains(s, "SECRET-TEST-2") {
		t.Fatalf("String leaks credentials: %q", s)
	}
}
