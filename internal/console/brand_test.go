package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBrand_ProtocolWordmark pins notify's brand mark to dagnats's current
// idiom: the "noti.fy://" wordmark with an accented segment and a muted scheme
// tail, plus the collapsed-rail glyph.
//
// The negative half matters as much as the positive half. dagnats retired its
// solid dot (.console-mark) and pins that deletion with its own test; notify
// kept rendering one, so the two consoles disagreed about their shared family
// look and notify's stylesheet still carried the dot's rules. If a future
// change reintroduces the dot, the styling for it is gone and it renders as an
// unstyled artifact -- so assert it stays absent rather than trusting that
// nobody puts it back.
func TestBrand_ProtocolWordmark(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	req := httptest.NewRequest(http.MethodGet, "/console/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	wantWordmark := `<span class="console-name">noti` +
		`<span class="console-name-accent">.fy</span>` +
		`<span class="console-name-scheme">://</span></span>`
	for _, want := range []string{wantWordmark, `class="console-glyph"`} {
		if !strings.Contains(body, want) {
			t.Errorf("brand markup missing %q", want)
		}
	}
	if strings.Contains(body, `class="console-mark"`) {
		t.Error("sidebar still renders the retired dot (.console-mark)")
	}
}

// TestBrand_LoginPageMatchesSidebar keeps the two brand sites from drifting.
// The login page carries its own copy of the mark and was missed when the
// sidebar changed, which would have shipped an unstyled dot on the first screen
// an operator sees.
func TestBrand_LoginPageMatchesSidebar(t *testing.T) {
	tc := newTestConsole(t)

	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/console/login", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/login: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	if !strings.Contains(body, `<span class="console-name-accent">.fy</span>`) {
		t.Error("login page does not carry the noti.fy:// wordmark")
	}
	if strings.Contains(body, `class="console-mark"`) {
		t.Error("login page still renders the retired dot (.console-mark)")
	}
}

// TestBrand_FontIsBundledNotFetched guards the console's standing rule that the
// browser makes zero outbound requests at runtime. The wordmark face is inlined
// into app.css as a data URI; if someone "optimises" it into a separate file
// served from a URL, the console starts making a request it never made before
// and the fingerprinted-asset story quietly breaks.
func TestBrand_FontIsBundledNotFetched(t *testing.T) {
	tc := newTestConsole(t)

	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, assetURL("app.css"), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET app.css: status = %d, want 200", rr.Code)
	}
	css := rr.Body.String()

	if !strings.Contains(css, `font-family: "IoskeleyMono"`) {
		t.Error("app.css does not declare the IoskeleyMono face")
	}
	if !strings.Contains(css, "src: url(data:font/woff2;base64,") {
		t.Error("IoskeleyMono is not bundled as a data URI; the browser would have to fetch it")
	}
	if strings.Contains(css, ".woff2)") || strings.Contains(css, `.woff2")`) {
		t.Error("app.css references a font by URL; the face must be bundled")
	}
}
