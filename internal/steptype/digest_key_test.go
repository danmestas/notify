package steptype

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestDigestKey_Validation is the guard for #336: an unrooted digestKey used to
// provision cleanly and then merge every event into a single digest, with no
// warning anywhere. Authoring must refuse it instead.
func TestDigestKey_Validation(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "empty is fine, it means no grouping", key: ""},
		{name: "rooted payload path", key: "payload.repo"},
		{name: "rooted nested payload path", key: "payload.repo.name"},
		{name: "unrooted single segment", key: "repo", wantErr: true},
		{name: "unrooted dotted path", key: "data.repo", wantErr: true},
		{name: "root with no leaf", key: "payload.", wantErr: true},
		{name: "bare root", key: "payload", wantErr: true},
		// Subscriber-rooted keys are long-standing tested behaviour here,
		// unlike throttle where they are rejected as a no-op.
		{name: "subscriber rooted", key: "subscriber.data.plan"},
		{name: "subscriber root with no leaf", key: "subscriber.", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDigestKey(tc.key)
			if tc.wantErr && err == nil {
				t.Fatalf("validateDigestKey(%q) = nil, want an error", tc.key)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateDigestKey(%q) = %v, want nil", tc.key, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "payload.repo") {
				t.Errorf("error should show the expected shape, got %q", err)
			}
		})
	}
}

// TestDigestParseForm_RejectsUnrootedKey: the console save path must refuse it,
// which is the same place data_json is checked for push.
func TestDigestParseForm_RejectsUnrootedKey(t *testing.T) {
	_, err := digest{}.ParseForm(url.Values{
		"type": {"regular"}, "amount": {"5"}, "unit": {"minutes"},
		"digestKey": {"repo"},
	})
	if err == nil {
		t.Fatal("ParseForm accepted an unrooted digestKey; it would silently merge every event")
	}

	// The rooted form still saves, for both window types.
	if _, err := (digest{}).ParseForm(url.Values{
		"type": {"regular"}, "amount": {"5"}, "unit": {"minutes"},
		"digestKey": {"payload.repo"},
	}); err != nil {
		t.Fatalf("ParseForm rejected a valid regular window: %v", err)
	}
	if _, err := (digest{}).ParseForm(url.Values{
		"type": {"scheduled"}, "cron": {"0 9 * * *"}, "digestKey": {"payload.repo"},
	}); err != nil {
		t.Fatalf("ParseForm rejected a valid scheduled window: %v", err)
	}
}

// TestDigestValidate_RejectsUnrootedKey covers the aibuilder draft path, which
// gates on Validate rather than ParseForm.
func TestDigestValidate_RejectsUnrootedKey(t *testing.T) {
	if err := (digest{}).Validate(json.RawMessage(`{"type":"regular","amount":1,"unit":"minutes","digestKey":"repo"}`)); err == nil {
		t.Fatal("Validate accepted an unrooted digestKey")
	}
	if err := (digest{}).Validate(json.RawMessage(`{"type":"regular","amount":1,"unit":"minutes","digestKey":"payload.repo"}`)); err != nil {
		t.Fatalf("Validate rejected a valid digestKey: %v", err)
	}
}

// TestDigestCompile_StillAcceptsStoredUnrootedKey is the deliberate limit of
// the fix. Compile also runs on the boot republish path, so rejecting there
// would un-register an already-stored workflow carrying a bad key — turning a
// misgrouped digest into no digest at all on upgrade. Existing steps keep
// compiling; trigger.go warns when their key resolves to nothing.
func TestDigestCompile_StillAcceptsStoredUnrootedKey(t *testing.T) {
	stored := json.RawMessage(`{"type":"regular","amount":1,"unit":"minutes","digestKey":"repo"}`)
	if _, cErr := digestType(stored); cErr != nil {
		t.Fatalf("digestType rejected a stored unrooted key: %v -- this would break "+
			"republish for workflows authored before the validation existed, turning a "+
			"misgrouped digest into no digest at all", cErr)
	}
	if _, cErr := (digest{}).Compile(domainStepWith(stored)); cErr != nil {
		t.Fatalf("Compile rejected a stored unrooted key: %v", cErr)
	}
}

// domainStepWith wraps control values in the minimal step Compile needs.
func domainStepWith(cv json.RawMessage) domain.Step {
	return domain.Step{StepID: "d1", Type: "digest", ControlValues: cv}
}
