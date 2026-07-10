package console

// L9 regression coverage: the copilot's guard-rail error messages ("Message
// is too long...", "Still generating...", "Prompt is too long...") must reach
// the wire as clean, unambiguous ASCII/UTF-8 - never mojibake (UTF-8 bytes
// for a typographic ellipsis/em-dash misread as Latin-1 and re-encoded,
// which renders as garbled two-byte sequences). The fix replaces the
// typographic ellipsis (U+2026) and em-dash (U+2014) in these SSE-carried
// messages with plain ASCII ("...", "-") so no decoder can mis-render them.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/danmestas/notify/internal/store"
)

// assertCleanSSEBytes fails the test if body is not valid UTF-8, or still
// carries the typographic ellipsis/em-dash characters the fix replaced with
// plain ASCII, so the guard-rail messages can never mis-encode regardless of
// what decodes the SSE stream.
func assertCleanSSEBytes(t *testing.T, body []byte, label string) {
	t.Helper()
	if !utf8.Valid(body) {
		t.Errorf("%s: response body is not valid UTF-8", label)
	}
	s := string(body)
	ellipsis := string(rune(0x2026))
	emDash := string(rune(0x2014))
	if strings.Contains(s, ellipsis) {
		t.Errorf("%s: response body still contains a typographic ellipsis (U+2026); body: %s", label, s)
	}
	if strings.Contains(s, emDash) {
		t.Errorf("%s: response body still contains a typographic em-dash (U+2014); body: %s", label, s)
	}
}

// TestAIGenerate_PromptTooLong_CleanUTF8 is the L9 regression for the
// ai-generate over-long-prompt guard rail.
func TestAIGenerate_PromptTooLong_CleanUTF8(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-1", nil, false, nil)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {strings.Repeat("a", aiPromptMaxLen+1)},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "too long") {
		t.Fatalf("expected too-long message; body: %s", rr.Body.String())
	}
	assertCleanSSEBytes(t, rr.Body.Bytes(), "ai-generate too-long")
}

// TestAIMessage_StillGenerating_CleanUTF8 is the L9 regression for the
// follow-up message "Still generating..." guard rail.
func TestAIMessage_StillGenerating_CleanUTF8(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-1", nil, false, nil)
	cookie, csrf := tc.login(t)

	sess, err := tc.st.BuilderSessions().Create(context.Background(), tc.scope, "seed prompt")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := tc.st.BuilderSessions().SetStatus(context.Background(), tc.scope, sess.ID, store.BuilderSessionGenerating); err != nil {
		t.Fatalf("set status generating: %v", err)
	}

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sess.ID+"/message", url.Values{
		"prompt":     {"another change"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Still generating") {
		t.Fatalf("expected still-generating message; body: %s", rr.Body.String())
	}
	assertCleanSSEBytes(t, rr.Body.Bytes(), "ai-message still-generating")
}

// TestAIMessage_TooLong_CleanUTF8 is the L9 regression for the follow-up
// message "Message is too long..." guard rail.
func TestAIMessage_TooLong_CleanUTF8(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-1", nil, false, nil)
	cookie, csrf := tc.login(t)

	sess, err := tc.st.BuilderSessions().Create(context.Background(), tc.scope, "seed prompt")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sess.ID+"/message", url.Values{
		"prompt":     {strings.Repeat("b", aiPromptMaxLen+1)},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "too long") {
		t.Fatalf("expected too-long message; body: %s", rr.Body.String())
	}
	assertCleanSSEBytes(t, rr.Body.Bytes(), "ai-message too-long")
}
