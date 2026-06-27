package console

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUIIcon_KnownName returns the embedded inline SVG for a ui-icon name,
// ready to inline in a template (currentColor stroke, inherits text colour).
func TestUIIcon_KnownName(t *testing.T) {
	for _, name := range []string{
		"x", "arrow-left", "arrow-right", "arrow-up-right",
		"arrow-up", "arrow-down", "unplug", "user", "message-circle",
		"star", "megaphone",
	} {
		got := string(uiIcon(name))
		if !strings.Contains(got, "<svg") {
			t.Errorf("uiIcon(%q) = %q, want an <svg ...> element", name, got)
		}
		if !strings.Contains(got, "currentColor") {
			t.Errorf("uiIcon(%q) = %q, want a currentColor-stroked icon", name, got)
		}
	}
}

// TestUIIcon_UnknownName returns a neutral placeholder (never errors, never
// panics) so an unmapped name can't break a render.
func TestUIIcon_UnknownName(t *testing.T) {
	got := string(uiIcon("does-not-exist"))
	if strings.Contains(got, "<svg") {
		t.Errorf("uiIcon(unknown) = %q, want a non-svg placeholder", got)
	}
	if got == "" {
		t.Error("uiIcon(unknown) returned empty string; want a neutral placeholder span")
	}
}

// TestUIIcon_PathTraversalGuarded rejects ids that try to escape the icon
// directory; they must fall back to the placeholder, never read other files.
func TestUIIcon_PathTraversalGuarded(t *testing.T) {
	for _, name := range []string{"", "../app", "a/b", "..\\x", "x."} {
		if strings.Contains(string(uiIcon(name)), "<svg") {
			t.Errorf("uiIcon(%q) returned an svg; want the placeholder", name)
		}
	}
}

// emojiInByte reports whether the rune at the given codepoint is a true emoji
// (the ranges the no-emoji rule hard-fails on).
func isEmojiCodepoint(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF: // emoji & symbols
		return true
	case r >= 0x2600 && r <= 0x27BF: // misc symbols + dingbats
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // misc symbols & arrows
		return true
	default:
		return false
	}
}

// isGlyphIconCodepoint reports whether r is one of the arrow/✕/× glyph icons
// the sweep replaced with SVGs. Flagged in rendered template bodies (outside
// Go template comments) so the no-glyph-icon rule can't silently regress.
func isGlyphIconCodepoint(r rune) bool {
	switch {
	case r >= 0x2190 && r <= 0x21FF: // arrows block (← → ↑ ↓ ↗ ⇄ ...)
		return true
	case r == 0x2715 || r == 0x2716: // ✕ ✖ multiplication x
		return true
	case r == 0x00D7: // × multiplication sign
		return true
	case r == 0x2605 || r == 0x2606: // ★ ☆ stars
		return true
	default:
		return false
	}
}

// stripTemplateComments removes {{/* ... */}} regions so glyphs that live only
// inside Go template comments (never rendered) don't trip the guard.
func stripTemplateComments(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "{{/*")
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		rest := s[i+4:]
		j := strings.Index(rest, "*/}}")
		if j < 0 {
			// Unterminated comment: drop the remainder.
			break
		}
		s = rest[j+4:]
	}
	return b.String()
}

// TestTemplates_NoEmojiOrGlyphIcons is the regression guard for the no-emoji
// rule: it scans every embedded template body (minus Go template comments) and
// hard-fails on any true emoji codepoint, plus the arrow/✕/×/★ glyph icons the
// SVG sweep removed.
func TestTemplates_NoEmojiOrGlyphIcons(t *testing.T) {
	err := fs.WalkDir(templatesFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".html") {
			return nil
		}
		body, err := fs.ReadFile(templatesFS, p)
		if err != nil {
			return err
		}
		rendered := stripTemplateComments(string(body))
		for _, r := range rendered {
			if isEmojiCodepoint(r) {
				t.Errorf("%s: contains emoji codepoint U+%04X (%q) — use {{ uiIcon \"...\" }} instead", p, r, string(r))
			}
			if isGlyphIconCodepoint(r) {
				t.Errorf("%s: contains glyph-icon codepoint U+%04X (%q) — use {{ uiIcon \"...\" }} instead", p, r, string(r))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
}

// TestTemplates_NoBrokenDatastarAttributeForms guards against the bug class the
// browser bug-hunt repeatedly surfaced: NON-REACTIVE Datastar attribute forms.
// In this Datastar build the bind/class/attr plugins are addressed with the
// value form (data-bind="signal") or the colon form (data-class:is-active,
// data-attr:aria-pressed). The hyphen-SUFFIX forms below silently do nothing:
//   - data-bind-wfFilter   -> the browser lowercases the attribute name to
//     data-bind-wffilter, yielding a 'wffilter' signal the server never reads
//     (this is why the workflows search box looked wired but never filtered).
//   - data-class-is-active -> the class plugin is never matched, so the active
//     state is never applied (the integration channel tabs had no selected look).
//   - data-attr-x          -> same parse failure as data-class-.
//
// These three substrings only ever appear in the broken form, so a plain
// Contains check is sufficient. NOTE: data-on-load / data-on-focus (hyphen) are
// intentionally NOT flagged — those work in this build; only bind/class/attr
// have the suffix-vs-colon hazard.
func TestTemplates_NoBrokenDatastarAttributeForms(t *testing.T) {
	broken := map[string]string{
		"data-bind-":  `use the value form data-bind="signalName"`,
		"data-class-": `use the colon form data-class:is-active`,
		"data-attr-":  `use the colon form data-attr:aria-pressed`,
	}
	err := fs.WalkDir(templatesFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".html") {
			return nil
		}
		body, err := fs.ReadFile(templatesFS, p)
		if err != nil {
			return err
		}
		rendered := stripTemplateComments(string(body))
		for bad, fix := range broken {
			if strings.Contains(rendered, bad) {
				t.Errorf("%s: contains non-reactive Datastar attribute form %q — %s", p, bad, fix)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
}

// TestGoSource_NoEmojiIconLiterals_Integrations is the regression guard for
// emoji that the template sweep can't catch: empty-state Icon values set in Go
// source (e.g. `Icon: "🔌"`). The empty-state partial renders Icon as text, so
// an emoji there violates the no-emoji rule just as it would in a template —
// but TestTemplates_NoEmojiOrGlyphIcons only scans .html bodies. This scans
// integrations.go's `Icon:` literals (the file the 🔌 sweep touched). Other
// console pages still carry emoji Icons tracked as their own cleanup; scoping
// here keeps this guard green while locking integrations' fix.
func TestGoSource_NoEmojiIconLiterals_Integrations(t *testing.T) {
	src, err := os.ReadFile(filepath.Clean("integrations.go"))
	if err != nil {
		t.Fatalf("read integrations.go: %v", err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "Icon:") {
			continue
		}
		for _, r := range line {
			if isEmojiCodepoint(r) {
				t.Errorf("integrations.go:%d: Icon literal contains emoji U+%04X (%q) — use a ui-icon name rendered via uiIcon instead: %s",
					i+1, r, string(r), strings.TrimSpace(line))
			}
		}
	}
}
