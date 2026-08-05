package delivery

import (
	"encoding/json"
	"testing"
)

// A send can be skipped by a step's filter condition or by a subscriber's
// channel preference, and both wrote a bare status:"skipped". An operator could
// not tell whether their workflow was misconfigured (go fix the condition) or
// the system had correctly honoured a subscriber's explicit choice (nothing to
// fix). Those call for opposite responses.
//
// The condition gate also fails CLOSED -- an unevaluable filter skips rather
// than sending to everyone -- which is the right default but made a genuinely
// broken condition indistinguishable from an honest false. That is the case
// most likely to be a real bug, so it is reported separately.

// TestConditionOutcome_DistinguishesFalseFromUnevaluable is the core of it.
func TestConditionOutcome_DistinguishesFalseFromUnevaluable(t *testing.T) {
	sub := map[string]any{"plan": "free"}
	payload := map[string]any{}

	// Honest false: a well-formed condition that simply does not match.
	honest := json.RawMessage(`[{"combinator":"and","conditions":[{"on":"subscriber","field":"plan","operator":"eq","value":"pro"}]}]`)
	met, invalid := conditionOutcome(honest, sub, payload, nil)
	if met {
		t.Error("a non-matching condition reported met")
	}
	if invalid {
		t.Error("a well-formed non-matching condition was reported unevaluable")
	}

	// Malformed: cannot be parsed at all.
	broken := json.RawMessage(`{not json at all`)
	met, invalid = conditionOutcome(broken, sub, payload, nil)
	if met {
		t.Error("a malformed condition reported met -- the gate must fail closed")
	}
	if !invalid {
		t.Error("a malformed condition was not reported unevaluable, so it is indistinguishable from an honest false")
	}
}

// TestConditionOutcome_AbsentFilterSends keeps the common case intact: no
// condition means always send, and that is not a skip of any kind.
func TestConditionOutcome_AbsentFilterSends(t *testing.T) {
	for _, filters := range []json.RawMessage{nil, json.RawMessage(``), json.RawMessage(`   `), json.RawMessage(`[]`)} {
		met, invalid := conditionOutcome(filters, map[string]any{}, map[string]any{}, nil)
		if !met {
			t.Errorf("absent/empty filter %q blocked the send", string(filters))
		}
		if invalid {
			t.Errorf("absent/empty filter %q reported unevaluable", string(filters))
		}
	}
}

// TestConditionMet_StillAgreesWithOutcome guards the boolean wrapper other call
// sites still use: it must not drift from the richer function it delegates to.
func TestConditionMet_StillAgreesWithOutcome(t *testing.T) {
	cases := []json.RawMessage{
		nil,
		json.RawMessage(`[]`),
		json.RawMessage(`{malformed`),
		json.RawMessage(`[{"combinator":"and","conditions":[{"on":"subscriber","field":"plan","operator":"eq","value":"pro"}]}]`),
	}
	sub := map[string]any{"plan": "pro"}
	for _, f := range cases {
		want, _ := conditionOutcome(f, sub, map[string]any{}, nil)
		if got := conditionMet(f, sub, map[string]any{}, nil); got != want {
			t.Errorf("conditionMet(%q) = %v, conditionOutcome said %v", string(f), got, want)
		}
	}
}

// TestSkipReasons_AreDistinct pins that the three reasons are actually different
// values -- a copy-paste collapsing two of them would silently restore the
// ambiguity this whole change removes.
func TestSkipReasons_AreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range []string{SkipReasonCondition, SkipReasonConditionInvalid, SkipReasonPreference} {
		if r == "" {
			t.Error("a skip reason is empty, which reads as 'not skipped'")
		}
		if seen[r] {
			t.Errorf("duplicate skip reason %q: two different gates would be indistinguishable", r)
		}
		seen[r] = true
	}
}
