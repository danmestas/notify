// assets_test.go pins the content-hashed (fingerprinted) asset URL contract.
// The tests specifically cover the bug class from issue #58: fixed URLs with
// immutable cache headers cause browsers to serve stale CSS indefinitely.
// A content-hashed URL changes when the asset changes, making immutable-cache
// correct and safe.
package console

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// hashSegmentRE matches a hex content hash embedded in a fingerprinted URL,
// e.g. "a1b2c3d4e5f6" in "/console/assets/app.a1b2c3d4e5f6.css".
var hashSegmentRE = regexp.MustCompile(`/console/assets/[^.]+\.[0-9a-f]{12}\.[a-z]+$`)

func TestAssetManifest_FingerprintedURLsContainHash(t *testing.T) {
	for _, logical := range []string{"app.css", "datastar.js", "theme.js"} {
		url := assetManifest[logical]
		if url == "" {
			t.Errorf("assetManifest[%q] is empty", logical)
			continue
		}
		if !hashSegmentRE.MatchString(url) {
			t.Errorf("assetManifest[%q] = %q does not match fingerprinted URL pattern", logical, url)
		}
		// The bare unversioned path must NOT be in the manifest value.
		bare := "/console/assets/" + logical
		if url == bare {
			t.Errorf("assetManifest[%q] = %q is bare (not fingerprinted)", logical, url)
		}
	}
}

func TestAssetManifest_DifferentContentProducesDifferentURLs(t *testing.T) {
	// app.css and datastar.js have different content; their hashed URLs must differ.
	cssURL := assetManifest["app.css"]
	jsURL := assetManifest["datastar.js"]
	if cssURL == "" || jsURL == "" {
		t.Fatal("manifest missing entries")
	}
	if cssURL == jsURL {
		t.Errorf("app.css and datastar.js produced the same fingerprinted URL %q — hash is not content-sensitive", cssURL)
	}
}

func TestAssetManifest_URLsAreStable(t *testing.T) {
	// Calling assetURL twice must return the same value — no randomness.
	for _, logical := range []string{"app.css", "datastar.js", "theme.js"} {
		first := assetURL(logical)
		second := assetURL(logical)
		if first != second {
			t.Errorf("assetURL(%q) returned different values on two calls: %q vs %q", logical, first, second)
		}
	}
}

func TestAssetRoutes_HashedURLReturns200WithImmutableCache(t *testing.T) {
	tc := newTestConsole(t)

	for logical, contentType := range map[string]string{
		"app.css":  "text/css",
		"theme.js": "application/javascript",
	} {
		url := assetManifest[logical]
		rr := tc.get(t, url, nil)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: hashed URL %q: status = %d, want 200", logical, url, rr.Code)
			continue
		}
		if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, contentType) {
			t.Errorf("%s: Content-Type = %q, want prefix %q", logical, got, contentType)
		}
		if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s: Cache-Control = %q, want immutable", logical, cc)
		}
	}

	// datastar.js is gzip-on-disk; verify Content-Encoding too.
	dsURL := assetManifest["datastar.js"]
	rr := tc.get(t, dsURL, nil)
	if rr.Code != http.StatusOK {
		t.Errorf("datastar.js: hashed URL %q: status = %d, want 200", dsURL, rr.Code)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("datastar.js: Content-Encoding = %q, want gzip", got)
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("datastar.js: Cache-Control = %q, want immutable", cc)
	}
}

func TestAssetRoutes_BareUnversionedURLIsNotFound(t *testing.T) {
	tc := newTestConsole(t)
	// The old bare paths must NOT be registered — they would be served with
	// immutable cache but the URL never changes, which is the exact bug this
	// fix addresses.
	for _, bare := range []string{
		"/console/assets/app.css",
		"/console/assets/datastar.js",
		"/console/assets/theme.js",
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, bare, nil)
		tc.h.ServeHTTP(rr, req)
		// The bare path should NOT return 200. It will either 303 (session gate,
		// since no route matches and NotFound uses sessionMiddleware) or 404.
		// Either way, not 200.
		if rr.Code == http.StatusOK {
			t.Errorf("bare URL %q returned 200 — the unversioned cache-busting bug is still present", bare)
		}
	}
}

// TestAssets_AnalyticsRunsLineCSSIsVisible guards against QA finding M2: the
// Workflow Runs Trend chart's <polyline class="analytics-runs-line"> was in
// the DOM but invisible because .analytics-runs-line had no CSS rule at all,
// so it fell back to the SVG default `stroke: none`. Its siblings
// .analytics-trend-line and .analytics-failed-line each set an explicit
// stroke color; .analytics-runs-line must too, and it must use a distinct
// color from the trend line so the two lines are visually separable.
func TestAssets_AnalyticsRunsLineCSSIsVisible(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, assetManifest["app.css"], nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET app.css: status = %d, want 200", rr.Code)
	}
	css := rr.Body.String()

	runsRE := regexp.MustCompile(`(?s)\.analytics-runs-line\s*\{([^}]*)\}`)
	m := runsRE.FindStringSubmatch(css)
	if m == nil {
		t.Fatal(".analytics-runs-line rule not found in app.css — the Workflow Runs Trend chart renders invisible (stroke: none)")
	}
	rule := m[1]
	if !strings.Contains(rule, "stroke:") || strings.Contains(rule, "stroke: none") {
		t.Errorf(".analytics-runs-line rule does not set a visible stroke: %q", rule)
	}

	trendRE := regexp.MustCompile(`(?s)\.analytics-trend-line\s*\{([^}]*)\}`)
	trendMatch := trendRE.FindStringSubmatch(css)
	if trendMatch == nil {
		t.Fatal(".analytics-trend-line rule not found in app.css (sibling rule expected to exist)")
	}
	strokeRE := regexp.MustCompile(`stroke:\s*([^;]+);`)
	runsStroke := strokeRE.FindStringSubmatch(rule)
	trendStroke := strokeRE.FindStringSubmatch(trendMatch[1])
	if runsStroke == nil || trendStroke == nil {
		t.Fatalf("could not parse stroke values: runs=%v trend=%v", runsStroke, trendStroke)
	}
	if runsStroke[1] == trendStroke[1] {
		t.Errorf(".analytics-runs-line stroke (%q) must be visually distinct from .analytics-trend-line stroke (%q)", runsStroke[1], trendStroke[1])
	}
}

// TestAssets_TopbarTitleTruncates guards against a browser-verified overflow
// bug: a page title built from long user data (e.g. a subscriber with a
// ~200-char last name) does not wrap or truncate in the sticky
// .console-topbar-title, causing real horizontal page overflow that pushes
// the topbar's search box, environment switcher, and theme toggle off-screen
// and inaccessible. A prior fix added overflow-wrap to .page-header h1 and
// .wf-meta-row dd (the main card body) but missed the topbar title. Unlike
// the card body, the topbar title must truncate (not wrap) because it lives
// in a fixed-height sticky flex row alongside the search/env/theme controls.
func TestAssets_TopbarTitleTruncates(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, assetManifest["app.css"], nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET app.css: status = %d, want 200", rr.Code)
	}
	css := rr.Body.String()

	titleRE := regexp.MustCompile(`(?s)\.console-topbar-title\s*\{([^}]*)\}`)
	m := titleRE.FindStringSubmatch(css)
	if m == nil {
		t.Fatal(".console-topbar-title rule not found in app.css")
	}
	rule := m[1]
	for _, want := range []string{"overflow: hidden", "text-overflow: ellipsis", "white-space: nowrap", "min-width: 0"} {
		if !strings.Contains(rule, want) {
			t.Errorf(".console-topbar-title rule does not set %q — long titles will overflow the sticky topbar and push the search/env/theme controls off-screen: %q", want, rule)
		}
	}
}

func TestLayoutHTML_EmitsHashedAssetURLs(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	// GET the dashboard while authenticated; parse the <head> for asset hrefs.
	rr := tc.get(t, "/console/", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// The rendered HTML must contain the fingerprinted CSS URL.
	cssURL := assetManifest["app.css"]
	if !strings.Contains(body, cssURL) {
		t.Errorf("layout HTML does not reference fingerprinted CSS URL %q", cssURL)
	}
	// The bare (unversioned) CSS path must NOT appear.
	if strings.Contains(body, `href="/console/assets/app.css"`) {
		t.Errorf("layout HTML still contains bare unversioned href='/console/assets/app.css'")
	}

	// datastar.js is imported inside a <script type="module"> block; html/template
	// applies JS string escaping so forward slashes become \/.  Check for the
	// hash segment instead of the full URL literal.
	dsURL := assetManifest["datastar.js"]
	// Extract the hash-bearing filename (e.g. "datastar.7eab056d9593.js").
	dsFilename := dsURL[len("/console/assets/"):]
	if !strings.Contains(body, dsFilename) {
		t.Errorf("layout HTML does not reference fingerprinted datastar.js filename %q (full URL %q)", dsFilename, dsURL)
	}
	if strings.Contains(body, `"datastar.js"`) && !strings.Contains(body, dsFilename) {
		t.Errorf("layout HTML still contains bare unversioned 'datastar.js' without a hash")
	}

	// Same for theme.js.
	themeURL := assetManifest["theme.js"]
	if !strings.Contains(body, themeURL) {
		t.Errorf("layout HTML does not reference fingerprinted theme.js URL %q", themeURL)
	}
	if strings.Contains(body, `"/console/assets/theme.js"`) {
		t.Errorf("layout HTML still contains bare unversioned '/console/assets/theme.js'")
	}
}
