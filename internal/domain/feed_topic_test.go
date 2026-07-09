package domain

import (
	"strings"
	"testing"
)

// TestValidateFeedTopic table-tests the subject-injection gate. Relocated
// from internal/eventbus (Slice A4 prep refactor): the validator itself now
// lives in domain so delivery (which must not import eventbus) can call it
// directly. eventbus's own feed tests (TestPublishFeed_RejectsInvalidTopic,
// TestConsumeFeed_RejectsInvalidTopic) stay in place and prove the wiring,
// not the rule table.
func TestValidateFeedTopic(t *testing.T) {
	cases := []struct {
		name    string
		topic   string
		wantErr bool
	}{
		{"simple", "orders", false},
		{"multi-segment", "orders.eu-west-1.created", false},
		{"underscore-dash-digit", "a_b-c.d2", false},
		{"empty", "", true},
		{"leading-dot", ".orders", true},
		{"trailing-dot", "orders.", true},
		{"double-dot", "a..b", true},
		{"star-wildcard", "orders.*", true},
		{"bare-gt", ">", true},
		{"gt-wildcard-suffix", "orders.>", true},
		{"internal-whitespace", "or ders", true},
		{"dollar-sign", "orders.$JS.api", true},
		{"too-long", strings.Repeat("a", 129), true},
		{"exactly-max-length", strings.Repeat("a", 128), false},
		{"star-alone", "*", true},
		{"unicode", "orders.☺", true},
		{"nul-byte", "orders.\x00", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFeedTopic(tc.topic)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateFeedTopic(%q) error = %v, wantErr %v", tc.topic, err, tc.wantErr)
			}
		})
	}
}
