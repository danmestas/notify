package steptype

import (
	"encoding/json"
	"net/url"
	"strings"
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

// TestEmailParseForm_HTMLBody: the console's html_body field is stored as
// htmlBody, and omitted when empty so existing steps stay byte-identical.
func TestEmailParseForm_HTMLBody(t *testing.T) {
	with, err := (email{}).ParseForm(url.Values{"subject": {"s"}, "body": {"b"}, "html_body": {"<p>b</p>"}})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	var got struct {
		HTMLBody string `json:"htmlBody"`
	}
	if err := json.Unmarshal(with, &got); err != nil || got.HTMLBody != "<p>b</p>" {
		t.Fatalf("htmlBody = %q (%v), from %s", got.HTMLBody, err, with)
	}
	without, _ := (email{}).ParseForm(url.Values{"subject": {"s"}, "body": {"b"}})
	if strings.Contains(string(without), "htmlBody") {
		t.Fatalf("empty html_body stored: %s", without)
	}
}
