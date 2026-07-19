package engine

import (
	"encoding/json"
	"testing"
)

// TestTaskPayload_UnmarshalsDagnatsTraceKeys pins the wire contract with
// dagnats's bridge poll response (dagnats#534).
//
// The JSON below is the shape dagnats emits: W3C trace context under the
// un-underscored wire names "traceparent"/"tracestate", deliberately distinct
// from the underscored trace_parent/trace_state it persists on events. If our
// struct tags drift from those exact names, the fields silently stay empty,
// every dispatched task roots a fresh trace, and nothing fails — the same
// invisible failure mode a canonicalised header key caused on the NATS seam
// (#250). Hence a literal-JSON test rather than a round-trip through our own
// marshaller, which would agree with itself no matter what the tags said.
func TestTaskPayload_UnmarshalsDagnatsTraceKeys(t *testing.T) {
	const dagnatsPollTask = `{
		"task_id": "task-1",
		"run_id": "run-1",
		"step_id": "send.in_app",
		"attempt": 1,
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"tracestate": "vendor=opaque"
	}`

	var got TaskPayload
	if err := json.Unmarshal([]byte(dagnatsPollTask), &got); err != nil {
		t.Fatalf("unmarshal dagnats poll task: %v", err)
	}

	if got.TraceParent != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Fatalf("TraceParent = %q; the \"traceparent\" key did not bind — dispatched tasks would orphan their traces", got.TraceParent)
	}
	if got.TraceState != "vendor=opaque" {
		t.Fatalf("TraceState = %q, want %q", got.TraceState, "vendor=opaque")
	}
	// Sanity: the pre-existing fields still bind, i.e. the tags above did not
	// disturb the rest of the contract.
	if got.TaskID != "task-1" || got.RunID != "run-1" || got.StepID != "send.in_app" || got.Attempt != 1 {
		t.Fatalf("existing fields broke: %+v", got)
	}
}

// TestTaskPayload_OmitsTraceKeysWhenEmpty keeps notify from inventing empty
// trace fields on anything it marshals back, so an absent context stays absent
// rather than becoming a present-but-blank traceparent.
func TestTaskPayload_OmitsTraceKeysWhenEmpty(t *testing.T) {
	b, err := json.Marshal(TaskPayload{TaskID: "t", RunID: "r", StepID: "s", Attempt: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"traceparent", "tracestate"} {
		if _, present := m[k]; present {
			t.Fatalf("empty %q should be omitted, got %s", k, b)
		}
	}
}
