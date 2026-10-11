package account

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// SecurityForTests names the program every Keychain delete runs, and makes
// any machine one that keeps Claude's logins in a Keychain. A shipped yad
// that set it would hand the Keychain's item names to whatever it named, on
// every removal and every token stored, so only a test may write it. The
// check is the source: any shipped file of the module that assigns it or
// takes its address, the file that declares it included — a setter beside
// the declaration is exactly where someone would write one. Matching the
// identifier is a backstop, not a proof.
func TestOnlyTestsReachTheKeychainSeam(t *testing.T) {
	if SecurityForTests != "" {
		t.Fatalf("a test left the Keychain seam set to %q", SecurityForTests)
	}
	const name = "SecurityForTests"
	written := regexp.MustCompile(`&\s*` + name + `\b|\b` + name + `\s*(?:[-+*/%|&^]|<<|>>)?=[^=]`)
	const root = "../.."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := written.FindIndex(raw); loc != nil {
			line := 1 + strings.Count(string(raw[:loc[0]]), "\n")
			t.Errorf("%s:%d assigns %s; only a test may point the Keychain at another program", path, line, name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
