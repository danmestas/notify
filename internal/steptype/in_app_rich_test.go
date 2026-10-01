package steptype

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// TestInAppParseForm_RichFields proves the in_app editor form fields are unpacked
// into the controlValues shape: avatar (scalar), primaryAction /
// secondaryAction ({label, redirect{url,target}}), top-level redirect, and a
// free-form data object.
func TestInAppParseForm_RichFields(t *testing.T) {
	form := url.Values{
		"subject":                          {"Hi"},
		"body":                             {"Hello {{payload.x}}"},
		"avatar":                           {"https://x.com/a.png"},
		"primary_action_label":             {"Open"},
		"primary_action_redirect_url":      {"/foo"},
		"primary_action_redirect_target":   {"_self"},
		"secondary_action_label":           {"Dismiss"},
		"secondary_action_redirect_url":    {"/bye"},
		"secondary_action_redirect_target": {"_blank"},
		"redirect_url":                     {"/top"},
		"redirect_target":                  {"_top"},
		"data_json":                        {`{"orderId":"123"}`},
	}
	raw, err := (inApp{}).ParseForm(form)
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}

	var cv struct {
		Subject       string `json:"subject"`
		Body          string `json:"body"`
		Avatar        string `json:"avatar"`
		PrimaryAction *struct {
			Label    string `json:"label"`
			Redirect *struct {
				URL    string `json:"url"`
				Target string `json:"target"`
			} `json:"redirect"`
		} `json:"primaryAction"`
		SecondaryAction *struct {
			Label string `json:"label"`
		} `json:"secondaryAction"`
		Redirect *struct {
			URL    string `json:"url"`
			Target string `json:"target"`
		} `json:"redirect"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &cv); err != nil {
		t.Fatalf("unmarshal controlValues: %v\nraw=%s", err, raw)
	}
	if cv.Avatar != "https://x.com/a.png" {
		t.Errorf("avatar = %q", cv.Avatar)
	}
	if cv.PrimaryAction == nil || cv.PrimaryAction.Label != "Open" {
		t.Fatalf("primaryAction.label not parsed: %+v", cv.PrimaryAction)
	}
	if cv.PrimaryAction.Redirect == nil || cv.PrimaryAction.Redirect.URL != "/foo" || cv.PrimaryAction.Redirect.Target != "_self" {
		t.Errorf("primaryAction.redirect = %+v", cv.PrimaryAction.Redirect)
	}
	if cv.SecondaryAction == nil || cv.SecondaryAction.Label != "Dismiss" {
		t.Errorf("secondaryAction.label not parsed: %+v", cv.SecondaryAction)
	}
	if cv.Redirect == nil || cv.Redirect.URL != "/top" || cv.Redirect.Target != "_top" {
		t.Errorf("redirect = %+v", cv.Redirect)
	}
	if string(cv.Data) != `{"orderId":"123"}` {
		t.Errorf("data = %s, want %s", cv.Data, `{"orderId":"123"}`)
	}
}

// TestInAppParseForm_RedirectOmittedWhenURLBlank proves a redirect sub-object is
// dropped entirely when its url is blank, even if a target is supplied (a
// redirect requires url). Applies to both top-level and per-action redirects.
func TestInAppParseForm_RedirectOmittedWhenURLBlank(t *testing.T) {
	form := url.Values{
		"subject":                        {"Hi"},
		"body":                           {"Body"},
		"primary_action_label":           {"Open"},
		"primary_action_redirect_target": {"_blank"}, // target set, url blank
		"redirect_target":                {"_blank"}, // target set, url blank
	}
	raw, err := (inApp{}).ParseForm(form)
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	var cv map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := cv["redirect"]; ok {
		t.Errorf("top-level redirect emitted with blank url: %s", raw)
	}
	// primaryAction is present (has a label) but its redirect must be absent.
	var pa struct {
		Redirect json.RawMessage `json:"redirect"`
	}
	if err := json.Unmarshal(cv["primaryAction"], &pa); err != nil {
		t.Fatalf("unmarshal primaryAction: %v", err)
	}
	if len(pa.Redirect) != 0 {
		t.Errorf("primaryAction.redirect emitted with blank url: %s", pa.Redirect)
	}
}

// TestInAppParseForm_InvalidDataRejected proves a non-object data payload is a
// user-visible error (mirrors push data_json validation).
func TestInAppParseForm_InvalidDataRejected(t *testing.T) {
	for _, bad := range []string{`[1,2,3]`, `"str"`, `not json`, `42`} {
		form := url.Values{"subject": {"s"}, "body": {"b"}, "data_json": {bad}}
		if _, err := (inApp{}).ParseForm(form); err == nil {
			t.Errorf("ParseForm(data_json=%q): expected error, got nil", bad)
		}
	}
}

// TestInAppControlMap_RichFields proves the in_app ControlMap flattens nested
// rich fields with PREFIX-EXPLICIT dot-notation keys the editor template reads.
func TestInAppControlMap_RichFields(t *testing.T) {
	cv := json.RawMessage(`{"subject":"S","body":"B","avatar":"https://x.com/a.png",` +
		`"primaryAction":{"label":"Open","redirect":{"url":"/foo","target":"_self"}},` +
		`"secondaryAction":{"label":"Dismiss"},` +
		`"redirect":{"url":"/top","target":"_blank"},` +
		`"data":{"orderId":"123"}}`)
	m := (inApp{}).ControlMap(cv)

	want := map[string]string{
		"subject":                       "S",
		"body":                          "B",
		"avatar":                        "https://x.com/a.png",
		"primaryAction.label":           "Open",
		"primaryAction.redirect.url":    "/foo",
		"primaryAction.redirect.target": "_self",
		"secondaryAction.label":         "Dismiss",
		"redirect.url":                  "/top",
		"redirect.target":               "_blank",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("ControlMap[%q] = %q, want %q", k, m[k], v)
		}
	}
	// data is re-rendered as a compact JSON string for the textarea.
	if !strings.Contains(m["data"], `"orderId":"123"`) {
		t.Errorf("ControlMap[data] = %q, want it to contain the data object JSON", m["data"])
	}
}
