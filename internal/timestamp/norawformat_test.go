package timestamp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoRawRFC3339NanoFormatting keeps the fix from eroding.
//
// The ordering property only holds if EVERY writer uses the fixed-width layout.
// A single `t.Format(time.RFC3339Nano)` reintroduced anywhere silently puts a
// variable-width value back into a column that is compared as a string, and
// nothing fails until pagination skips a row. Formatting is therefore banned
// outside this package.
//
// Parsing with time.RFC3339Nano is fine and deliberately not flagged: it accepts
// both widths, which is what reading legacy rows requires.
func TestNoRawRFC3339NanoFormatting(t *testing.T) {
	root := filepath.Join("..", "..")

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// This package defines the layout; it necessarily names RFC3339Nano.
		if strings.Contains(filepath.ToSlash(path), "internal/timestamp/") {
			return nil
		}

		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "Format(time.RFC3339Nano)") {
				t.Errorf("%s:%d formats with time.RFC3339Nano -- use timestamp.Now or timestamp.Format.\n"+
					"RFC3339Nano drops the fraction on whole seconds, which breaks string "+
					"comparison of timestamp columns (keyset pagination, ORDER BY created_at).\n"+
					"    %s", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
}
