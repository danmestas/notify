package domain

import "testing"

// SupportedRuntimes must list only runtimes that have a registered executor in
// cmd/notify/main.go (currently mesh). bridge/managed are defined constants but
// have no WithRuntime registration, so they must NOT be supported.
func TestSupportedRuntimes_OnlyMesh(t *testing.T) {
	if len(SupportedRuntimes) != 1 || SupportedRuntimes[0] != RuntimeMesh {
		t.Fatalf("SupportedRuntimes = %v, want [%q]", SupportedRuntimes, RuntimeMesh)
	}
}

func TestIsSupportedRuntime(t *testing.T) {
	cases := []struct {
		runtime string
		want    bool
	}{
		{RuntimeMesh, true},
		{RuntimeBridge, false},
		{RuntimeManaged, false},
		{"bogus", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsSupportedRuntime(tc.runtime); got != tc.want {
			t.Errorf("IsSupportedRuntime(%q) = %v, want %v", tc.runtime, got, tc.want)
		}
	}
}
