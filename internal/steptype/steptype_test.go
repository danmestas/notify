package steptype_test

import (
	"encoding/json"
	"net/url"
	"reflect"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// TestNames_CanonicalOrder pins the single source of truth that the console's
// offered set and the httpapi's allowed-types list both derive from. The order
// must equal the two hand-kept lists this registry replaced, or those surfaces
// (add-step buttons, "allowed: ..." error text) would shift.
// Updated: throttle appended after digest — intentional, matches PR adding throttle step type.
// Updated: http_request appended after throttle — intentional, matches PR adding the
// http_request action step type. Appending (not inserting) keeps every prior
// surface's ordering stable; only the new type joins the tail of the list.
func TestNames_CanonicalOrder(t *testing.T) {
	got := steptype.Default().Names()
	want := []string{"in_app", "email", "push", "chat", "sms", "delay", "digest", "throttle", "http_request"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

func TestGetAndHas(t *testing.T) {
	r := steptype.Default()
	if !r.Has("delay") {
		t.Error("Has(delay) = false, want true")
	}
	if r.Has("webhook") {
		t.Error("Has(webhook) = true, want false")
	}
	if r.Get("webhook") != nil {
		t.Error("Get(webhook) != nil, want nil for unregistered type")
	}
	if got := r.Get("in_app").Name(); got != "in_app" {
		t.Errorf("Get(in_app).Name() = %q, want in_app", got)
	}
}

// TestDelayDigestCompileIdentical documents that delay and digest produce the
// identical engine sleep StepDef today while owning SEPARATE Compile funcs —
// the structural split that lets them diverge later without contaminating each
// other.
func TestDelayDigestCompileIdentical(t *testing.T) {
	r := steptype.Default()
	cv := json.RawMessage(`{"amount":3,"unit":"minutes"}`)
	delayDef, err := r.Get("delay").Compile(domain.Step{StepID: "s", Type: "delay", ControlValues: cv})
	if err != nil {
		t.Fatalf("delay compile: %v", err)
	}
	digestDef, err := r.Get("digest").Compile(domain.Step{StepID: "s", Type: "digest", ControlValues: cv})
	if err != nil {
		t.Fatalf("digest compile: %v", err)
	}
	if delayDef.Type != "sleep" || string(delayDef.Config) != `{"duration":180000000000}` {
		t.Fatalf("delay def = %+v", delayDef)
	}
	if !reflect.DeepEqual(delayDef, digestDef) {
		t.Fatalf("delay/digest StepDef diverged: %+v vs %+v", delayDef, digestDef)
	}
}

func TestParseFormAndDefaults(t *testing.T) {
	r := steptype.Default()
	form := url.Values{"subject": {"  Hi  "}, "body": {"there"}}
	got, err := r.Get("email").ParseForm(form)
	if err != nil {
		t.Fatalf("email ParseForm: %v", err)
	}
	if string(got) != `{"body":"there","subject":"Hi"}` {
		t.Fatalf("email ParseForm = %s", got)
	}
	if string(r.Get("delay").DefaultControlValues()) != `{"amount":1,"unit":"minutes"}` {
		t.Fatalf("delay default = %s", r.Get("delay").DefaultControlValues())
	}
	if string(r.Get("email").DefaultControlValues()) != `{}` {
		t.Fatalf("email default = %s", r.Get("email").DefaultControlValues())
	}
}

// TestThrottle_DefaultControlValues verifies the seeded config is valid JSON
// with positive limit and windowSec fields that Compile accepts.
func TestThrottle_DefaultControlValues(t *testing.T) {
	r := steptype.Default()
	raw := r.Get("throttle").DefaultControlValues()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("DefaultControlValues is not valid JSON: %v", err)
	}
	limit, _ := m["limit"].(float64)
	windowSec, _ := m["windowSec"].(float64)
	if limit <= 0 {
		t.Errorf("limit = %v, want > 0", limit)
	}
	if windowSec <= 0 {
		t.Errorf("windowSec = %v, want > 0", windowSec)
	}
}

// TestThrottle_ParseForm_Valid checks that valid form values round-trip to
// the expected JSON shape.
func TestThrottle_ParseForm_Valid(t *testing.T) {
	r := steptype.Default()
	form := url.Values{"limit": {"5"}, "windowSec": {"3600"}}
	got, err := r.Get("throttle").ParseForm(form)
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if v, _ := m["limit"].(float64); v != 5 {
		t.Errorf("limit = %v, want 5", v)
	}
	if v, _ := m["windowSec"].(float64); v != 3600 {
		t.Errorf("windowSec = %v, want 3600", v)
	}
}

// TestThrottle_ParseForm_ZeroLimit verifies that a non-positive limit is rejected.
func TestThrottle_ParseForm_ZeroLimit(t *testing.T) {
	r := steptype.Default()
	form := url.Values{"limit": {"0"}, "windowSec": {"60"}}
	_, err := r.Get("throttle").ParseForm(form)
	if err == nil {
		t.Fatal("ParseForm: expected error for limit=0, got nil")
	}
}

// TestThrottle_Compile_Valid verifies that a throttle step compiles to the
// check.throttle normal task — this is the guard ensuring the engine handler
// task name matches what the registry emits.
func TestThrottle_Compile_Valid(t *testing.T) {
	r := steptype.Default()
	cv := json.RawMessage(`{"limit":5,"windowSec":60}`)
	sd, err := r.Get("throttle").Compile(domain.Step{StepID: "T1", Type: "throttle", ControlValues: cv})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if sd.Task != "check.throttle" {
		t.Errorf("Task = %q, want check.throttle", sd.Task)
	}
	if sd.Type != "normal" {
		t.Errorf("Type = %q, want normal", sd.Type)
	}
	if sd.ID != "T1" {
		t.Errorf("ID = %q, want T1", sd.ID)
	}
	if sd.Config != nil {
		t.Errorf("Config = %s, want nil (handler loads config from store)", sd.Config)
	}
}

// TestThrottle_Compile_ZeroLimit verifies that limit=0 fails compilation.
func TestThrottle_Compile_ZeroLimit(t *testing.T) {
	r := steptype.Default()
	_, err := r.Get("throttle").Compile(domain.Step{
		StepID: "T", Type: "throttle",
		ControlValues: json.RawMessage(`{"limit":0,"windowSec":60}`),
	})
	if err == nil {
		t.Fatal("Compile: expected error for limit=0, got nil")
	}
}

// TestThrottle_Compile_ZeroWindow verifies that windowSec=0 fails compilation.
func TestThrottle_Compile_ZeroWindow(t *testing.T) {
	r := steptype.Default()
	_, err := r.Get("throttle").Compile(domain.Step{
		StepID: "T", Type: "throttle",
		ControlValues: json.RawMessage(`{"limit":1,"windowSec":0}`),
	})
	if err == nil {
		t.Fatal("Compile: expected error for windowSec=0, got nil")
	}
}

// TestThrottle_Has verifies the registry knows about the throttle type.
func TestThrottle_Has(t *testing.T) {
	r := steptype.Default()
	if !r.Has("throttle") {
		t.Error("Has(throttle) = false, want true")
	}
	if r.Get("throttle") == nil {
		t.Error("Get(throttle) = nil, want non-nil")
	}
	if r.Get("throttle").Name() != "throttle" {
		t.Errorf("Name() = %q, want throttle", r.Get("throttle").Name())
	}
}
