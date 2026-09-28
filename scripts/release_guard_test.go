package scripts

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// scripts/release-guard.sh refuses a release tagged on a commit an earlier
// release already contains, the topology DEV-90 found, and nothing else.
func TestTheReleaseGuardRefusesOnlyOlderCode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(r *repo) string // returns the tag being released
		code  int
		says  string
	}{
		{
			name: "a release from the tip",
			build: func(r *repo) string {
				r.git("tag", "v0.2.0", r.commit("D"))
				r.git("tag", "v0.3.0", r.commit("E"))
				return "v0.3.0"
			},
			says: "not older than any release",
		},
		{
			name: "a release from a strict ancestor of an earlier release",
			build: func(r *repo) string {
				a := r.commit("A")
				r.git("tag", "v0.2.0", r.commit("D"))
				r.git("tag", "v0.3.0", a)
				return "v0.3.0"
			},
			code: 1,
			says: "which v0.2.0",
		},
		{
			// Two tags pushed at once start two runs, and in the lower one's
			// run the higher tag already contains it.
			name: "a release a higher release already contains",
			build: func(r *repo) string {
				r.git("tag", "v0.2.0", r.commit("B"))
				r.git("tag", "v0.3.0", r.commit("C"))
				return "v0.2.0"
			},
			says: "not older than any release",
		},
		{
			name: "a patch release on a side branch",
			build: func(r *repo) string {
				p := r.commit("P")
				r.git("tag", "v0.1.0", p)
				r.git("tag", "v0.2.0", r.commit("D"))
				r.git("checkout", "-q", "--detach", p)
				r.git("tag", "v0.1.1", r.commit("H"))
				return "v0.1.1"
			},
			says: "not older than any release",
		},
		{
			name: "a second name for a released commit",
			build: func(r *repo) string {
				d := r.commit("D")
				r.git("tag", "v0.2.0", d)
				r.git("tag", "v1.0.0", d)
				return "v1.0.0"
			},
			says: "not older than any release",
		},
		{
			name: "the first release",
			build: func(r *repo) string {
				r.git("tag", "v0.1.0", r.commit("P"))
				return "v0.1.0"
			},
			says: "not older than any release",
		},
		{
			name: "no such tag",
			build: func(r *repo) string {
				r.commit("P")
				return "v9.9.9"
			},
			code: 2,
			says: "usage",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			out, code := r.run("release-guard.sh", tc.build(r))
			if code != tc.code || !strings.Contains(out, tc.says) {
				t.Errorf("exit %d, want %d, saying %q:\n%s", code, tc.code, tc.says, out)
			}
		})
	}
}

// The refusal's next action is pasted, so it must run as printed, for a tag
// name that needs quoting too. git accepts a quote and a dollar sign in a tag.
func TestTheReleaseGuardsNextActionRunsAsPrinted(t *testing.T) {
	for _, name := range []string{"v0.3.0", `v0.3.0-it's$HOME`} {
		t.Run(name, func(t *testing.T) {
			r := newRepo(t)
			a := r.commit("A")
			r.git("tag", "v0.2.0", r.commit("D"))
			r.git("tag", name, a)
			out, code := r.run("release-guard.sh", name)
			if code != 1 {
				t.Fatalf("exit %d, want 1:\n%s", code, out)
			}
			m := regexp.MustCompile("`(git tag -d [^`]*)`").FindStringSubmatch(out)
			if m == nil {
				t.Fatalf("no `git tag -d …` in:\n%s", out)
			}
			cmd := exec.Command("sh", "-c", m[1])
			cmd.Dir = r.dir
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", m[1], err, b)
			}
			if tags := r.git("tag", "--list", name); tags != "" {
				t.Errorf("%s left %q in place", m[1], tags)
			}
		})
	}
}
