package trigger_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// fakeDigester is an in-memory Digester: it records appended events per key and
// grants the claim to the FIRST caller of each distinct key (subsequent callers
// merge). It lets the digestKey grouping tests assert which triggers start a run
// (owners persist a notification) without a live NATS-KV bucket.
type fakeDigester struct {
	mu      sync.Mutex
	appends map[string]int
	claimed map[string]bool
}

func newFakeDigester() *fakeDigester {
	return &fakeDigester{appends: map[string]int{}, claimed: map[string]bool{}}
}

func (f *fakeDigester) DigestAppend(key string, _ json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appends[key]++
	return nil
}

func (f *fakeDigester) DigestClaim(key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed[key] {
		return false, nil
	}
	f.claimed[key] = true
	return true, nil
}

// setupDigest seeds a single-step digest workflow whose controlValues are cv,
// registers the matching sleep DAG, and returns the trigger service wired with
// a fakeDigester plus the db path for notification inspection.
func setupDigest(t *testing.T, cv string) (*trigger.Service, domain.Scope, string, *fakeDigester) {
	t.Helper()
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	dbPath := filepath.Join(t.TempDir(), "digest.db")
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	steps, err := json.Marshal([]domain.Step{{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(cv)}})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	if _, err := st.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Digest",
		TriggerIdentifier: "digest-flow",
		Active:            true,
		Steps:             steps,
	}); err != nil {
		t.Fatalf("Workflows().Create: %v", err)
	}
	if err := eng.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  scope.EnvID + "-digest-flow",
		Steps: []engine.StepDef{{ID: "dg", Type: "sleep", Config: json.RawMessage(`{"duration":1000000000}`)}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	fake := newFakeDigester()
	return trigger.New(audience.New(st), st, eng).WithDigester(fake), scope, dbPath, fake
}

// digestKeys returns the digest_key column of every persisted notification.
func digestKeys(t *testing.T, dbPath string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open verify db: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT digest_key FROM notifications ORDER BY id`)
	if err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		keys = append(keys, k)
	}
	return keys
}

func triggerOrder(t *testing.T, svc *trigger.Service, scope domain.Scope, orderID, tx string) {
	t.Helper()
	payload := json.RawMessage(`{}`)
	if orderID != "" {
		payload = json.RawMessage(`{"orderId":"` + orderID + `"}`)
	}
	if _, err := svc.Ingest(context.Background(), scope, trigger.TriggerRequest{
		Name:          "digest-flow",
		To:            trigger.ToField{SubscriberID: "dk_user"},
		Payload:       payload,
		TransactionID: tx,
	}); err != nil {
		t.Fatalf("Ingest %s: %v", tx, err)
	}
}

// TestIngest_DigestKey_GroupsByPayloadField proves two triggers with DISTINCT
// payload.orderId values start two separate runs (distinct claim keys) and
// persist two notifications with distinct digest_key column values.
func TestIngest_DigestKey_GroupsByPayloadField(t *testing.T) {
	svc, scope, dbPath, _ := setupDigest(t, `{"type":"regular","amount":1,"unit":"seconds","digestKey":"payload.orderId"}`)
	triggerOrder(t, svc, scope, "A", "g1")
	triggerOrder(t, svc, scope, "B", "g2")

	keys := digestKeys(t, dbPath)
	if len(keys) != 2 {
		t.Fatalf("got %d notifications, want 2 (distinct orderIds start distinct runs): %v", len(keys), keys)
	}
	if keys[0] == keys[1] {
		t.Fatalf("digest_key values collided: %v (distinct orderIds must produce distinct keys)", keys)
	}
}

// TestIngest_DigestKey_SameValue_Merges proves three triggers with the SAME
// payload.orderId value start exactly one run (first claims, two merge).
func TestIngest_DigestKey_SameValue_Merges(t *testing.T) {
	svc, scope, dbPath, fake := setupDigest(t, `{"type":"regular","amount":1,"unit":"seconds","digestKey":"payload.orderId"}`)
	triggerOrder(t, svc, scope, "X", "m1")
	triggerOrder(t, svc, scope, "X", "m2")
	triggerOrder(t, svc, scope, "X", "m3")

	keys := digestKeys(t, dbPath)
	if len(keys) != 1 {
		t.Fatalf("got %d notifications, want 1 (identical orderId merges): %v", len(keys), keys)
	}
	// All three events accumulated under the one key.
	if got := fake.appends[keys[0]]; got != 3 {
		t.Fatalf("appended %d events under %q, want 3", got, keys[0])
	}
}

// TestIngest_DigestKey_AbsentField_FallsBackToSubscriberScope proves a
// configured digestKey whose dot-path is absent from the payload resolves to ""
// and falls back to the per-subscriber base key (so such events merge, exactly
// like a workflow with no digestKey).
func TestIngest_DigestKey_AbsentField_FallsBackToSubscriberScope(t *testing.T) {
	svc, scope, dbPath, _ := setupDigest(t, `{"type":"regular","amount":1,"unit":"seconds","digestKey":"payload.missing"}`)
	triggerOrder(t, svc, scope, "", "a1")
	triggerOrder(t, svc, scope, "", "a2")

	keys := digestKeys(t, dbPath)
	if len(keys) != 1 {
		t.Fatalf("got %d notifications, want 1 (absent field falls back to base key): %v", len(keys), keys)
	}
	// Base key is exactly envID:subID:wfID — three colon-separated segments,
	// NO encoded 4th segment (which a spurious trailing ":" would have added).
	k := keys[0]
	if prefix := scope.EnvID + ":dk_user:"; !strings.HasPrefix(k, prefix) {
		t.Fatalf("digest_key = %q, want prefix %q", k, prefix)
	}
	if n := strings.Count(k, ":"); n != 2 {
		t.Fatalf("digest_key = %q has %d ':' separators, want 2 (base key, no 4th segment)", k, n)
	}
}
