package domain

import "testing"

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
