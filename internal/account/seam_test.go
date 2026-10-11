package account

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

// SecurityForTests names the program every Keychain delete runs, and makes
// any machine one that keeps Claude's logins in a Keychain. A shipped yad
// that set it would hand the Keychain's item names to whatever it named, on
// every removal and every token stored, so only a test may write it. The
// check is the source, parsed: any shipped file of the module that assigns
// it — alone, in a tuple, by range or in a var declaration, bare or as
// account.SecurityForTests — or takes its address, the file that declares it
// included, since a setter beside the declaration is exactly where someone
// would write one. A value reached some other way, through reflect or
// unsafe, is beyond what reading the source can see.
func TestOnlyTestsReachTheKeychainSeam(t *testing.T) {
	if SecurityForTests != "" {
		t.Fatalf("a test left the Keychain seam set to %q", SecurityForTests)
	}
	for _, at := range seamWrites(t, "../..", "SecurityForTests") {
		t.Errorf("%s writes SecurityForTests; only a test may point the Keychain at another program", at)
	}
}

// The guard has to see each way Go can write a package variable; a pattern
// over the text missed a tuple assignment and an address taken through the
// package name.
func TestTheKeychainSeamGuardSeesEveryWrite(t *testing.T) {
	writes := map[string]string{
		"assigned":            `SecurityForTests = "x"`,
		"assigned by package": `account.SecurityForTests = "x"`,
		"in a tuple":          `account.SecurityForTests, ok = lookup()`,
		"declared in a tuple": `SecurityForTests, ok := lookup()`,
		"its address":         `flag.StringVar(&account.SecurityForTests, "s", "", "")`,
		"its address, bare":   `p := &SecurityForTests`,
		"by range":            `for _, SecurityForTests = range names {}`,
		"appended to":         `SecurityForTests += "x"`,
	}
	reads := map[string]string{
		"compared":           `if SecurityForTests != "" {}`,
		"compared, equal":    `ok = account.SecurityForTests == ""`,
		"read into another":  `p := account.SecurityForTests`,
		"passed as a value":  `use(SecurityForTests)`,
		"another identifier": `NotSecurityForTests = "x"`,
	}
	for name, stmt := range writes {
		if got := seamWrites(t, srcDir(t, stmt), "SecurityForTests"); len(got) != 1 {
			t.Errorf("%s (%s): found %d writes, want 1", name, stmt, len(got))
		}
	}
	for name, stmt := range reads {
		if got := seamWrites(t, srcDir(t, stmt), "SecurityForTests"); len(got) != 0 {
			t.Errorf("%s (%s): found writes %q, want none", name, stmt, got)
		}
	}
}

// srcDir is a directory holding one shipped file whose one function body is
// stmt.
func srcDir(t *testing.T, stmt string) string {
	t.Helper()
	dir := t.TempDir()
	src := "package p\n\nfunc f() {\n\t" + stmt + "\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// seamWrites is where any non-test Go file under root writes the package
// variable name, as file:line.
func seamWrites(t *testing.T, root, name string) []string {
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
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
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
