package console

import "testing"

// TestAssetCacheHeader_DevMode pins the asset Cache-Control policy now that it
// is driven by an injected flag (Deps.DevMode) rather than an init-time
// os.Getenv global: no-store while developing, long+immutable in production.
func TestAssetCacheHeader_DevMode(t *testing.T) {
	if got := assetCacheHeader(true); got != "no-store, must-revalidate" {
		t.Fatalf("assetCacheHeader(true) = %q, want no-store, must-revalidate", got)
	}
	if got := assetCacheHeader(false); got != "public, max-age=31536000, immutable" {
		t.Fatalf("assetCacheHeader(false) = %q, want public, max-age=31536000, immutable", got)
	}
}
