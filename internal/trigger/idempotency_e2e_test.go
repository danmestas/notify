package trigger_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

const idempotencyTestTrigger = "idempotent-flow"

// setupIdempotency spins a real dagnats sidecar + engine and a temp SQLite
// store, then creates and publishes a 1-step in_app workflow through
// authoring.Service (Create -> Compile -> Publish/RegisterWorkflow) so the
// registered DAG carries the real IdempotencyKey dot-path ("idempotencyKey")
// that authoring.Compile sets — exactly the code path a live server
// exercises, unlike setup() in trigger_test.go which hand-builds an
// engine.WorkflowDef with no IdempotencyKey.
func setupIdempotency(t *testing.T) (*trigger.Service, domain.Scope, string) {
	t.Helper()

	natsURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	dbPath := filepath.Join(t.TempDir(), "idempotency.db")
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	authSvc := authoring.New(st, eng)
	if _, err := authSvc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Idempotent",
		TriggerIdentifier: idempotencyTestTrigger,
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "in_app",
			ControlValues: json.RawMessage(`{"subject":"Hi","body":"hello"}`),
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := authSvc.Publish(ctx, scope, idempotencyTestTrigger); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	return trigger.New(audience.New(st), st, eng), scope, dbPath
}

// runIDsForTransaction returns the run_id column of every notification row
// persisted for transactionID, ordered by creation so callers can inspect
// per-Ingest-call values.
func runIDsForTransaction(t *testing.T, dbPath, transactionID string) []string {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open verify db: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(
		`SELECT run_id FROM notifications WHERE transaction_id = ? ORDER BY created_at ASC`,
		transactionID,
	)
	if err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	defer rows.Close()
	var runIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan run_id: %v", err)
		}
		runIDs = append(runIDs, id)
	}
	return runIDs
}

// TestIngest_SameTransactionID_DedupesToOneRun proves the fix end-to-end
// against a real dagnats sidecar: two Ingest calls carrying the SAME
// TransactionID AND the same recipient each persist their own notification
// row (Ingest's own bookkeeping is unconditional), but dagnats's StartRun
// admission check — keyed on the composite "idempotencyKey" field
// authoring.Compile now targets — returns the SAME run id for both, instead
// of starting a duplicate run.
func TestIngest_SameTransactionID_DedupesToOneRun(t *testing.T) {
	svc, scope, dbPath := setupIdempotency(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
			Name:          idempotencyTestTrigger,
			To:            trigger.ToField{SubscriberID: "user_dedup"},
			Payload:       json.RawMessage(`{}`),
			TransactionID: "tx-dedup",
		}); err != nil {
			t.Fatalf("Ingest[%d]: %v", i, err)
		}
	}

	runIDs := runIDsForTransaction(t, dbPath, "tx-dedup")
	if len(runIDs) != 2 {
		t.Fatalf("got %d notification rows for tx-dedup, want 2 (each Ingest call persists its own row): %v", len(runIDs), runIDs)
	}
	if runIDs[0] == "" || runIDs[1] == "" {
		t.Fatalf("run ids = %v, want both non-empty", runIDs)
	}
	if runIDs[0] != runIDs[1] {
		t.Fatalf("run ids = %v, want equal (retried trigger must dedupe to one dagnats run)", runIDs)
	}
}

// TestIngest_DistinctTransactionID_SeparateRuns proves the negative case:
// two Ingest calls with DISTINCT TransactionID values (including the
// auto-minted ULID trigger.go generates when a client omits one) start
// DISTINCT dagnats runs, so the fix does not over-merge unrelated triggers.
func TestIngest_DistinctTransactionID_SeparateRuns(t *testing.T) {
	svc, scope, dbPath := setupIdempotency(t)
	ctx := context.Background()

	if _, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
		Name:          idempotencyTestTrigger,
		To:            trigger.ToField{SubscriberID: "user_a"},
		Payload:       json.RawMessage(`{}`),
		TransactionID: "tx-a",
	}); err != nil {
		t.Fatalf("Ingest tx-a: %v", err)
	}
	// Omit TransactionID: trigger.go mints a fresh ULID, which must be
	// distinct from "tx-a" and therefore start its own run.
	respB, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
		Name:    idempotencyTestTrigger,
		To:      trigger.ToField{SubscriberID: "user_b"},
		Payload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Ingest (auto-minted): %v", err)
	}
	if respB.TransactionID == "" || respB.TransactionID == "tx-a" {
		t.Fatalf("auto-minted TransactionID = %q, want a fresh non-empty id distinct from tx-a", respB.TransactionID)
	}

	runIDsA := runIDsForTransaction(t, dbPath, "tx-a")
	runIDsB := runIDsForTransaction(t, dbPath, respB.TransactionID)
	if len(runIDsA) != 1 || len(runIDsB) != 1 {
		t.Fatalf("got %d row(s) for tx-a and %d for auto-minted id, want 1 each", len(runIDsA), len(runIDsB))
	}
	if runIDsA[0] == "" || runIDsB[0] == "" {
		t.Fatalf("run ids = %v / %v, want both non-empty", runIDsA, runIDsB)
	}
	if runIDsA[0] == runIDsB[0] {
		t.Fatalf("run ids = %v / %v, want distinct (unrelated triggers must not merge)", runIDsA, runIDsB)
	}
}

// TestIngest_SharedTransactionID_DistinctSubscribers_SeparateRuns is the
// multicast-crux regression: it holds TransactionID FIXED and varies only
// SubscriberID — exactly what a to.topicKey fan-out does internally, where
// every recipient of one multicast shares the single minted TransactionID.
// The two recipients MUST land on two DISTINCT dagnats runs. This is the case
// that fails if the DAG's IdempotencyKey dot-path is the bare "transactionId"
// (both recipients collapse onto one run, dropping delivery to everyone after
// the first); it passes only because the key is the composite
// (TransactionID, SubscriberID) "idempotencyKey" field. The two tests above
// vary BOTH fields together and so cannot catch a revert to the bare key —
// this one can.
func TestIngest_SharedTransactionID_DistinctSubscribers_SeparateRuns(t *testing.T) {
	svc, scope, dbPath := setupIdempotency(t)
	ctx := context.Background()

	const sharedTx = "tx-multicast"
	for _, subID := range []string{"mc_user_1", "mc_user_2"} {
		if _, err := svc.Ingest(ctx, scope, trigger.TriggerRequest{
			Name:          idempotencyTestTrigger,
			To:            trigger.ToField{SubscriberID: subID},
			Payload:       json.RawMessage(`{}`),
			TransactionID: sharedTx,
		}); err != nil {
			t.Fatalf("Ingest %s: %v", subID, err)
		}
	}

	runIDs := runIDsForTransaction(t, dbPath, sharedTx)
	if len(runIDs) != 2 {
		t.Fatalf("got %d notification rows for %s, want 2 (one per distinct recipient): %v", len(runIDs), sharedTx, runIDs)
	}
	if runIDs[0] == "" || runIDs[1] == "" {
		t.Fatalf("run ids = %v, want both non-empty", runIDs)
	}
	if runIDs[0] == runIDs[1] {
		t.Fatalf("run ids = %v, want DISTINCT (distinct recipients of one multicast must not collapse onto a single dagnats run)", runIDs)
	}
}
