// Package analyticslake is notify's side of the analytics lake (#119): the
// one-shot import of SQLite analytics_deliveries rows into Lode's
// stream_notify_analytics table, and the listing of the parts a reader may
// trust. Lode drains NOTIFY_ANALYTICS into the same table; this package writes
// the rows from before the live publisher existed, in Lode's exact shape, under
// the separate stream notify-import.
//
// Layout (see github.com/danmestas/dagnats/lake):
//
//	<prefix>/notify/stream_notify_analytics/dt=YYYY-MM-DD/part-<sha>.parquet
//	<prefix>/notify/stream_notify_analytics/dt=YYYY-MM-DD/part-<sha>.json
//
// The package is pure Go; reading the parts with DuckDB is
// internal/analyticsduck's LakeRepo, behind the duckdb build tag.
package analyticslake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danmestas/dagnats/lake"
	"github.com/danmestas/dagnats/lake/writer"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/timestamp"
)

const (
	// Program is notify's program prefix in the lake.
	Program = "notify"
	// Table is Lode's table for the NOTIFY_ANALYTICS stream.
	Table = "stream_notify_analytics"
	// ImportStream names the import in every commit and part digest, so its
	// sequence range never overlaps the live stream's.
	ImportStream = "notify-import"
	// oldRowIDPrefix makes the event id of a row with no event_key. See eventID.
	oldRowIDPrefix = "analytics-row:"
)

// DefaultCutoff is when the live NOTIFY_ANALYTICS publisher started (#126 on
// the mini). Rows created before it are the import's; the rest are the live
// stream's.
var DefaultCutoff = time.Date(2026, 11, 8, 17, 9, 48, 0, time.UTC)

// Schema is Lode's schema for a drained stream: payload as raw bytes plus the
// stream message's subject, epoch, headers and time.
func Schema() lake.Schema {
	return lake.Schema{Version: 3, PayloadBytes: true, Columns: []lake.Column{
		{Name: "subject", Type: lake.TypeString},
		{Name: "epoch", Type: lake.TypeInt64},
		{Name: "headers", Type: lake.TypeBytes},
		{Name: "time_ns", Type: lake.TypeInt64},
	}}
}

// Rows is the import's source: *store.Store walks analytics_deliveries in
// rowid order.
type Rows interface {
	EachAnalyticsDelivery(ctx context.Context, fn func(domain.AnalyticsDelivery) error) error
}

// Day is one event date of a plan and how many rows it holds.
type Day struct {
	Dt   string
	Rows int
}

// Plan is the import's rows, numbered and grouped by event date, before any
// write. A dry run reports its Days; Write commits it.
type Plan struct {
	byDay map[string][]writer.Row // dt -> rows in seq order
	rows  int
	// chunkRows is the most rows one Flush takes for a date: writer.PartRowsMax.
	// Tests lower it.
	chunkRows int
}

// NewPlan reads every row created before cutoff and numbers them 1..N with no
// holes in (created_at, rowid) order. created_at is the message's creation
// time, so a late status event has a later rowid but an earlier date; numbering
// by rowid alone would interleave the dates' seq ranges, and lake.ListCommitted
// would report the per-date parts as Overlaps, its duplicate signal. Ordering
// by created_at first keeps each date's seqs contiguous and its parts disjoint.
//
// The whole import is held in memory: it is a one-shot over a table that
// predates the lake. A created_at that does not parse is an error, since
// dropping the row would make the numbers disagree with SQLite.
func NewPlan(ctx context.Context, src Rows, cutoff time.Time) (*Plan, error) {
	type kept struct {
		d       domain.AnalyticsDelivery
		created time.Time
	}
	var rows []kept
	err := src.EachAnalyticsDelivery(ctx, func(d domain.AnalyticsDelivery) error {
		created, err := timestamp.Parse(d.CreatedAt)
		if err != nil {
			return fmt.Errorf("analytics row %s: created_at %q: %w", d.ID, d.CreatedAt, err)
		}
		if created.Before(cutoff) {
			rows = append(rows, kept{d, created})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Stable: rows arrive in rowid order, which breaks created_at ties.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].created.Before(rows[j].created) })

	plan := &Plan{byDay: map[string][]writer.Row{}, chunkRows: writer.PartRowsMax}
	for i, k := range rows {
		row, err := lakeRow(k.d, k.created, int64(i+1))
		if err != nil {
			return nil, err
		}
		dt := row.EventTime.Format(time.DateOnly)
		plan.byDay[dt] = append(plan.byDay[dt], row)
	}
	plan.rows = len(rows)
	return plan, nil
}

// Rows is the number of rows in the plan.
func (p *Plan) Rows() int { return p.rows }

// Days lists the plan's event dates in order with their row counts.
func (p *Plan) Days() []Day {
	days := make([]Day, 0, len(p.byDay))
	for _, dt := range p.dates() {
		days = append(days, Day{Dt: dt, Rows: len(p.byDay[dt])})
	}
	return days
}

func (p *Plan) dates() []string {
	dts := make([]string, 0, len(p.byDay))
	for dt := range p.byDay {
		dts = append(dts, dt)
	}
	sort.Strings(dts)
	return dts
}

// Write commits the plan through one lake writer, one date at a time, in
// chunks of at most chunkRows rows, flushing after each chunk and at no other
// time (no timer, no buffer pressure). A part's key is a digest of its exact
// rows, so the batch boundaries must be a function of the rows alone: then a
// rerun over the same rows makes the same parts, the writer finds them already
// committed, and nothing overlaps for Lode's compaction to refuse.
func (p *Plan) Write(ctx context.Context, store writer.Store) ([]lake.Commit, error) {
	w, err := writer.New(store, Program, writer.Config{
		Table: Table, Stream: ImportStream, Schema: Schema(),
	})
	if err != nil {
		return nil, err
	}
	var commits []lake.Commit
	for _, dt := range p.dates() {
		rows := p.byDay[dt]
		for low := 0; low < len(rows); low += p.chunkRows {
			high := min(low+p.chunkRows, len(rows))
			for _, row := range rows[low:high] {
				if err := w.Append(row); err != nil {
					return commits, err
				}
			}
			made, err := w.Flush(ctx)
			commits = append(commits, made...)
			if err != nil {
				return commits, err
			}
		}
	}
	return commits, nil
}

// lakeRow is the row Lode would have drained for d, had the live publisher
// existed: the same v1 event, encoded by the same code, on the same subject.
func lakeRow(d domain.AnalyticsDelivery, created time.Time, seq int64) (writer.Row, error) {
	id, messageID := eventID(d)
	// The row lacks notificationId, stepId, integrationId, providerId,
	// errorClass and the publish time (at); they stay empty.
	body, err := eventbus.AnalyticsEvent{
		V: eventbus.AnalyticsEventVersion, MessageID: messageID, OrgID: d.OrgID, EnvID: d.EnvID,
		WorkflowID: d.WorkflowID, RunID: d.RunID, TransactionID: d.TransactionID,
		SubscriberID: d.SubscriberID, Channel: d.Channel, Status: d.Status,
		CreatedAt: d.CreatedAt,
	}.Encode()
	if err != nil {
		return writer.Row{}, fmt.Errorf("analytics row %s: encode: %w", d.ID, err)
	}
	created = created.UTC()
	return writer.Row{
		EventID: id, EventTime: created, StreamSeq: seq, PayloadBytes: body,
		Values: map[string]any{
			"subject": eventbus.AnalyticsSubject(d.EnvID),
			"time_ns": created.UnixNano(),
		},
	}, nil
}

// eventID is a row's lake event id and the message id it names.
//
// A row with an event_key (migration 0052 on) keeps it: it is
// eventbus.DeliveryEventKey of the source event, the same "<message id>:<status>"
// the live publisher sets as Nats-Msg-Id, so a reader deduplicating on event_id
// counts an event once even when both the import and the live stream hold it.
// The message id is the key less its ":<status>" suffix. A key without that
// suffix is the SHA-256 fallback for a payload with no message id; it is kept,
// with no message id.
//
// A row from before 0052 has no event_key, and the table never stored the
// message id, so it cannot be recovered. Its id is "analytics-row:<row id>".
// That cannot collide with a live id: a live id is either "<message id>:<status>",
// where the message id is a notify ULID (never "analytics-row") and the status
// is a delivery status (never a ULID), or 64 hex digits, which have no colon.
// Row ids are the table's primary key, so two old rows never share one either.
func eventID(d domain.AnalyticsDelivery) (id, messageID string) {
	if d.EventKey == "" {
		return oldRowIDPrefix + d.ID, ""
	}
	if msg, ok := strings.CutSuffix(d.EventKey, ":"+d.Status); ok && msg != "" {
		return d.EventKey, msg
	}
	return d.EventKey, ""
}
