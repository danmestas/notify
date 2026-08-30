package steptype

import (
	"net/url"
	"testing"
)

// TestSendStepTypes_IntegrationControl_RoundTrips proves the optional
// "integration" control value round-trips through ParseForm -> controlValues
// -> ControlMap for every send step type (email, sms, push, chat), and that
// omitting it from the form leaves it out of the persisted controlValues
// entirely (so existing workflows stay byte-identical).
func TestSendStepTypes_IntegrationControl_RoundTrips(t *testing.T) {
	cases := []struct {
		name string
		st   StepType
		form url.Values
	}{
		{"email", email{}, url.Values{"subject": {"s"}, "body": {"b"}, "integration": {"mastodon-danmestas"}}},
		{"sms", sms{}, url.Values{"body": {"b"}, "integration": {"twilio-secondary"}}},
		{"push", push{}, url.Values{"title": {"t"}, "body": {"b"}, "integration": {"fcm-eu"}}},
		{"chat", chat{}, url.Values{"body": {"b"}, "integration": {"discord-alerts"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cv, err := tc.st.ParseForm(tc.form)
			if err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			got := tc.st.ControlMap(cv)
			want := tc.form.Get("integration")
			if got["integration"] != want {
				t.Fatalf("ControlMap[integration] = %q, want %q (controlValues: %s)", got["integration"], want, cv)
			}
			if err := tc.st.Validate(cv); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// TestSendStepTypes_IntegrationControl_OmittedWhenBlank proves an absent or
// blank "integration" form field never appears in the persisted
// controlValues, so pre-existing workflows (and their tests) that never set
// it are unaffected.
func TestSendStepTypes_IntegrationControl_OmittedWhenBlank(t *testing.T) {
	cases := []struct {
		name string
		st   StepType
		form url.Values
	}{
		{"email", email{}, url.Values{"subject": {"s"}, "body": {"b"}}},
		{"sms", sms{}, url.Values{"body": {"b"}}},
		{"push", push{}, url.Values{"title": {"t"}, "body": {"b"}}},
		{"chat", chat{}, url.Values{"body": {"b"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cv, err := tc.st.ParseForm(tc.form)
			if err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			got := tc.st.ControlMap(cv)
			if v, ok := got["integration"]; ok {
				t.Fatalf("ControlMap[integration] present as %q, want absent", v)
			}
		})
	}
}
