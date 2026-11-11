package notify

// Methodology: table test over cleanBasePath, the one place Host.BasePath is
// validated. Host config, so bad input must fail at New rather than emit
// URLs that leave the origin or break routing.

import "testing"

func TestCleanBasePath(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""},
		{"/", ""},
		{"/notify", "/notify"},
		{"/notify/", "/notify"},
		{"/a/b", "/a/b"},
		{"/a-b_c.d~e", "/a-b_c.d~e"},
	}
	for _, tc := range ok {
		got, err := cleanBasePath(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("cleanBasePath(%q) = %q, %v; want %q, nil", tc.in, got, err, tc.want)
		}
	}
	bad := []string{
		"notify",           // not rooted
		"//evil.com",       // protocol-relative
		"//",               // empty segment
		"/a//b",            // internal //
		"/a//",             // trailing empty segment
		"/a?x=1",           // query
		"/a#frag",          // fragment
		"/a\\b",            // backslash
		"/\\evil.com",      // backslash host trick
		"https://evil.com", // absolute URL
	}
	for _, in := range bad {
		if got, err := cleanBasePath(in); err == nil {
			t.Errorf("cleanBasePath(%q) = %q, nil; want error", in, got)
		}
	}
}
