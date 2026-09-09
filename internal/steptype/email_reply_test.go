package steptype

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/danmestas/notify/internal/runenv"
)

// TestEmailParseForm_ReplyOptIn: the checkbox posts "on" when ticked and
// nothing when not; only the ticked form stores the key, so an untouched
// step's control values stay byte-identical.
func TestEmailParseForm_ReplyOptIn(t *testing.T) {
	on, err := (email{}).ParseForm(url.Values{"subject": {"s"}, "body": {"b"}, "reply": {"on"}})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	var c runenv.StepControls
	if err := json.Unmarshal(on, &c); err != nil || !c.Reply {
		t.Fatalf("ticked: Reply=%v err=%v cv=%s", c.Reply, err, on)
	}

	off, err := (email{}).ParseForm(url.Values{"subject": {"s"}, "body": {"b"}})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if string(off) != `{"body":"b","subject":"s"}` {
		t.Fatalf("unticked control values changed shape: %s", off)
	}
}
