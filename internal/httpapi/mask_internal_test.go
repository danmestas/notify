package httpapi

import (
	"testing"

	"github.com/danmestas/notify/internal/connectivity"
)

// TestMaskCredentials_FailsClosedForUnknownProvider proves an unrecognized
// provider has ALL fields masked (no secret leak), not none.
func TestMaskCredentials_FailsClosedForUnknownProvider(t *testing.T) {
	out := maskCredentials("future-provider", map[string]any{
		"secret_key": "leak-me",
		"region":     "us-east",
	})
	if out["secret_key"] != connectivity.CredentialMaskSentinel {
		t.Fatalf("unknown-provider secret leaked: %v", out["secret_key"])
	}
	if out["region"] != connectivity.CredentialMaskSentinel {
		t.Fatalf("unknown-provider field not masked (fail-closed): %v", out["region"])
	}
}

// TestMaskCredentials_EveryProviderHasSecretFields enforces that every provider
// the create switch accepts has an integrationSecretFields entry.
func TestMaskCredentials_EveryProviderHasSecretFields(t *testing.T) {
	for _, p := range []string{"smtp", "cloudflare-email", "ntfy", "expo", "fcm", "discord", "telegram", "twilio", "signalwire"} {
		if _, ok := integrationSecretFields[p]; !ok {
			t.Errorf("provider %q accepted by create but has no integrationSecretFields entry", p)
		}
	}
}
