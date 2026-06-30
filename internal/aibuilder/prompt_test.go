package aibuilder

import (
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

func TestSystemPrompt_IncludesGatedCatalogAndVocab(t *testing.T) {
	reg := steptype.Default()
	gated := AvailableStepTypes(reg, []domain.Integration{{Channel: "email", Active: true}})
	p := systemPrompt(reg.Catalog(), gated)

	// Gated-in channel + an action step are documented.
	if !strings.Contains(p, "email (Email)") {
		t.Error("prompt missing email catalog entry")
	}
	if !strings.Contains(p, "delay (Delay)") {
		t.Error("prompt missing delay catalog entry")
	}
	// Gated-out channels must not be offered.
	for _, excluded := range []string{"sms (SMS)", "push (Push)", "in_app (In-App)", "chat (Chat)"} {
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
