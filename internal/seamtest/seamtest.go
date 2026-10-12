// Package seamtest finds where shipped code writes a test seam.
//
// A seam is a package variable a test sets to make a runner faster or point it
// at a fake — a sync floor, a lease, a timeout, the program the Keychain runs.
// A shipped yad that wrote one would still pass every test the seam exists for,
// because the code still works; it is the machine running yad that pays. So
// the only thing that stands between a seam and a shipped binary is a test that
// reads the source, and it has to read it as Go: a pattern over the text missed
// a tuple assignment and an address taken through the package name (YAD-191).
package seamtest

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Writes is where any shipped Go file under root writes the package variable
// name, as file:line. A shipped file is any .go file but a _test.go one, under
// root but outside .git and outside any directory holding its own go.mod. The
// go tool leaves testdata and "." and "_" directories out of ./..., and still
// builds any of them a shipped package imports, so they are read. A nested
// module is another module, compiled in only if go.mod requires it — and a
// main checkout carries whole other branches under .claude/worktrees, each a
// module of its own.
//
// A write is an assignment of any kind — alone, in a tuple, with an operator,
// by := — a range key or value, a var declared with a value, ++ or --, or the
// variable's address taken, the name bare or behind a package. The file that
// declares the seam is not exempt: a setter beside the declaration is exactly
// where someone would write one. Any identifier or field spelled name counts —
// the walk does not type-check, so a write it cannot tell from the seam's is
// reported rather than let through. A value reached some other way, through
// reflect or unsafe, is beyond what reading the source can see.
func Writes(t testing.TB, root, name string) []string {
	t.Helper()
	named := func(e ast.Expr) bool {
		switch e := ast.Unparen(e).(type) {
		case *ast.Ident:
			return e.Name == name
		case *ast.SelectorExpr:
			return e.Sel.Name == name
		}
		return false
	}
	var found []string
	fset := gotoken.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			var lhs []ast.Expr
			switch n := n.(type) {
			case *ast.AssignStmt:
				lhs = n.Lhs
			case *ast.IncDecStmt:
				lhs = []ast.Expr{n.X}
			case *ast.RangeStmt:
				lhs = []ast.Expr{n.Key, n.Value}
			case *ast.ValueSpec:
				if len(n.Values) > 0 {
					for _, id := range n.Names {
						lhs = append(lhs, id)
					}
				}
			case *ast.UnaryExpr:
				if n.Op == gotoken.AND {
					lhs = []ast.Expr{n.X}
				}
			}
			for _, e := range lhs {
				if e != nil && named(e) {
					found = append(found, fset.Position(e.Pos()).String())
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}
