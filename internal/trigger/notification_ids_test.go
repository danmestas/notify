package trigger_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// notificationIDsFor returns the ids of every notification row of
// transactionID, ordered by subscriber id.
func notificationIDsFor(t *testing.T, dbPath, transactionID string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", dbPath))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id FROM notifications WHERE transaction_id = ? ORDER BY subscriber_id`, transactionID)
	if err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestIngest_NotificationIDs_SingleRecipient(t *testing.T) {
	svc, scope, dbPath := setupIdempotency(t)
	resp, err := svc.Ingest(context.Background(), scope, trigger.TriggerRequest{
		Name:          idempotencyTestTrigger,
		To:            trigger.ToField{SubscriberID: "u1"},
		Payload:       json.RawMessage(`{}`),
		TransactionID: "tx-ids-1",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	want := notificationIDsFor(t, dbPath, "tx-ids-1")
	if len(want) != 1 || !reflect.DeepEqual(resp.NotificationIDs, want) {
		t.Fatalf("NotificationIDs = %v, want the one stored row id %v", resp.NotificationIDs, want)
	}
	raw, _ := json.Marshal(resp)
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ids, ok := wire["notificationIds"].([]any)
	if !ok || len(ids) != 1 || ids[0] != want[0] {
		t.Fatalf("wire notificationIds = %v, want [%s]", wire["notificationIds"], want[0])
	}
}

func TestIngest_NotificationIDs_TopicOnePerRecipient(t *testing.T) {
	svc, scope, dbPath := setupIdempotency(t)
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	aud := audience.New(st)
	if _, err := st.Topics().Create(ctx, scope, "t1", "Team"); err != nil {
		t.Fatalf("Topics().Create: %v", err)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		if _, err := aud.Upsert(ctx, scope, audience.SubscriberInput{SubscriberID: id}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := st.Topics().AddSubscriber(ctx, scope, "t1", id); err != nil {
			t.Fatalf("AddSubscriber: %v", err)
		}
	}
	resp, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
		Name:          idempotencyTestTrigger,
		To:            trigger.ToField{TopicKey: "t1"},
		Payload:       json.RawMessage(`{}`),
		TransactionID: "tx-ids-topic",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	want := notificationIDsFor(t, dbPath, "tx-ids-topic")
	if len(want) != 3 || len(resp.NotificationIDs) != 3 {
		t.Fatalf("NotificationIDs = %v, stored %v, want 3 each", resp.NotificationIDs, want)
	}
	seen := map[string]bool{}
	for _, id := range resp.NotificationIDs {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("stored id %s missing from response %v", id, resp.NotificationIDs)
		}
	}
}

func TestIngest_NotificationIDs_NoneWhenRefused(t *testing.T) {
	svc, scope, dbPath := setupIdempotency(t)
	ctx := context.Background()
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", dbPath))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE workflows SET active = 0`); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	for _, name := range []string{idempotencyTestTrigger, "no-such-workflow"} {
		resp, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
			Name: name, To: trigger.ToField{SubscriberID: "u1"}, Payload: json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("Ingest %s: %v", name, err)
		}
		if resp.Acknowledged || len(resp.NotificationIDs) != 0 {
			t.Fatalf("%s: resp = %+v, want refused with no ids", name, resp)
		}
		raw, _ := json.Marshal(resp)
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, present := wire["notificationIds"]; present {
			t.Fatalf("%s: wire = %s, want no notificationIds key", name, raw)
		}
	}
}

// A retry of the same transactionId reuses the notification row, so it answers
// with the ids the first attempt wrote.
func TestIngest_NotificationIDs_RetryReturnsSameIDs(t *testing.T) {
	svc, scope, dbPath := setupIdempotency(t)
	req := trigger.TriggerRequest{
		Name:          idempotencyTestTrigger,
		To:            trigger.ToField{SubscriberID: "u1"},
		Payload:       json.RawMessage(`{}`),
		TransactionID: "tx-ids-retry",
	}
	first, err := svc.Ingest(context.Background(), scope, req)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	second, err := svc.Ingest(context.Background(), scope, req)
	if err != nil {
		t.Fatalf("Ingest retry: %v", err)
	}
	if len(first.NotificationIDs) != 1 || !reflect.DeepEqual(first.NotificationIDs, second.NotificationIDs) {
		t.Fatalf("ids first=%v retry=%v, want identical single id", first.NotificationIDs, second.NotificationIDs)
	}
	if got := notificationIDsFor(t, dbPath, "tx-ids-retry"); !reflect.DeepEqual(got, first.NotificationIDs) {
		t.Fatalf("stored rows %v, want %v", got, first.NotificationIDs)
	}
}
