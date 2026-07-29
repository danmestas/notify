package steptype_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/steptype"
)

// A dynamic delay reads its wait from the trigger payload. Historically the
// dispatcher discriminated the value's SHAPE at run time -- an RFC3339 string
// meant "wait until that instant", an {amount,unit} object meant "wait that
// long" -- which meant the same authored step could behave either way depending
// on what a given trigger happened to send.
//
// That per-payload sniffing is what blocks compiling these steps to the engine's
// native sleep (see #258): compile time cannot know which shape will arrive. The
// step now DECLARES its kind up front via dynamicKind, so the shape is a
// property of the workflow rather than of each individual payload.
//
// These tests pin that contract at the authoring layer.

func delayCV(t *testing.T, form url.Values) map[string]any {
	t.Helper()
	raw, err := steptype.Default().Get("delay").ParseForm(form)
	if err != nil {
		t.Fatalf("ParseForm(%v): %v", form, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal control values: %v", err)
	}
	return out
}

// TestDynamicDelay_ParseFormPersistsKind proves the declared kind survives into
// the stored control values, which is the whole point: the workflow, not the
// payload, decides how the value is interpreted.
func TestDynamicDelay_ParseFormPersistsKind(t *testing.T) {
	for _, kind := range []string{"instant", "relative"} {
		cv := delayCV(t, url.Values{
			"type":        {"dynamic"},
			"dynamicKey":  {"payload.sendAt"},
			"dynamicKind": {kind},
		})
		if cv["dynamicKind"] != kind {
			t.Errorf("dynamicKind = %v, want %q", cv["dynamicKind"], kind)
		}
		if cv["dynamicKey"] != "payload.sendAt" {
			t.Errorf("dynamicKey = %v, want payload.sendAt", cv["dynamicKey"])
		}
	}
}

// TestDynamicDelay_ParseFormRejectsMissingKind keeps the ambiguous shape from
// being authorable at all. Accepting it and inferring later is exactly the
// behaviour being removed.
func TestDynamicDelay_ParseFormRejectsMissingKind(t *testing.T) {
	_, err := steptype.Default().Get("delay").ParseForm(url.Values{
		"type":       {"dynamic"},
		"dynamicKey": {"payload.sendAt"},
	})
	if err == nil {
		t.Fatal("a dynamic delay with no dynamicKind was accepted")
	}
	if !strings.Contains(err.Error(), "dynamicKind") {
		t.Errorf("error should name the missing field, got: %v", err)
	}
}

// TestDynamicDelay_ParseFormRejectsUnknownKind guards the enum: a typo must fail
// at authoring rather than silently falling back to a default interpretation.
func TestDynamicDelay_ParseFormRejectsUnknownKind(t *testing.T) {
	_, err := steptype.Default().Get("delay").ParseForm(url.Values{
		"type":        {"dynamic"},
		"dynamicKey":  {"payload.sendAt"},
		"dynamicKind": {"instantaneous"},
	})
	if err == nil {
		t.Fatal("an unknown dynamicKind was accepted")
	}
}

// TestDynamicDelay_ValidateRequiresKind pins the same rule on the validate path,
// which is what guards blobs arriving from the API rather than the editor form.
func TestDynamicDelay_ValidateRequiresKind(t *testing.T) {
	dl := steptype.Default().Get("delay")

	if err := dl.Validate(json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.sendAt"}`)); err == nil {
		t.Error("Validate accepted a dynamic delay with no dynamicKind")
	}
	if err := dl.Validate(json.RawMessage(`{"type":"dynamic","dynamicKey":"p.x","dynamicKind":"nonsense"}`)); err == nil {
		t.Error("Validate accepted an unknown dynamicKind")
	}
	for _, kind := range []string{"instant", "relative"} {
		cv := json.RawMessage(`{"type":"dynamic","dynamicKey":"p.x","dynamicKind":"` + kind + `"}`)
		if err := dl.Validate(cv); err != nil {
			t.Errorf("Validate rejected valid kind %q: %v", kind, err)
		}
	}
}

// TestDynamicDelay_OtherKindsUnaffected keeps the contract change scoped: a
// regular or scheduled delay must not acquire a dynamicKind requirement.
func TestDynamicDelay_OtherKindsUnaffected(t *testing.T) {
	dl := steptype.Default().Get("delay")

	if err := dl.Validate(json.RawMessage(`{"type":"regular","amount":5,"unit":"minutes"}`)); err != nil {
		t.Errorf("regular delay rejected: %v", err)
	}
	if err := dl.Validate(json.RawMessage(`{"amount":5,"unit":"minutes"}`)); err != nil {
		t.Errorf("legacy (type-absent) delay rejected: %v", err)
	}
	sched := `{"type":"scheduled","atTime":"09:00","weekDays":["MON"]}`
	if err := dl.Validate(json.RawMessage(sched)); err != nil {
		t.Errorf("scheduled delay rejected: %v", err)
	}
}
