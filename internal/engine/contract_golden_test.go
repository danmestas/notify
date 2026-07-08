package engine_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/engine"
)

// contractFixture bundles the notify-side wire types that are hand-mirrored
// copies of dagnats's dag.WorkflowDef / dag.StepDef / dag.RetryPolicy /
// dag.SingletonConfig / protocol.TaskPayload (see the doc comments on
// engine.go's WorkflowDef, StepDef, RetryPolicy, TaskPayload). Nothing in the
// Go type system enforces that these stay byte-identical to dagnats's wire
// format — only this test does, by pinning the exact JSON shape against a
// checked-in fixture.
type contractFixture struct {
	Workflow    engine.WorkflowDef `json:"workflow"`
	TaskPayload engine.TaskPayload `json:"task_payload"`
}

// fixtureValue is the single source of truth for both testdata/contract.json
// (regenerated with `go run` when the wire format changes deliberately) and
// this test's expectations.
func fixtureValue() contractFixture {
	return contractFixture{
		Workflow: engine.WorkflowDef{
			Name: "contract-check",
			Steps: []engine.StepDef{
				{
					ID:     "s1",
					Task:   "send.email",
					Type:   "normal",
					Config: json.RawMessage(`{"to":"a@b.com"}`),
					Retry: &engine.RetryPolicy{
						MaxAttempts:  3,
						Strategy:     "exponential",
						InitialDelay: time.Second,
						MaxDelay:     30 * time.Second,
					},
					Timeout: 5 * time.Second,
				},
				{
					ID:        "s2",
					Task:      "send.push",
					Type:      "normal",
					DependsOn: []string{"s1"},
				},
			},
			IdempotencyKey: "input.transactionId",
			Singleton: &engine.SingletonConfig{
				Mode: "skip",
				Key:  "input.subscriberId",
			},
		},
		TaskPayload: engine.TaskPayload{
			TaskID:    "t1",
			RunID:     "r1",
			StepID:    "s1",
			Iteration: 2,
			Attempt:   1,
			Input:     json.RawMessage(`{"foo":"bar"}`),
		},
	}
}

// TestContract_GoldenRoundTrip pins engine.WorkflowDef / StepDef / RetryPolicy
// / SingletonConfig / TaskPayload's JSON wire shape against
// testdata/contract.json. A failure here means either the fixture is
// deliberately stale (dagnats's wire format changed — update the fixture) or
// someone edited a json tag, field name, or omitempty on the notify side and
// silently drifted from dagnats's actual protocol.
func TestContract_GoldenRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/contract.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var fx contractFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	remarshaled, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal fixture: %v", err)
	}
	remarshaled = append(remarshaled, '\n')

	if string(remarshaled) != string(raw) {
		t.Errorf("round-trip JSON drifted from testdata/contract.json:\n--- fixture ---\n%s\n--- got ---\n%s", raw, remarshaled)
	}

	// Also assert against the Go-side fixtureValue directly, so a change to
	// fixtureValue (not just the on-disk file) is caught too.
	want, err := json.MarshalIndent(fixtureValue(), "", "  ")
	if err != nil {
		t.Fatalf("marshal fixtureValue: %v", err)
	}
	want = append(want, '\n')
	if string(want) != string(raw) {
		t.Errorf("testdata/contract.json is stale relative to fixtureValue():\n--- fixture ---\n%s\n--- fixtureValue() ---\n%s", raw, want)
	}
}
