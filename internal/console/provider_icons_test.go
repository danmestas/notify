package console

import (
	"strings"
	"testing"
)

// TestProviderIcon_KnownID returns the embedded SVG bytes for a catalog
// provider id, ready to inline in the drawer's provider rows.
func TestProviderIcon_KnownID(t *testing.T) {
	for _, id := range []string{"smtp", "resend", "slack", "twilio", "chat-webhook"} {
		got := string(providerIcon(id))
		if !strings.Contains(got, "<svg") {
			t.Errorf("providerIcon(%q) = %q, want an <svg ...> element", id, got)
		}
	}
}

// TestProviderIcon_EveryCatalogEntry guards that every provider in the
// catalog has a usable embedded icon — a missing file would silently degrade
// the drawer to placeholders.
func TestProviderIcon_EveryCatalogEntry(t *testing.T) {
	for _, p := range providerCatalog {
		if !strings.Contains(string(providerIcon(p.ID)), "<svg") {
			t.Errorf("providerIcon(%q) missing embedded svg for catalog entry %q", p.ID, p.Label)
		}
	}
}

// TestProviderIcon_SeparatorAgnostic guards the hyphen/underscore alias: the
// stored ProviderID "cloudflare-email" must resolve to the cloudflare_email.svg
// brand icon (the bug where it rendered a blank placeholder tile).
func TestProviderIcon_SeparatorAgnostic(t *testing.T) {
	got := string(providerIcon("cloudflare-email"))
	if !strings.Contains(got, "<svg") || !strings.Contains(got, "Cloudflare") {
		t.Errorf("providerIcon(%q) = %q, want the Cloudflare brand svg", "cloudflare-email", got)
	}
}

// TestProviderIcon_UnknownID returns a neutral placeholder (never errors,
// never panics) so an unmapped id can't break a render.
func TestProviderIcon_UnknownID(t *testing.T) {
	got := string(providerIcon("does-not-exist"))
	if strings.Contains(got, "<svg") {
		t.Errorf("providerIcon(unknown) = %q, want a non-svg placeholder", got)
	}
	if got == "" {
		t.Error("providerIcon(unknown) returned empty string; want a neutral placeholder span")
	}
}
