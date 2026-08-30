package steptype_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/steptype"
)

// TestThrottle_ThrottleKeyRoundTripsAndValidates covers the #295 control:
// a payload dot-path round-trips through the form, a blank field is omitted
// entirely (so existing workflows keep one shared budget), and a
// subscriber-rooted path is rejected rather than silently doing nothing.
func TestThrottle_ThrottleKeyRoundTripsAndValidates(t *testing.T) {
	st := steptype.Default().Get("throttle")
	if st == nil {
		t.Fatal("throttle step type not registered")
	}

	cv, err := st.ParseForm(url.Values{
		"limit": {"3"}, "windowSec": {"3600"}, "throttleKey": {"payload.check"},
	})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if got := st.ControlMap(cv)["throttleKey"]; got != "payload.check" {
		t.Errorf("throttleKey = %q, want payload.check", got)
	}
	if err := st.Validate(cv); err != nil {
		t.Errorf("Validate: %v", err)
	}

	blank, err := st.ParseForm(url.Values{"limit": {"3"}, "windowSec": {"3600"}, "throttleKey": {""}})
	if err != nil {
		t.Fatalf("ParseForm blank: %v", err)
	}
	if strings.Contains(string(blank), "throttleKey") {
		t.Errorf("blank throttleKey must be omitted, got %s", blank)
	}

	bad := json.RawMessage(`{"limit":3,"windowSec":3600,"throttleKey":"subscriber.data.plan"}`)
	if err := st.Validate(bad); err == nil {
		t.Error("Validate accepted a subscriber-rooted throttleKey, want a rejection")
	}
}
