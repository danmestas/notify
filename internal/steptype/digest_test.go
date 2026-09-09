package steptype_test

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// TestDigest_DefaultControlValues pins the seeded blob for a freshly added
// digest step: a regular window with an empty digestKey. The legacy delay
// default stays {"amount":1,"unit":"minutes"}; digest now carries type +
// digestKey so the editor can round-trip them.
func TestDigest_DefaultControlValues(t *testing.T) {
	got := string(steptype.Default().Get("digest").DefaultControlValues())
	want := `{"type":"regular","amount":1,"unit":"minutes","digestKey":""}`
	if got != want {
		t.Fatalf("digest DefaultControlValues = %s, want %s", got, want)
	}
}

// TestCompile_DigestRegular_WithDigestKey proves a regular digest compiles to
// the SAME sleep StepDef whether or not a digestKey is configured — the
// digestKey is a trigger-time grouping concern, never part of the compiled DAG.
func TestCompile_DigestRegular_WithDigestKey(t *testing.T) {
	d := steptype.Default().Get("digest")
	plain, err := d.Compile(domain.Step{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"type":"regular","amount":3,"unit":"minutes"}`)})
	if err != nil {
		t.Fatalf("compile plain: %v", err)
	}
	keyed, err := d.Compile(domain.Step{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"type":"regular","amount":3,"unit":"minutes","digestKey":"payload.orderId"}`)})
	if err != nil {
		t.Fatalf("compile keyed: %v", err)
	}
	if plain.Type != "sleep" || string(plain.Config) != `{"until_input_path":"digestDeadline"}` {
		t.Fatalf("regular digest def = %+v, want a run-input-resolved sleep", plain)
	}
	if string(keyed.Config) != string(plain.Config) || keyed.Type != plain.Type {
		t.Fatalf("digestKey changed compiled StepDef: %+v vs %+v", keyed, plain)
	}
}

// TestCompile_DigestRegular_BackwardCompat proves legacy controlValues with no
// "type" field (only amount+unit) still compile to a sleep step — type defaults
// to regular so existing persisted digests keep firing unchanged.
func TestCompile_DigestRegular_BackwardCompat(t *testing.T) {
	def, err := steptype.Default().Get("digest").Compile(domain.Step{
		StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"amount":1,"unit":"seconds"}`),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if def.Type != "sleep" || string(def.Config) != `{"until_input_path":"digestDeadline"}` {
		t.Fatalf("legacy digest def = %+v, want a run-input-resolved sleep", def)
	}
	// The authored window is no longer IN the compiled config, so Compile must
	// still validate it -- otherwise a bad amount would only surface at
	// dispatch, as a failed run rather than a refused workflow.
	if _, err := steptype.Default().Get("digest").Compile(domain.Step{
		StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"amount":0,"unit":"seconds"}`),
	}); err == nil {
		t.Fatal("Compile accepted a zero digest amount; the window is validated here or nowhere")
	}
}

// TestCompile_DigestScheduled proves a scheduled digest with a valid cron
// expression compiles to a normal digest.wait task carrying no static Config —
// the wait duration is computed per-run by the handler, not baked into the DAG.
func TestCompile_DigestScheduled(t *testing.T) {
	def, err := steptype.Default().Get("digest").Compile(domain.Step{
		StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"type":"scheduled","cron":"0 * * * *","digestKey":"payload.orderId"}`),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if def.ID != "dg" || def.Type != "normal" || def.Task != "digest.wait" {
		t.Fatalf("scheduled digest def = %+v, want {ID:dg Type:normal Task:digest.wait}", def)
	}
	if def.Config != nil {
		t.Fatalf("scheduled digest Config = %s, want nil", def.Config)
	}
}

// TestCompile_DigestScheduled_HasRetryPolicy pins that a scheduled-digest
// StepDef carries a RetryPolicy with a large MaxAttempts so that a process
// restart during a cron wait requeues the digest.wait task rather than
// dead-lettering it. Mirrors TestDelayRetryPolicy_SurvivesRestarts for delay.
func TestCompile_DigestScheduled_HasRetryPolicy(t *testing.T) {
	def, err := steptype.Default().Get("digest").Compile(domain.Step{
		StepID: "dg", Type: "digest",
		ControlValues: json.RawMessage(`{"type":"scheduled","cron":"0 * * * *"}`),
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if def.Retry == nil || def.Retry.MaxAttempts < 100 {
		t.Fatalf("scheduled digest Retry = %+v: MaxAttempts must be >= 100 to survive routine deploys", def.Retry)
	}
}

// TestCompile_DigestScheduled_InvalidCron proves an unparseable cron expression
// fails compilation (which authoring maps to ErrCompileFailure / HTTP 422),
// so no bad DAG is ever registered.
func TestCompile_DigestScheduled_InvalidCron(t *testing.T) {
	_, err := steptype.Default().Get("digest").Compile(domain.Step{
		StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"type":"scheduled","cron":"not-a-cron"}`),
	})
	if err == nil {
		t.Fatal("compile invalid cron: want error, got nil")
	}
}

// TestDigest_ParseForm_Regular builds regular-digest controlValues from the
// editor form fields, defaulting the window unit to minutes.
func TestDigest_ParseForm_Regular(t *testing.T) {
	got, err := steptype.Default().Get("digest").ParseForm(url.Values{
		"type": {"regular"}, "amount": {"5"}, "unit": {"minutes"}, "digestKey": {"payload.orderId"},
	})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	want := `{"amount":5,"digestKey":"payload.orderId","type":"regular","unit":"minutes"}`
	if string(got) != want {
		t.Fatalf("ParseForm regular = %s, want %s", got, want)
	}
}

// TestDigest_ParseForm_Scheduled builds scheduled-digest controlValues and
// rejects an empty/invalid cron at save time.
func TestDigest_ParseForm_Scheduled(t *testing.T) {
	got, err := steptype.Default().Get("digest").ParseForm(url.Values{
		"type": {"scheduled"}, "cron": {"0 * * * *"}, "digestKey": {"subscriber.data.plan"},
	})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	want := `{"cron":"0 * * * *","digestKey":"subscriber.data.plan","type":"scheduled"}`
	if string(got) != want {
		t.Fatalf("ParseForm scheduled = %s, want %s", got, want)
	}
	if _, err := steptype.Default().Get("digest").ParseForm(url.Values{
		"type": {"scheduled"}, "cron": {"nope"},
	}); err == nil {
		t.Fatal("ParseForm invalid cron: want error, got nil")
	}
}

// TestParseCron_FiveFieldOnly proves the shared parser accepts the 5-field
// standard form. The 6-field with-seconds form and @descriptors were removed in
// #258 so the grammar matches what the dagnats engine can express; see
// digestcron_test.go for the rejection cases.
func TestParseCron_FiveFieldOnly(t *testing.T) {
	for _, spec := range []string{"0 * * * *", "* * * * *"} {
		if _, err := steptype.ParseCron(spec); err != nil {
			t.Errorf("ParseCron(%q): %v", spec, err)
		}
	}
	if _, err := steptype.ParseCron("not-a-cron"); err == nil {
		t.Error("ParseCron(not-a-cron): want error, got nil")
	}
}
