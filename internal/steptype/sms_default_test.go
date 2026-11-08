package steptype

import (
	"encoding/json"
	"net/url"
	"testing"
)

// TestSMSParseForm_DefaultEnabled: the console's "send without a preference"
// checkbox round-trips as controlValues.defaultEnabled, and is absent (off)
// when unchecked, so a console save never silently turns SMS on.
func TestSMSParseForm_DefaultEnabled(t *testing.T) {
	on, err := sms{}.ParseForm(url.Values{"body": {"b"}, "default_enabled": {"on"}})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(on, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["defaultEnabled"] != true {
		t.Errorf("checked: controlValues = %s, want defaultEnabled true", on)
	}
	if cm := (sms{}).ControlMap(on); cm["defaultEnabled"] != "true" {
		t.Errorf("ControlMap = %v, want defaultEnabled \"true\" for the editor", cm)
	}

	off, err := sms{}.ParseForm(url.Values{"body": {"b"}})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	got = nil
	_ = json.Unmarshal(off, &got)
	if _, present := got["defaultEnabled"]; present {
		t.Errorf("unchecked: controlValues = %s, want no defaultEnabled", off)
	}
}
