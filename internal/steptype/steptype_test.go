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
func TestNames_CanonicalOrder(t *testing.T) {
	got := steptype.Default().Names()
	want := []string{"in_app", "email", "push", "chat", "sms", "delay", "digest"}
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
