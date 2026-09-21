package control

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// The refusal of a data directory others can reach names the chmod that
// closes it, and the directory comes from YAD_DATA_DIR, XDG_* or $HOME — none
// of them limited to shell-safe characters.
func TestOpenDirectoryChmodRunsAsPrinted(t *testing.T) {
	p := testPaths(t)
	p.Data = filepath.Join(p.Data, "a b'$c")
	if err := os.MkdirAll(p.Data, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := Claim(p)
	if err == nil {
		d.Close()
		t.Fatal("claimed a directory others can reach")
	}
	cmds := shellwordtest.Commands(err.Error(), "chmod ")
	if len(cmds) != 1 {
		t.Fatalf("want one chmod in %q", err)
	}
	shellwordtest.Check(t, cmds[0], "chmod", "700", p.Data)
}
