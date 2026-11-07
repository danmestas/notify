package connectivity

import (
	"strings"
	"testing"
)

func TestRedactCredentials(t *testing.T) {
	creds := []byte(`{"base_url":"https://ntfy.example.com","token":"tk_secret_1","password":"hunter22","nested":{"key":"abc-def-ghi"},"port":"587","tls":"on"}`)
	got := redactCredentials(
		"probe https://ntfy.example.com failed: bad token tk_secret_1, password=hunter22, key abc-def-ghi, tls on, port 587",
		creds,
	)
	for _, leaked := range []string{"tk_secret_1", "hunter22", "abc-def-ghi"} {
		if strings.Contains(got, leaked) {
			t.Errorf("redacted text still holds %q: %s", leaked, got)
		}
	}
	// The server location and short non-secret values stay readable.
	for _, kept := range []string{"https://ntfy.example.com", "tls on", "port 587"} {
		if !strings.Contains(got, kept) {
			t.Errorf("redacted text lost %q: %s", kept, got)
		}
	}
}

func TestRedactCredentials_LongerValueWinsOverItsSubstring(t *testing.T) {
	got := redactCredentials("saw tokenvalue-long", []byte(`{"a":"tokenvalue","b":"tokenvalue-long"}`))
	if strings.Contains(got, "tokenvalue") || strings.Contains(got, "-long") {
		t.Errorf("partial redaction left a fragment: %q", got)
	}
}

func TestRedactCredentials_UnparseableOrEmptyChangesNothing(t *testing.T) {
	if got := redactCredentials("hello", []byte("not json")); got != "hello" {
		t.Errorf("got %q", got)
	}
	if got := redactCredentials("", []byte(`{"a":"secretvalue"}`)); got != "" {
		t.Errorf("got %q", got)
	}
}
