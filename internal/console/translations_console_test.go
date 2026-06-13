// translations_console_test.go proves the W4-L2 translations list + locale
// editor + settings console pages. Pattern follows layouts_console_test.go.
package console

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// ─── seed helper ──────────────────────────────────────────────────────────────

// seedLocale upserts each pair for locale within tc.scope.
func (tc *testConsole) seedLocale(t *testing.T, locale string, pairs map[string]string) {
	t.Helper()
	ctx := context.Background()
	for k, v := range pairs {
		if err := tc.st.Translations().Upsert(ctx, tc.scope, locale, k, v); err != nil {
			t.Fatalf("seed locale %s key %s: %v", locale, k, err)
		}
	}
}

// ─── page render ──────────────────────────────────────────────────────────────

func TestTranslations_PageRenderWithSeededData(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedLocale(t, "en", map[string]string{"greeting": "Hello"})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/translations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/translations: status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "en") {
		t.Errorf("translations page missing seeded locale 'en'")
	}
}

func TestTranslations_PageEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/translations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/translations: status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "No locales yet") {
		t.Errorf("empty translations page missing empty-state copy")
	}
}

func TestTranslations_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)

	rr := tc.get(t, "/console/translations", nil)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated /console/translations: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("redirect location = %q, want /console/login", got)
	}
}

func TestTranslations_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/translations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/translations: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/translations"`) {
		t.Errorf("translations page missing nav link")
	}
	if !strings.Contains(body, "is-active") {
		t.Errorf("translations page nav link not marked as active")
	}
}

// ─── fragment ─────────────────────────────────────────────────────────────────

func TestFragment_TranslationsList_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedLocale(t, "en", map[string]string{"k": "v"})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/translations-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("translations-list fragment: status = %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("translations-list fragment missing SSE event marker")
	}
}

func TestFragment_TranslationsList_SeededRows(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedLocale(t, "es", map[string]string{"k": "v"})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/translations-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("translations-list fragment: status = %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "es") {
		t.Errorf("translations-list fragment missing seeded locale")
	}
}

// ─── add-locale slide-over ────────────────────────────────────────────────────

func TestTranslations_NewRendersCreateForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.get(t, "/console/translations/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/translations/new: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-component="translation-create"`) {
		t.Errorf("/new missing create slide-over component")
	}
	if !strings.Contains(body, `action="/console/api/translations"`) {
		t.Errorf("/new missing create form action")
	}
	if !strings.Contains(body, csrf) {
		t.Errorf("/new create form missing csrf token")
	}
}

func TestTranslations_ListDoesNotRenderCreateForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/translations", cookie)
	if strings.Contains(rr.Body.String(), `data-component="translation-create"`) {
		t.Errorf("/console/translations should not render the create slide-over")
	}
}

func TestTranslations_ConfirmDeleteState(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedLocale(t, "en", map[string]string{"k": "v"})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/translations-list?confirmDelete=en", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Confirm delete") {
		t.Errorf("confirmDelete fragment missing confirm-row CTA")
	}
	if !strings.Contains(body, `action="/console/api/translations/en/delete"`) {
		t.Errorf("confirmDelete fragment missing delete POST form for locale")
	}
}

// ─── read-only gating ─────────────────────────────────────────────────────────

func TestTranslations_Create_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/translations",
		url.Values{"locale": {"en"}, "csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only create locale: status = %d, want 403", rr.Code)
	}
}

func TestTranslations_Save_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	tc.seedLocale(t, "en", map[string]string{"k": "v"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/translations/en/save",
		url.Values{"content": {`{"k":"x"}`}, "csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only save locale: status = %d, want 403", rr.Code)
	}
}

func TestTranslations_Delete_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	tc.seedLocale(t, "en", map[string]string{"k": "v"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/translations/en/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only delete locale: status = %d, want 403", rr.Code)
	}
}

// ─── mutations ────────────────────────────────────────────────────────────────

func TestTranslations_Create_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/translations",
		url.Values{"locale": {"de"}, "csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create locale: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got, want := rr.Header().Get("Location"), "/console/translations/de"; got != want {
		t.Errorf("create locale: Location = %q, want %q", got, want)
	}
}

func TestTranslations_Save_Persists(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/translations/en/save",
		url.Values{"content": {`{"greeting":"Hello","bye":"Bye"}`}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save locale: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got, want := rr.Header().Get("Location"), "/console/translations/en"; got != want {
		t.Errorf("save locale: Location = %q, want %q", got, want)
	}

	m, err := tc.st.Translations().Map(testBackground(), tc.scope, "en")
	if err != nil {
		t.Fatalf("map after save: %v", err)
	}
	if m["greeting"] != "Hello" || m["bye"] != "Bye" {
		t.Errorf("saved map = %v, want greeting/bye", m)
	}
}

func TestTranslations_Delete_Removes(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedLocale(t, "en", map[string]string{"k": "v"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/translations/en/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete locale: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got, want := rr.Header().Get("Location"), "/console/translations"; got != want {
		t.Errorf("delete locale: Location = %q, want %q", got, want)
	}

	rr2 := tc.get(t, "/console/api/fragments/translations-list", cookie)
	if strings.Contains(rr2.Body.String(), "translation-row-en") {
		t.Errorf("deleted locale still present in list fragment")
	}
}

func TestTranslations_Mutations_NonDatastarPostRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	cases := []struct {
		name string
		path func() string
		form url.Values
	}{
		{
			"create", func() string { return "/console/api/translations" },
			url.Values{"locale": {"mut-create"}, "csrf_token": {csrf}},
		},
		{"save", func() string {
			tc.seedLocale(t, "mutsave", map[string]string{"k": "v"})
			return "/console/api/translations/mutsave/save"
		}, url.Values{"content": {`{"k":"v2"}`}, "csrf_token": {csrf}}},
		{"delete", func() string {
			tc.seedLocale(t, "mutdelete", map[string]string{"k": "v"})
			return "/console/api/translations/mutdelete/delete"
		}, url.Values{"csrf_token": {csrf}}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rr := tc.postForm(t, tt.path(), tt.form, cookie)
			if rr.Code != http.StatusSeeOther {
				t.Fatalf("%s: status = %d, want 303 (body: %s)", tt.name, rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
				t.Errorf("%s: mutation answered SSE (%q) to a native form POST", tt.name, ct)
			}
			if body := rr.Body.String(); strings.Contains(body, "datastar-patch-elements") {
				t.Errorf("%s: mutation response carries a raw SSE patch body", tt.name)
			}
		})
	}
}

func TestTranslations_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	altScope := tc.altScope(t)

	tc.seedLocale(t, "mine", map[string]string{"k": "v"})
	if err := tc.st.Translations().Upsert(testBackground(), altScope, "theirs", "k", "v"); err != nil {
		t.Fatalf("seed alt locale: %v", err)
	}

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/translations-list", cookie)
	body := rr.Body.String()

	if !strings.Contains(body, "mine") {
		t.Errorf("translations list missing own locale 'mine'")
	}
	if strings.Contains(body, "theirs") {
		t.Errorf("translations list leaked alt-scope locale 'theirs'")
	}
}

func TestConsoleTranslations_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/console/translations"},
		{http.MethodGet, "/console/translations/new"},
		{http.MethodGet, "/console/translations/settings"},
		{http.MethodGet, "/console/translations/en"},
		{http.MethodPost, "/console/api/translations"},
		{http.MethodPost, "/console/api/translations/en/save"},
		{http.MethodPost, "/console/api/translations/en/delete"},
		{http.MethodPost, "/console/api/translations/settings"},
		{http.MethodGet, "/console/api/fragments/translations-list"},
	}
	for _, rt := range routes {
		resp := tc.get(t, rt.path, nil)
		if rt.method == http.MethodPost {
			resp = tc.postForm(t, rt.path, url.Values{}, nil)
		}
		if resp.Code != http.StatusSeeOther {
			t.Errorf("%s %s without session: status = %d, want 303", rt.method, rt.path, resp.Code)
			continue
		}
		if got := resp.Header().Get("Location"); got != "/console/login" {
			t.Errorf("%s %s: Location = %q, want /console/login", rt.method, rt.path, got)
		}
	}
}

func TestConsoleTemplates_NoNativeConfirm_Translations(t *testing.T) {
	for _, name := range []string{
		"templates/translations.html",
		"templates/translation_editor.html",
		"templates/translation_settings.html",
		"templates/partials/translations_tbody.html",
	} {
		b, err := templatesFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(b)
		if strings.Contains(body, "confirm(") {
			t.Errorf("%s uses native confirm()", name)
		}
		if strings.Contains(body, "alert(") {
			t.Errorf("%s uses native alert()", name)
		}
	}
}

// ─── settings ─────────────────────────────────────────────────────────────────

func TestTranslationSettings_PageRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/translations/settings", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET settings: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "Translation settings") {
		t.Errorf("settings page missing title")
	}
}

func TestTranslationSettings_Save_Persists(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/translations/settings",
		url.Values{"default_locale": {"en"}, "target_locales": {"es, fr"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save settings: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	got, err := tc.st.Environments().GetTranslationSettings(testBackground(), tc.scope)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if got.DefaultLocale != "en" {
		t.Errorf("default locale = %q, want en", got.DefaultLocale)
	}
	if len(got.TargetLocales) != 2 || got.TargetLocales[0] != "es" || got.TargetLocales[1] != "fr" {
		t.Errorf("target locales = %v, want [es fr]", got.TargetLocales)
	}

	// Reload the page: new values are visible.
	rr2 := tc.get(t, "/console/translations/settings", cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("reload settings: status = %d", rr2.Code)
	}
	body := rr2.Body.String()
	if !strings.Contains(body, `value="en"`) {
		t.Errorf("settings reload missing default locale value")
	}
	if !strings.Contains(body, "es, fr") {
		t.Errorf("settings reload missing target locales value")
	}
}

func TestTranslations_ScopeUsed(t *testing.T) {
	// guards against a handler ignoring scope: a save under tc.scope must not
	// be visible under an alt scope.
	tc := newTestConsole(t)
	altScope := tc.altScope(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/translations/en/save",
		url.Values{"content": {`{"k":"v"}`}, "csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d", rr.Code)
	}
	m, err := tc.st.Translations().Map(testBackground(), altScope, "en")
	if err != nil {
		t.Fatalf("map alt: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("alt scope leaked save: %v", m)
	}
}
