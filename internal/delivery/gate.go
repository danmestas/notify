package delivery

import (
	"bytes"
	"encoding/json"

	"github.com/danmestas/notify/internal/delivery/filter"
)

// conditionMet reports whether a step's per-step filter condition permits the
// send for this {subscriber, payload} context.
//
// Absent/empty filters => no condition => always send (the 99% path is
// unchanged: callers with no Filters proceed exactly as before).
//
// When filters are present they are a JSON array of filter.Group (the top-level
// array is OR'd, per the reference platform). On a malformed filters payload or an evaluation
// error we FAIL CLOSED: the step is SKIPPED, never sent. A broken condition must
// never silently blast a send to every subscriber, so the safe default for a
// condition we cannot evaluate is "do not send" rather than "send to everyone".
func conditionMet(filters json.RawMessage, subMap, payloadMap map[string]any) bool {
	if len(bytes.TrimSpace(filters)) == 0 {
		// Absent, empty, or whitespace-only => no condition => always send.
		return true
	}
	var groups []filter.Group
	if err := json.Unmarshal(filters, &groups); err != nil {
		// Malformed condition: fail closed (skip), do not send.
		return false
	}
	if len(groups) == 0 {
		// Explicit empty array is equivalent to no condition.
		return true
	}
	ok, err := filter.Match(groups, filter.EvalContext{Sources: map[string]map[string]any{
		"subscriber": subMap,
		"payload":    payloadMap,
	}})
	if err != nil {
		// Evaluation error: fail closed (skip), do not send.
		return false
	}
	return ok
}
