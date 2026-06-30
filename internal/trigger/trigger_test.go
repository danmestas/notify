package trigger_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

const (
	testTrigger = "welcome-flow"
)

// setup spins a real sidecar + engine, a temp SQLite store, seeds a 1-step
// in_app workflow, and registers the matching DAG with the engine.
func setup(t *testing.T) (*trigger.Service, domain.Scope, string) {
	t.Helper()

	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	dbPath := filepath.Join(t.TempDir(), "trigger.db")
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	steps, err := json.Marshal([]domain.Step{{
		StepID:        "S1",
		Type:          "in_app",
		ControlValues: json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"You have {{payload.itemCount}} items"}`),
	}})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	if _, err := st.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Welcome",
		TriggerIdentifier: testTrigger,
		Active:            true,
		Steps:             steps,
	}); err != nil {
		t.Fatalf("Workflows().Create: %v", err)
	}

	dagName := scope.EnvID + "-" + testTrigger
	if err := eng.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  dagName,
		Steps: []engine.StepDef{{ID: "S1", Task: "send.in_app", Type: "normal"}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	return trigger.New(audience.New(st), st, eng), scope, dbPath
}

// fetchNotification reads back the notification row by transaction id via a
// raw read-only connection (the store exposes no list accessor).
func fetchNotification(t *testing.T, dbPath, transactionID string) (runID string, found bool) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open verify db: %v", err)
	}
	defer db.Close()
	err = db.QueryRow(
		`SELECT run_id FROM notifications WHERE transaction_id = ?`,
		transactionID,
	).Scan(&runID)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("query notification: %v", err)
	}
	return runID, true
}

func TestIngest_ProcessedAndPersists(t *testing.T) {
	svc, scope, dbPath := setup(t)
	ctx := context.Background()

	resp, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
		Name:          testTrigger,
		To:            trigger.ToField{SubscriberID: "user_123", FirstName: "Ada"},
		Payload:       json.RawMessage(`{"itemCount":3}`),
		TransactionID: "tx-1",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !resp.Acknowledged {
		t.Errorf("Acknowledged = false, want true")
	}
	if resp.Status != "processed" {
		t.Errorf("Status = %q, want %q", resp.Status, "processed")
	}
	if resp.TransactionID != "tx-1" {
		t.Errorf("TransactionID = %q, want %q", resp.TransactionID, "tx-1")
	}
	if len(resp.Error) != 0 {
		t.Errorf("Error = %v, want empty on success", resp.Error)
	}

	runID, found := fetchNotification(t, dbPath, "tx-1")
	if !found {
		t.Fatal("no notifications row for tx-1")
	}
	if runID == "" {
		t.Error("notification RunID is empty, want non-empty")
	}
}

func TestIngest_MintsTransactionIDWhenEmpty(t *testing.T) {
	svc, scope, _ := setup(t)
	ctx := context.Background()

	resp, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
		Name:    testTrigger,
		To:      trigger.ToField{SubscriberID: "user_123", FirstName: "Ada"},
		Payload: json.RawMessage(`{"itemCount":3}`),
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !resp.Acknowledged {
		t.Errorf("Acknowledged = false, want true")
	}
	if len(resp.TransactionID) != 26 {
		t.Errorf("TransactionID = %q (len %d), want a 26-char minted ULID",
			resp.TransactionID, len(resp.TransactionID))
	}
}

func TestIngest_UnknownWorkflow(t *testing.T) {
	svc, scope, _ := setup(t)
	ctx := context.Background()

	resp, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
		Name:    "does-not-exist",
		To:      trigger.ToField{SubscriberID: "user_123"},
		Payload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Ingest returned transport error, want nil: %v", err)
	}
	if resp.Acknowledged {
		t.Errorf("Acknowledged = true, want false for unknown workflow")
	}
	if resp.Status != "error" {
		t.Errorf("Status = %q, want %q", resp.Status, "error")
	}
	if len(resp.Error) == 0 {
		t.Error("Error is empty, want a message for unknown workflow")
	}
}

// TestIngest_EmailWorkflow proves trigger generalizes beyond in_app: a workflow
// whose single step is Type:"email" ingests, dispatches send.email via StartRun,
// and persists a notification with a non-empty run_id.
func TestIngest_EmailWorkflow(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	dbPath := filepath.Join(t.TempDir(), "trigger.db")
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	const emailTrigger = "email-flow"

	steps, err := json.Marshal([]domain.Step{{
		StepID:        "S1",
		Type:          "email",
		ControlValues: json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"You have {{payload.itemCount}} items"}`),
	}})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	if _, err := st.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "EmailWelcome",
		TriggerIdentifier: emailTrigger,
		Active:            true,
		Steps:             steps,
	}); err != nil {
		t.Fatalf("Workflows().Create: %v", err)
	}

	dagName := scope.EnvID + "-" + emailTrigger
	if err := eng.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  dagName,
		Steps: []engine.StepDef{{ID: "S1", Task: "send.email", Type: "normal"}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	svc := trigger.New(audience.New(st), st, eng)
	resp, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
		Name:          emailTrigger,
		To:            trigger.ToField{SubscriberID: "user_123", FirstName: "Ada", Email: "ada@example.com"},
		Payload:       json.RawMessage(`{"itemCount":3}`),
		TransactionID: "tx-email-1",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !resp.Acknowledged {
		t.Errorf("Acknowledged = false, want true")
	}
	if resp.Status != "processed" {
		t.Errorf("Status = %q, want %q", resp.Status, "processed")
	}

	runID, found := fetchNotification(t, dbPath, "tx-email-1")
	if !found {
		t.Fatal("no notifications row for tx-email-1")
	}
	if runID == "" {
		t.Error("notification RunID is empty, want non-empty")
	}
}

// TestIngest_TopicFanout proves a multicast trigger (To{TopicKey}) fans out to
// every topic member: each subscriber gets its own engine run + notification
// row (with a non-empty run_id), all sharing one transactionId.
func TestIngest_TopicFanout(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	dbPath := filepath.Join(t.TempDir(), "trigger.db")
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	steps, err := json.Marshal([]domain.Step{{
		StepID:        "S1",
		Type:          "in_app",
		ControlValues: json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"You have {{payload.itemCount}} items"}`),
	}})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	if _, err := st.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Broadcast",
		TriggerIdentifier: testTrigger,
		Active:            true,
		Steps:             steps,
	}); err != nil {
		t.Fatalf("Workflows().Create: %v", err)
	}

	dagName := scope.EnvID + "-" + testTrigger
	if err := eng.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  dagName,
		Steps: []engine.StepDef{{ID: "S1", Task: "send.in_app", Type: "normal"}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	aud := audience.New(st)
	for _, s := range []audience.SubscriberInput{
		{SubscriberID: "s1", FirstName: "Ada"},
		{SubscriberID: "s2", FirstName: "Bob"},
	} {
		if _, err := aud.Upsert(ctx, scope, s); err != nil {
			t.Fatalf("upsert %s: %v", s.SubscriberID, err)
		}
	}

	if _, err := st.Topics().Create(ctx, scope, "t1", "Team"); err != nil {
		t.Fatalf("Topics().Create: %v", err)
	}
	for _, id := range []string{"s1", "s2"} {
		if err := st.Topics().AddSubscriber(ctx, scope, "t1", id); err != nil {
			t.Fatalf("AddSubscriber %s: %v", id, err)
		}
	}

	svc := trigger.New(aud, st, eng)
	resp, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
		Name:          testTrigger,
		To:            trigger.ToField{TopicKey: "t1"},
		Payload:       json.RawMessage(`{"itemCount":3}`),
		TransactionID: "tx-topic-1",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !resp.Acknowledged || resp.Status != "processed" {
		t.Fatalf("resp = %+v, want acknowledged/processed", resp)
	}
	if resp.TransactionID != "tx-topic-1" {
		t.Errorf("TransactionID = %q, want tx-topic-1", resp.TransactionID)
	}

	count, withRun := countNotifications(t, dbPath, "tx-topic-1")
	if count != 2 {
		t.Fatalf("notifications for tx-topic-1 = %d, want 2 (one per topic member)", count)
	}
	if withRun != 2 {
		t.Fatalf("notifications with non-empty run_id = %d, want 2", withRun)
	}
}

// TestIngest_PushDataJSONCorrupted_ReturnsError proves that a push workflow
// whose control_values contain a corrupted (non-parseable) data_json string
// causes Ingest to surface an error rather than silently sending a nil payload.
func TestIngest_PushDataJSONCorrupted_ReturnsError(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	dbPath := filepath.Join(t.TempDir(), "trigger.db")
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	const pushTrigger = "push-corrupt-flow"

	// data_json is stored as a JSON string in controlValues; this value is
	// deliberately malformed so json.Unmarshal into map[string]string fails.
	steps, err := json.Marshal([]domain.Step{{
		StepID: "P1",
		Type:   "push",
		ControlValues: json.RawMessage(
			`{"title":"Alert","body":"Body","data_json":"not valid json {"}`,
		),
	}})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	if _, err := st.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "PushCorrupt",
		TriggerIdentifier: pushTrigger,
		Active:            true,
		Steps:             steps,
	}); err != nil {
		t.Fatalf("Workflows().Create: %v", err)
	}

	dagName := scope.EnvID + "-" + pushTrigger
	if err := eng.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  dagName,
		Steps: []engine.StepDef{{ID: "P1", Task: "send.push", Type: "normal"}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	svc := trigger.New(audience.New(st), st, eng)
	_, ingestErr := svc.Ingest(ctx, scope, trigger.TriggerRequest{
		Name:          pushTrigger,
		To:            trigger.ToField{SubscriberID: "user_push", FirstName: "Ada"},
		Payload:       json.RawMessage(`{}`),
		TransactionID: "tx-push-corrupt-1",
	})
	if ingestErr == nil {
		t.Fatal("Ingest: expected error for corrupted data_json, got nil")
	}
	if !strings.Contains(ingestErr.Error(), "data_json") {
		t.Errorf("error = %q, want mention of data_json", ingestErr.Error())
	}
}

// countNotifications returns how many notification rows exist for a transaction
// id and how many of those carry a non-empty run_id.
func countNotifications(t *testing.T, dbPath, transactionID string) (total, withRun int) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open verify db: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT run_id FROM notifications WHERE transaction_id = ?`, transactionID)
	if err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		total++
		if runID != "" {
			withRun++
		}
	}
	return total, withRun
}
