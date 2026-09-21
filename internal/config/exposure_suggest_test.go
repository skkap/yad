//go:build unix

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// Every chmod an exposure warning offers runs as printed on the path it names,
// whatever the profile's directories are called: they come from YAD_*_DIR,
// XDG_* or $HOME, and none of those is constrained to shell-safe characters.
func TestExposureCommandsRunAsPrinted(t *testing.T) {
	old := geteuid
	geteuid = os.Getuid
	t.Cleanup(func() { geteuid = old })
	for _, name := range []string{"My Disk", "$HOME", "it's", "a;b", "back`tick", "~"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			p := Paths{Profile: DefaultProfile, Config: dir, Data: dir}
			cred := filepath.Join(dir, "credentials", "yashiki")
			write(t, cred, 0o644)
			modes := map[string]string{dir: "700", cred: "600"}
			seen := map[string]bool{}
			for _, line := range Exposures(p) {
				for _, cmd := range shellwordtest.Commands(line, "chmod ") {
					calls := shellwordtest.Run(t, cmd, "chmod")
					if len(calls) != 1 || len(calls[0]) != 3 || modes[calls[0][2]] != calls[0][1] {
						t.Errorf("sh ran\n  %s\nas %q, want one chmod of a path the warnings name", cmd, calls)
						continue
					}
					seen[calls[0][2]] = true
				}
			}
			if !seen[dir] || !seen[cred] {
				t.Errorf("want a backticked chmod of both %s and %s:\n%s", dir, cred, strings.Join(Exposures(p), "\n"))
			}
		})
	}
}
