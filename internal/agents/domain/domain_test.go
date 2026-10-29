package domain

import (
	"encoding/base64"
	"testing"
)

// SupportedRuntimes must list only runtimes that have a registered executor in
// cmd/notify/main.go (mesh and managed). bridge is a defined constant with no
// WithRuntime registration, so it must NOT be supported.
func TestSupportedRuntimes_MeshAndManaged(t *testing.T) {
	if len(SupportedRuntimes) != 2 || SupportedRuntimes[0] != RuntimeMesh || SupportedRuntimes[1] != RuntimeManaged {
		t.Fatalf("SupportedRuntimes = %v, want [%q %q]", SupportedRuntimes, RuntimeMesh, RuntimeManaged)
	}
}

func TestIsSupportedRuntime(t *testing.T) {
	cases := []struct {
		runtime string
		want    bool
	}{
		{RuntimeMesh, true},
		{RuntimeBridge, false},
		{RuntimeManaged, true},
		{"bogus", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsSupportedRuntime(tc.runtime); got != tc.want {
			t.Errorf("IsSupportedRuntime(%q) = %v, want %v", tc.runtime, got, tc.want)
		}
	}
}

// TestValidIdentifier proves the H2-class fix for agent identifiers: the
// URL-safe slug allowlist (^[a-z0-9][a-z0-9_-]*$) rejects characters that
// would otherwise be used verbatim as a console URL path segment
// (view/edit/delete/save/activate routes), permanently orphaning the agent
// once it 404'd forever. Mirrors authoring.triggerIdentifierPattern (H2).
func TestValidIdentifier(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"a", true},
		{"support-bot", true},
		{"agent_2", true},
		{"agent123", true},
		{"foo/bar", false},
		{"a b", false},
		{"Foo", false},
		{"<script>", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := ValidIdentifier(tc.id); got != tc.want {
			t.Errorf("ValidIdentifier(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestValidateLogo(t *testing.T) {
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	jpeg := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xe0, 0, 0x10})
	webp := "data:image/webp;base64," + base64.StdEncoding.EncodeToString([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "))
	big := "data:image/png;base64," + base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, MaxLogoBytes)...))
	atLimit := "data:image/png;base64," + base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, MaxLogoBytes-8)...))
	cases := []struct {
		name string
		logo string
		ok   bool
	}{
		{"empty clears", "", true},
		{"png", png, true},
		{"jpeg", jpeg, true},
		{"webp", webp, true},
		{"exactly the limit", atLimit, true},
		{"over the limit", big, false},
		{"svg", "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg/>")), false},
		{"gif", "data:image/gif;base64," + base64.StdEncoding.EncodeToString([]byte("GIF89a")), false},
		{"not a data uri", "https://example.com/a.png", false},
		{"not base64 declared", "data:image/png,abc", false},
		{"bad base64", "data:image/png;base64,@@@", false},
		{"empty payload", "data:image/png;base64,", false},
		{"bytes do not match type", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("hello world")), false},
		{"extra parameters", "data:image/png;charset=x;base64,iVBORw0KGgo=", false},
		{"uppercase scheme", "DATA:image/png;base64,iVBORw0KGgo=", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLogo(tc.logo)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateLogo err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
