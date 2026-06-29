package delivery

import (
	"encoding/json"
	"testing"
)

// TestDigestData_InjectsCountAndEvents proves the digest render context exposes
// BOTH {{digest.count}} and an iterable {{digest.events}}: each accumulated
// event payload is unmarshalled to a map so a Handlebars {{#each}} can read its
// fields.
func TestDigestData_InjectsCountAndEvents(t *testing.T) {
	events := []json.RawMessage{
		json.RawMessage(`{"n":1,"name":"a"}`),
		json.RawMessage(`{"n":2,"name":"b"}`),
		json.RawMessage(`{"n":3,"name":"c"}`),
	}
	got := digestData(events)
	if got["count"] != 3 {
		t.Fatalf("count = %v, want 3", got["count"])
	}
	items, ok := got["events"].([]map[string]any)
	if !ok {
		t.Fatalf("events type = %T, want []map[string]any", got["events"])
	}
	if len(items) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(items))
	}
	if items[0]["name"] != "a" || items[2]["n"].(float64) != 3 {
		t.Fatalf("events not faithfully decoded: %+v", items)
	}
}

// TestDigestData_DropsNonObjectEvents proves a non-object event (e.g. a bare
// array or scalar slipped into the accumulator) is dropped from events while
// count still reflects the raw accumulated total.
func TestDigestData_DropsNonObjectEvents(t *testing.T) {
	events := []json.RawMessage{
		json.RawMessage(`{"n":1}`),
		json.RawMessage(`[1,2,3]`),
		json.RawMessage(`"scalar"`),
		json.RawMessage(`{"n":2}`),
	}
	got := digestData(events)
	if got["count"] != 4 {
		t.Fatalf("count = %v, want 4 (raw total)", got["count"])
	}
	items := got["events"].([]map[string]any)
	if len(items) != 2 {
		t.Fatalf("len(events) = %d, want 2 (non-objects dropped)", len(items))
	}
}
