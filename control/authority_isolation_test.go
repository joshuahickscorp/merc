package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Isolation between the measurement authorities and the decision paths is
// enforced in the build, not in a comment. This is the shared machinery.
//
// The first version of these checks used substring matching over file text,
// which was wrong in both directions: it flagged a doc comment that merely
// EXPLAINED the separation, and it would have missed a read reached through a
// dot-import or an aliased identifier. Parsing gives the right question — does
// this file's CODE name the symbol — instead of the approximate one.
//
// It is still not a call-graph. A guarded file that calls a helper in an allowed
// file which itself reads the table would pass. That is a known limit, stated
// here rather than left for someone to discover: the allowlists are small and
// deliberately reviewed, and the day one of them grows a helper worth calling
// from a money path is the day this needs a real reachability check.

// codeReferences reports which of the given names appear in a Go file's code —
// identifiers, selectors, or string literals — ignoring comments entirely.
func codeReferences(t *testing.T, path string, names []string) []string {
	t.Helper()
	fset := token.NewFileSet()
	// No parser.ParseComments: comments are not code, and a file that documents
	// why it must not read something is doing the right thing.
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.Ident:
			if wanted[node.Name] {
				found[node.Name] = true
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			// SQL lives in string literals, so a table name is a real reference.
			for name := range wanted {
				if strings.Contains(node.Value, name) {
					found[name] = true
				}
			}
		}
		return true
	})
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	return out
}

// The parser-based check must be strictly better than the substring one it
// replaced: blind to comments, and still able to see a table named only inside
// a SQL string.
func TestCodeReferencesIgnoresCommentsAndSeesSQLStrings(t *testing.T) {
	// execution_overhead.go documents plan_actuals in prose and must not read it.
	if refs := codeReferences(t, "execution_overhead.go", []string{"plan_actuals"}); len(refs) != 0 {
		t.Errorf("a doc-comment mention was counted as a reference: %v", refs)
	}
	// plan_actuals.go names the table only inside SQL, and that must be seen.
	if refs := codeReferences(t, "plan_actuals.go", []string{"plan_actuals"}); len(refs) == 0 {
		t.Error("a table named inside a SQL string literal was not seen as a reference")
	}
}
