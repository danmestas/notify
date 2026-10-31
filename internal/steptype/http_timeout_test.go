package steptype

import (
	"net/url"
	"testing"
	"time"
)

// TestHTTPTimeout_DefaultsAndClamps pins the run-time bound of an http_request
// step: a missing timeout is 30s, and an authored one is capped at
// HTTPMaxTimeoutSec, which the task's max run time is derived from. A stored
// workflow from before the cap still runs, just no longer than the cap.
func TestHTTPTimeout_DefaultsAndClamps(t *testing.T) {
	cases := []struct {
		sec  int
		want time.Duration
	}{
		{0, 30 * time.Second},
		{-5, 30 * time.Second},
		{45, 45 * time.Second},
		{HTTPMaxTimeoutSec, HTTPMaxTimeoutSec * time.Second},
		{HTTPMaxTimeoutSec + 1, HTTPMaxTimeoutSec * time.Second},
		{86400, HTTPMaxTimeoutSec * time.Second},
	}
	for _, c := range cases {
		if got := HTTPTimeout(c.sec); got != c.want {
			t.Errorf("HTTPTimeout(%d) = %v, want %v", c.sec, got, c.want)
		}
	}
}

// TestHTTPRequestParseForm_RejectsTimeoutOverCap proves the editor refuses a
// timeout the engine would not honour, instead of saving it and clamping later.
func TestHTTPRequestParseForm_RejectsTimeoutOverCap(t *testing.T) {
	form := url.Values{"timeoutSec": {"301"}, "url": {"https://example.com"}}
	if _, err := (httpRequest{}).ParseForm(form); err == nil {
		t.Fatal("ParseForm accepted timeoutSec 301, over the 300s cap")
	}
	form.Set("timeoutSec", "300")
	if _, err := (httpRequest{}).ParseForm(form); err != nil {
		t.Fatalf("ParseForm rejected timeoutSec 300: %v", err)
	}
}
