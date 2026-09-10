package emailloop

import "testing"

// TestStripQuotedHistory pins the cut rules, including the wrapped Gmail
// attribution line that let a whole quoted original through (#352).
func TestStripQuotedHistory(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"one-line attribution", "Yes\n\nOn Mon, Sep 8, 2026 at 09:00 Ann <a@x> wrote:\n> hi\n", "Yes"},
		{
			"gmail wraps before wrote:",
			"Received\n\nOn Thu, Sep 10, 2026 at 07:21 quarry <daniel@example.org>\nwrote:\n\n> zeitgeist wrote:\n> the original\n",
			"Received",
		},
		{
			"apple mail wraps a long display name twice",
			"Done\n\nOn 10 Sep 2026, at 07:21, Some Very Long Display Name\nOf A Person <p@example.org>\nwrote:\n\n> original\n",
			"Done",
		},
		{"quoted run at the end with no attribution", "Sure\n\n> can you?\n> please\n", "Sure"},
		{"quoted run followed by text is inline answering", "> can you?\nYes\n> and this?\nNo", "> can you?\nYes\n> and this?\nNo"},
		{"outlook divider", "ok\n-----Original Message-----\nFrom: x", "ok"},
		{"forward divider", "fyi\n-------- Forwarded message --------\nFrom: x", "fyi"},
		{"On sentence that is not an attribution", "On Monday I will be out.\nThanks", "On Monday I will be out.\nThanks"},
		{"wrote: far below an On line is not a marker", "On the topic:\nfirst\nsecond\nthird\nshe wrote:\nmore", "On the topic:\nfirst\nsecond\nthird\nshe wrote:\nmore"},
		{"no history", "just a reply", "just a reply"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripQuotedHistory(tc.body); got != tc.want {
				t.Fatalf("stripQuotedHistory(%q)\n got %q\nwant %q", tc.body, got, tc.want)
			}
		})
	}
}
