package seamtest

import (
	"os"
	"path/filepath"
	"testing"
)

// The guard has to see each way Go can write a package variable. The first two
// are what the pattern over the text it replaced let through.
func TestWritesSeesEveryWrite(t *testing.T) {
	writes := map[string]string{
		"its address, by package": `flag.DurationVar(&pkg.XForTests, "x", 0, "")`,
		"in a tuple, by package":  `pkg.XForTests, ok = lookup()`,
		"assigned":                `XForTests = 1`,
		"assigned by package":     `pkg.XForTests = 1`,
		"assigned in parentheses": `(pkg.XForTests) = 1`,
		"declared in a tuple":     `XForTests, ok := lookup()`,
		"its address, bare":       `p := &XForTests`,
		"by range, as the value":  `for _, pkg.XForTests = range xs {}`,
		"by range, as the key":    `for XForTests = range xs {}`,
		"declared with a value":   `var XForTests = 1`,
		"in a tuple declaration":  `var ok, XForTests = lookup()`,
		"added to":                `XForTests += 1`,
		"shifted":                 `pkg.XForTests <<= 1`,
		"incremented":             `pkg.XForTests++`,
		"decremented":             `XForTests--`,
	}
	reads := map[string]string{
		"compared":                  `if XForTests != 0 {}`,
		"compared, equal":           `ok = pkg.XForTests == 0`,
		"read into another":         `p := pkg.XForTests`,
		"passed as a value":         `use(XForTests)`,
		"declared without a value":  `var XForTests int`,
		"another identifier":        `NotXForTests = 1`,
		"another identifier's addr": `p := &pkg.NotXForTests`,
	}
	for name, stmt := range writes {
		if got := Writes(t, tree(t, map[string]string{"p.go": body(stmt)}), "XForTests"); len(got) != 1 {
			t.Errorf("%s (%s): found %d writes %q, want 1", name, stmt, len(got), got)
		}
	}
	for name, stmt := range reads {
		if got := Writes(t, tree(t, map[string]string{"p.go": body(stmt)}), "XForTests"); len(got) != 0 {
			t.Errorf("%s (%s): found writes %q, want none", name, stmt, got)
		}
	}
}

// A package-level write runs before main, in every binary that imports it.
func TestWritesSeesAWriteOutsideAFunction(t *testing.T) {
	for name, src := range map[string]string{
		"a var's initialiser": "package p\n\nvar _ = func() int { XForTests = 1; return 0 }()\n",
		"an init":             "package p\n\nfunc init() { pkg.XForTests = 1 }\n",
		"a package var":       "package p\n\nvar p = &XForTests\n",
	} {
		if got := Writes(t, tree(t, map[string]string{"p.go": src}), "XForTests"); len(got) != 1 {
			t.Errorf("%s: found %d writes %q, want 1", name, len(got), got)
		}
	}
}

// The walk reads what the module ships and nothing else: a test file sets the
// seam on purpose, and a directory the go tool leaves out is not this module's
// code. A main checkout holds whole worktrees under .claude, each with every
// test seam in it.
func TestWritesReadsOnlyShippedFiles(t *testing.T) {
	write := body(`pkg.XForTests = 1`)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"p.go", 1},
		{"internal/deep/p.go", 1},
		{"cmd/yad/main.go", 1},
		{"p_test.go", 0},
		{"internal/deep/p_test.go", 0},
		{"testdata/p.go", 0},
		{"internal/deep/testdata/p.go", 0},
		{".claude/worktrees/other/p.go", 0},
		{"_scratch/p.go", 0},
		{"nested/p.go", 0},
		{"p.txt", 0},
	} {
		files := map[string]string{tc.path: write}
		if tc.path == "nested/p.go" {
			files["nested/go.mod"] = "module example.com/nested\n"
		}
		if got := Writes(t, tree(t, files), "XForTests"); len(got) != tc.want {
			t.Errorf("a write in %s: found %d %q, want %d", tc.path, len(got), got, tc.want)
		}
	}
}

// The root itself is walked whatever it is called: a test's temporary
// directory, or a checkout someone cloned into a directory starting with a dot.
func TestWritesWalksARootTheGoToolWouldSkip(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".checkout")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte(body(`XForTests = 1`)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Writes(t, root, "XForTests"); len(got) != 1 {
		t.Errorf("found %d writes %q, want 1", len(got), got)
	}
}

// body is a Go file whose one function body is stmt.
func body(stmt string) string {
	return "package p\n\nfunc f() {\n\t" + stmt + "\n}\n"
}

// tree is a temporary directory holding files, by slash-separated path.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, src := range files {
		path = filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
