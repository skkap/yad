package shellword_test

import (
	"testing"

	"github.com/skkap/yad/internal/shellword"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// An expanded variable is exactly one argument whatever it holds, and as the
// program it runs what the variable names.
func TestExpandIsReadBackByARealShell(t *testing.T) {
	values := append([]string{"plain", "/Users/someone/bin/tool-2.45"}, shellwordtest.Hostile...)
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			set := "YAD_X_PATH=" + shellword.Quote(v) + "\n"
			shellwordtest.Check(t, set+"prog "+shellword.Expand("YAD_X_PATH")+" --flag", "prog", v, "--flag")
		})
	}
	shellwordtest.Check(t, "YAD_X_PATH=prog\n"+shellword.Expand("YAD_X_PATH")+" --version", "prog", "--version")
}

// A name that is not a variable's would be a command that means something
// else, so it is refused where it is written.
func TestExpandRefusesWhatIsNotAName(t *testing.T) {
	for _, name := range []string{"", "1X", "X-Y", "X Y", "X$(id)", "X}"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Expand(%q) did not panic", name)
				}
			}()
			shellword.Expand(name)
		}()
	}
}
