package aibuilder

import (
	"reflect"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

func TestSystemPrompt_IncludesGatedCatalogAndVocab(t *testing.T) {
	reg := steptype.Default()
	gated := AvailableStepTypes(reg, []domain.Integration{{Channel: "email", Active: true}})
	p := systemPrompt(reg.Catalog(), gated)

	// Gated-in channel + an action step + the built-in in_app inbox are documented.
	if !strings.Contains(p, "email (Email)") {
		t.Error("prompt missing email catalog entry")
	}
	if !strings.Contains(p, "delay (Delay)") {
		t.Error("prompt missing delay catalog entry")
	}
	if !strings.Contains(p, "in_app (In-App)") {
		t.Error("prompt missing built-in in_app catalog entry")
	}
	if !strings.Contains(p, "stream (Stream)") {
		t.Error("prompt missing built-in stream catalog entry")
	}
	// Gated-out (external-provider) channels must not be offered.
	for _, excluded := range []string{"sms (SMS)", "push (Push)", "chat (Chat)"} {
		if strings.Contains(p, excluded) {
			t.Errorf("prompt should not list gated-out %q", excluded)
		}
	}
	// All 16 operators appear.
	for _, op := range conditionOperators {
		if !strings.Contains(p, op) {
			t.Errorf("prompt missing operator %q", op)
		}
	}
	// Trigger-id rule and iterative instruction.
	if !strings.Contains(p, "slugif") {
		t.Error("prompt missing trigger-id slugify rule")
	}
	if !strings.Contains(p, "STOP calling tools") {
		t.Error("prompt missing final-summary / stop instruction")
	}
}

// TestSystemPrompt_ExplicitlyEnumeratesAvailableTypesAndForbidsHallucination is
// the C1 regression test. The copilot was observed (browser QA) claiming a
// step type "not available" that was in fact gated-in, then silently
// substituting a different channel — even though the gating plumbing
// (AvailableStepTypes → the tool-schema enum) is deterministic and correct.
// The fix hardens the prompt itself: an explicit, unambiguous enumeration of
// every available type plus a direct instruction never to claim a listed type
// is unavailable and never to silently substitute — ask instead.
func TestSystemPrompt_ExplicitlyEnumeratesAvailableTypesAndForbidsHallucination(t *testing.T) {
	reg := steptype.Default()
	gated := AvailableStepTypes(reg, []domain.Integration{
		{Channel: "email", Active: true},
		{Channel: "sms", Active: true},
	})
	p := systemPrompt(reg.Catalog(), gated)

	want := "Available step types in this environment: " + strings.Join(gated, ", ") + "."
	if !strings.Contains(p, want) {
		t.Errorf("prompt missing explicit available-types enumeration; want substring %q; got: %s", want, p)
	}
	if !strings.Contains(p, "never tell the user a listed type is unavailable") {
		t.Error("prompt missing the anti-hallucination instruction")
	}
	if !strings.Contains(p, "never silently substitute") {
		t.Error("prompt missing the anti-silent-substitution instruction")
	}
}

// TestRefineSystemPrompt_ExplicitlyEnumeratesAvailableTypesAndForbidsHallucination
// mirrors the create-flow C1 regression test for the refine (edit) system
// prompt, which carries the same catalog/gating section.
func TestRefineSystemPrompt_ExplicitlyEnumeratesAvailableTypesAndForbidsHallucination(t *testing.T) {
	reg := steptype.Default()
	gated := AvailableStepTypes(reg, []domain.Integration{{Channel: "email", Active: true}})
	existing := &WorkflowDraft{Name: "Flow", TriggerIdentifier: "flow"}
	p := refineSystemPrompt(reg.Catalog(), gated, existing)

	want := "Available step types in this environment: " + strings.Join(gated, ", ") + "."
	if !strings.Contains(p, want) {
		t.Errorf("refine prompt missing explicit available-types enumeration; want substring %q; got: %s", want, p)
	}
	if !strings.Contains(p, "never tell the user a listed type is unavailable") {
		t.Error("refine prompt missing the anti-hallucination instruction")
	}
	if !strings.Contains(p, "never silently substitute") {
		t.Error("refine prompt missing the anti-silent-substitution instruction")
	}
}

// TestAvailableStepTypes_Deterministic proves the gated-type computation the
// prompt and tool schema are built from carries no hidden nondeterminism
// (e.g. unsorted map iteration) across repeated calls with the same inputs —
// ruling out (a) from the C1 investigation (a racy/inconsistent available-set
// per turn) so the observed hallucination is a prompt-robustness problem, not
// a data problem.
func TestAvailableStepTypes_Deterministic(t *testing.T) {
	reg := steptype.Default()
	ints := []domain.Integration{
		{Channel: "email", Active: true},
		{Channel: "sms", Active: false},
		{Channel: "push", Active: true},
	}
	first := AvailableStepTypes(reg, ints)
	for i := 0; i < 20; i++ {
		got := AvailableStepTypes(reg, ints)
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("AvailableStepTypes is non-deterministic across calls: %v vs %v", first, got)
		}
	}
}

func TestSystemPrompt_PerTypeControlFields(t *testing.T) {
	reg := steptype.Default()
	gated := []string{"email", "delay", "http_request"}
	p := systemPrompt(reg.Catalog(), gated)
	if !strings.Contains(p, "subject") || !strings.Contains(p, "body") {
		t.Error("email control fields missing")
	}
	if !strings.Contains(p, "weeks|months") {
		t.Error("delay unit options missing")
	}
	if !strings.Contains(p, "failurePolicy") {
		t.Error("http_request control fields missing")
	}
}
