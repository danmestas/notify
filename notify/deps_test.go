package notify_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNotify_ReadsNoProcessState pins the package boundary: the host
// interprets flags and the environment, so package notify imports neither
// flag nor cmd/..., and never reads environment variables itself.
func TestNotify_ReadsNoProcessState(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "flag" || strings.Contains(path, "/cmd/") {
				t.Errorf("%s imports %q", name, path)
			}
		}
		ast.Inspect(f, func(node ast.Node) bool {
			sel, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" {
				switch sel.Sel.Name {
				case "Getenv", "LookupEnv", "Environ":
					t.Errorf("%s: os.%s at %s", name, sel.Sel.Name, fset.Position(sel.Pos()))
				}
			}
			return true
		})
	}
}
