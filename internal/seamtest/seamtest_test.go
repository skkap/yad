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

// The walk reads everything this module could compile into yad: a test file
// sets the seam on purpose, and a nested module is another module — a main
// checkout holds whole worktrees under .claude, each with every test seam in
// it. A directory ./... leaves out is still read, because a shipped package can
// import it all the same.
func TestWritesReadsOnlyShippedFiles(t *testing.T) {
	write := body(`pkg.XForTests = 1`)
	for _, tc := range []struct {
		path string
		mod  string // a directory given a go.mod of its own
		want int
	}{
		{"p.go", "", 1},
		{"internal/deep/p.go", "", 1},
		{"cmd/yad/main.go", "", 1},
		{"testdata/p.go", "", 1},
		{"internal/deep/testdata/p.go", "", 1},
		{"_scratch/p.go", "", 1},
		{"internal/.hidden/p.go", "", 1},
		{".claude/p.go", "", 1},
		{"p_test.go", "", 0},
		{"internal/deep/p_test.go", "", 0},
		{"p.txt", "", 0},
		{".git/p.go", "", 0},
		{"nested/p.go", "nested", 0},
		{"nested/deep/p.go", "nested", 0},
		{".claude/worktrees/other/internal/p.go", ".claude/worktrees/other", 0},
	} {
		files := map[string]string{tc.path: write}
		if tc.mod != "" {
			files[tc.mod+"/go.mod"] = "module example.com/nested\n"
		}
		if got := Writes(t, tree(t, files), "XForTests"); len(got) != tc.want {
			t.Errorf("a write in %s: found %d %q, want %d", tc.path, len(got), got, tc.want)
		}
	}
}

// The root holds a go.mod of its own, and is walked: it is the module.
func TestWritesWalksTheRootModule(t *testing.T) {
	root := tree(t, map[string]string{
		"go.mod":          "module example.com/m\n",
		"p.go":            body(`XForTests = 1`),
		"internal/q/q.go": body(`XForTests = 2`),
	})
	if got := Writes(t, root, "XForTests"); len(got) != 2 {
		t.Errorf("found %d writes %q, want 2", len(got), got)
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
