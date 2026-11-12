package twilio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/danmestas/notify/internal/providers"
)

// TestSend_StatusCallback: a message with a StatusCallback asks Twilio to
// report its delivery status there; one without sends no such field.
func TestSend_StatusCallback(t *testing.T) {
	var got []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = append(got, r.PostForm)
		_, _ = w.Write([]byte(`{"sid":"SM1"}`))
	}))
	defer srv.Close()
	c := &Client{AccountSID: "AC1", AuthToken: "tok", From: "+15550001111", BaseURL: srv.URL}
	const cb = "https://notify.example.com/v1/inbound/sms/int-1/status"
	if _, err := c.Send(context.Background(), providers.SMSMessage{To: "+15551112222", Body: "hi", StatusCallback: cb}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := c.Send(context.Background(), providers.SMSMessage{To: "+15551112222", Body: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got[0].Get("StatusCallback") != cb {
		t.Errorf("StatusCallback = %q, want %q", got[0].Get("StatusCallback"), cb)
	}
	if _, ok := got[1]["StatusCallback"]; ok {
		t.Error("a send without a callback sent a StatusCallback field")
	}
}
