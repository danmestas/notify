package console

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// assetManifest maps a logical asset name (e.g. "app.css") to its
// content-fingerprinted URL path (e.g. "/console/assets/app.a1b2c3d4.css").
// Initialized at var-declaration time (not in init()) so that other package-
// level vars can reference it safely — Go evaluates var initializers in
// dependency order before any init() runs.  The URLs are stable for the
// lifetime of the process and change only when the embedded content changes,
// making "Cache-Control: immutable" correct and safe.
var assetManifest = buildAssetManifest()

// buildAssetManifest reads every embedded asset, sha256-hashes its bytes, and
// builds the logical→fingerprinted-URL map.  It panics on any error because a
// missing or unreadable asset is a programmer error caught at startup, not at
// request time.
func buildAssetManifest() map[string]string {
	// logical name → embedded filename (same as the file on disk)
	assets := map[string]string{
		"app.css":     "assets/app.css",
		"datastar.js": "assets/datastar.js.gz", // gzip-on-disk; logical name is .js
		"theme.js":    "assets/theme.js",
	}
	m := make(map[string]string, len(assets))
	for logical, embedPath := range assets {
		body, err := fs.ReadFile(assetsFS, embedPath)
		if err != nil {
			panic(fmt.Sprintf("assetManifest: read %s: %v", embedPath, err))
		}
		sum := sha256.Sum256(body)
		hash := fmt.Sprintf("%x", sum[:6]) // 12 hex chars = 48-bit collision-resistance
		m[logical] = fingerprintedURL(logical, hash)
	}
	return m
}

// fingerprintedURL converts a logical asset name and a hex hash into the
// versioned URL path.  "app.css" + "a1b2c3d4e5f6" →
// "/console/assets/app.a1b2c3d4e5f6.css".
func fingerprintedURL(logical, hash string) string {
	ext := path.Ext(logical)
	base := strings.TrimSuffix(logical, ext)
	return "/console/assets/" + base + "." + hash + ext
}

// assetURL returns the fingerprinted URL for the given logical asset name.
// It returns "" for unknown names so templates can branch on truthiness and
// a bad name never silently serves a broken link.
func assetURL(logical string) string {
	return assetManifest[logical]
}
