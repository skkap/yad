package capability

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/hostool"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// The command a failing probe tells its owner to run is the binary that was
// probed. Found through YAD_<ID>_PATH, the plain name would run PATH's copy —
// or nothing, on the daemon the override exists for — so the command names
// the variable, never its value: a path under the owner's home may not leave
// the machine (DEV-67, DEV-108). Found on PATH, the plain name is right.
func TestAdviceRunsTheBinaryThatWasProbed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override bool
		// ran is what the pasted command must run.
		ran string
	}{
		{"found through the override", true, "the override"},
		{"found on PATH", false, "PATH's copy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noTools(t)
			dir := t.TempDir()
			pathDir := filepath.Join(dir, "path")
			t.Setenv("PATH", pathDir)
			overrides := map[string]string{}
			for _, e := range everyEntry() {
				write(t, filepath.Join(pathDir, e.binary), "#!/bin/sh\necho \"PATH's copy\"\nexit 3\n", 0o755)
				if tc.override {
					overrides[e.env] = write(t, filepath.Join(dir, e.id, e.binary+"-own"), "#!/bin/sh\necho 'the override'\nexit 3\n", 0o755)
					t.Setenv(e.env, overrides[e.env])
				}
			}
			said := map[string]string{}
			for _, d := range harness.Detect(context.Background()) {
				said["harness "+d.ID] = d.Error
			}
			for _, d := range hostool.Detect(context.Background()) {
				said["host tool "+d.ID] = d.Error
			}

			for _, e := range everyEntry() {
				key := e.kind + " " + e.id
				msg := said[key]
				prefix, args := e.binary+" ", strings.Fields(e.command)[1:]
				if tc.override {
					prefix = `"$` + e.env + `" `
				}
				cmds := shellwordtest.Commands(msg, prefix)
				if len(cmds) != 1 {
					t.Errorf("%s says %q, want one command starting %s", key, msg, prefix)
					continue
				}
				if strings.Contains(msg, dir) {
					t.Errorf("%s names a path on the machine: %q", key, msg)
				}
				if tc.override && !strings.Contains(msg, e.env+" set in that shell") {
					t.Errorf("%s says %q, want it to say %s must be set where it is pasted", key, msg, e.env)
				}
				// As parsed: the variable, standing in for a stub, is the
				// program, and the probe's arguments follow it unchanged. A
				// plain name is proved by the run below alone: some, such as
				// cursor-agent, cannot be a stub's name in sh.
				if tc.override {
					shellwordtest.Check(t, e.env+"=stub\n"+cmds[0], append([]string{"stub"}, args...)...)
				}

				// As run, with the variable set as the runner has it and PATH
				// holding another copy: the owner reaches the binary that failed.
				sh := exec.Command("/bin/sh", "-c", cmds[0])
				sh.Env = []string{"PATH=" + pathDir}
				if tc.override {
					sh.Env = append(sh.Env, e.env+"="+overrides[e.env])
				}
				out, _ := sh.Output()
				if got := strings.TrimSpace(string(out)); got != tc.ran {
					t.Errorf("%s: pasting `%s` ran %q, want %s", key, cmds[0], got, tc.ran)
				}
			}
		})
	}
}
