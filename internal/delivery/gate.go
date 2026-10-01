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
// array is OR'd). On a malformed filters payload or an evaluation
// error we FAIL CLOSED: the step is SKIPPED, never sent. A broken condition must
// never silently blast a send to every subscriber, so the safe default for a
// condition we cannot evaluate is "do not send" rather than "send to everyone".
//
// stepOutputs adds each upstream action step's captured output (e.g. an
// http_request response) as a named source keyed by its step id, so a condition
// can test On='<httpRequestStepId>' Field='statusCode'. A nil/empty map simply
// contributes no extra sources (the 99% no-action-step path is unchanged).
func conditionMet(filters json.RawMessage, subMap, payloadMap map[string]any, stepOutputs map[string]map[string]any) bool {
	met, _ := conditionOutcome(filters, subMap, payloadMap, stepOutputs)
	return met
}

// conditionOutcome is conditionMet plus WHY it said no: invalid reports that the
// filter could not be evaluated, as distinct from evaluating honestly to false.
//
// Both skip the send -- the gate fails closed on purpose, so a broken condition
// can never blast a send to every subscriber -- but they mean opposite things to
// an operator. A false condition is the workflow working; an unevaluable one is
// a bug that has been silently suppressing sends.
func conditionOutcome(filters json.RawMessage, subMap, payloadMap map[string]any, stepOutputs map[string]map[string]any) (met, invalid bool) {
	if len(bytes.TrimSpace(filters)) == 0 {
		// Absent, empty, or whitespace-only => no condition => always send.
		return true, false
	}
	var groups []filter.Group
	if err := json.Unmarshal(filters, &groups); err != nil {
		// Malformed condition: fail closed (skip), do not send.
		return false, true
	}
	if len(groups) == 0 {
		// Explicit empty array is equivalent to no condition.
		return true, false
	}
	sources := map[string]map[string]any{
		"subscriber": subMap,
		"payload":    payloadMap,
	}
	for id, out := range stepOutputs {
		// Step ids are authored separately and never collide with the reserved
		// "subscriber"/"payload" source names, so a plain assignment is correct.
		sources[id] = out
	}
	ok, err := filter.Match(groups, filter.EvalContext{Sources: sources})
	if err != nil {
		// Evaluation error: fail closed (skip), do not send. This is the same
		// "could not evaluate" class as a malformed condition, not an honest
		// false, so it reports invalid.
		return false, true
	}
	return ok, false
}
