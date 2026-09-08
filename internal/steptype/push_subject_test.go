package steptype

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/danmestas/notify/internal/runenv"
)

// TestPushControls_TitleReachesSubjectTemplate is the end-to-end guard for
// #334: a push step authored through the console form must produce control
// values whose subject line actually reaches runenv.Step.SubjectTemplate.
//
// The pre-fix code round-tripped console-to-console through a "title" key that
// no reader consulted, so the editor looked right while every delivered push
// notification had an empty title. Asserting the form output through
// StepControls is what closes that gap.
func TestPushControls_TitleReachesSubjectTemplate(t *testing.T) {
	cv, err := push{}.ParseForm(url.Values{
		"subject": {"{{payload.from}} mentioned you"},
		"body":    {"in {{payload.repo}}"},
	})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}

	var controls runenv.StepControls
	if err := json.Unmarshal(cv, &controls); err != nil {
		t.Fatalf("unmarshal control values: %v", err)
	}
	// Assert the CANONICAL field, not SubjectTemplate(): the legacy fallback
	// would happily rescue a value still stored under "title" and hide the
	// fact that ParseForm never got fixed.
	if controls.Subject != "{{payload.from}} mentioned you" {
		t.Fatalf("controls.Subject = %q, want the authored title stored under the "+
			"canonical key; an empty subject is a push notification with no title "+
			"on the device", controls.Subject)
	}
	if controls.Title != "" {
		t.Fatalf("controls.Title = %q, want empty: \"title\" is a legacy spelling to "+
			"be read, never written", controls.Title)
	}
}

// TestPushControls_LegacyTitleStillRenders: steps authored before the fix have
// "title" in their stored control values and must keep working without being
// re-saved.
func TestPushControls_LegacyTitleStillRenders(t *testing.T) {
	var controls runenv.StepControls
	if err := json.Unmarshal([]byte(`{"title":"Legacy title","body":"b"}`), &controls); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := controls.SubjectTemplate(); got != "Legacy title" {
		t.Fatalf("SubjectTemplate() = %q, want the legacy title to still render", got)
	}
}

// TestPushControls_SubjectWinsOverTitle: a step carrying both keys (the
// consumer's workaround while the bug was open) must use subject.
func TestPushControls_SubjectWinsOverTitle(t *testing.T) {
	var controls runenv.StepControls
	if err := json.Unmarshal([]byte(`{"subject":"New","title":"Old","body":"b"}`), &controls); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := controls.SubjectTemplate(); got != "New" {
		t.Fatalf("SubjectTemplate() = %q, want subject to win over the legacy title", got)
	}
}

// TestPushControlMap_PromotesLegacyTitle: the editor must show a legacy step's
// title, otherwise opening and saving it silently overwrites the stored value
// with an empty one.
func TestPushControlMap_PromotesLegacyTitle(t *testing.T) {
	m := push{}.ControlMap(json.RawMessage(`{"title":"Legacy title","body":"b"}`))
	if m["subject"] != "Legacy title" {
		t.Fatalf("ControlMap[subject] = %q, want the legacy title promoted so a "+
			"save does not blank it", m["subject"])
	}

	// A canonical step is untouched.
	m = push{}.ControlMap(json.RawMessage(`{"subject":"Real","title":"Stale","body":"b"}`))
	if m["subject"] != "Real" {
		t.Fatalf("ControlMap[subject] = %q, want the canonical subject preserved", m["subject"])
	}
}

// TestChannelParseForm_SubjectSpelling pins which channels author a subject
// line and under which key. push and email are the only two; sms, chat and
// stream deliberately author none, so there is nothing for them to drop.
func TestChannelParseForm_SubjectSpelling(t *testing.T) {
	tests := []struct {
		name        string
		parse       func(url.Values) (json.RawMessage, error)
		form        url.Values
		wantSubject string
	}{
		{"push", push{}.ParseForm, url.Values{"subject": {"T"}, "body": {"b"}}, "T"},
		{"email", email{}.ParseForm, url.Values{"subject": {"S"}, "body": {"b"}}, "S"},
		{"sms", sms{}.ParseForm, url.Values{"body": {"b"}}, ""},
		{"chat", chat{}.ParseForm, url.Values{"body": {"b"}}, ""},
		{"stream", stream{}.ParseForm, url.Values{"topic": {"t"}, "body": {"b"}}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cv, err := tc.parse(tc.form)
			if err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			var controls runenv.StepControls
			if err := json.Unmarshal(cv, &controls); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if controls.Subject != tc.wantSubject {
				t.Errorf("controls.Subject = %q, want %q", controls.Subject, tc.wantSubject)
			}
			if controls.Title != "" {
				t.Errorf("controls.Title = %q, want empty: no channel writes the legacy key", controls.Title)
			}
		})
	}
}
