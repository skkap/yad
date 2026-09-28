package scripts

import (
	"strings"
	"testing"
)

// Which release scripts/breaking.sh compares against, in every topology it
// can meet (DEV-90).
//
// The repositories hold empty commits, so no tag carries the documents. The
// script then says, for each document, "does not exist at <tag>" and never
// reaches oasdiff. That line names the baseline it chose, and naming the
// baseline is the whole question here.
func TestTheBreakingCheckComparesAgainstTheReleaseBeforeThisOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		// build makes the history and returns the commit to check out.
		build func(r *repo) string
		// want is the baseline, or "" for a run that must say it is inert.
		want string
		// inert is a phrase the inert message must carry, so each absence
		// reads as the absence it is.
		inert string
	}{
		{
			name: "a release from the tip compares against the one before",
			build: func(r *repo) string {
				r.git("tag", "v0.1.0", r.commit("P"))
				r.git("tag", "v0.2.0", r.commit("D"))
				e := r.commit("E")
				r.git("tag", "v0.3.0", e)
				return e
			},
			want: "v0.2.0",
		},
		{
			// The topology DEV-90 is about: every tag contains HEAD, and
			// --no-contains alone left nothing to compare against.
			name: "a release from a strict ancestor of an earlier release compares against it",
			build: func(r *repo) string {
				r.git("tag", "v0.1.0", r.commit("P"))
				a := r.commit("A")
				r.git("tag", "v0.2.0", r.commit("D"))
				r.git("tag", "v0.3.0", a)
				return a
			},
			want: "v0.2.0",
		},
		{
			// DEV-90 as measured: no release below either, so --no-contains
			// excluded every tag and the check went inert.
			name: "a release from a strict ancestor, with nothing older, compares against the later release",
			build: func(r *repo) string {
				a := r.commit("A")
				r.git("tag", "v0.2.0", r.commit("D"))
				r.git("tag", "v0.3.0", a)
				return a
			},
			want: "v0.2.0",
		},
		{
			name: "an old commit older than every release is inert",
			build: func(r *repo) string {
				old := r.commit("O")
				r.git("tag", "v0.1.0", r.commit("P"))
				r.git("tag", "v0.2.0", r.commit("D"))
				return old
			},
			inert: "older than every release",
		},
		{
			// A bisect lands on a tagged commit as readily as on any other.
			// Picking the newest tag not on HEAD would compare v0.1.0 with
			// v0.2.0 and report everything 0.2.0 added as removed.
			name: "an old release checked out, with none before it, is inert",
			build: func(r *repo) string {
				p := r.commit("P")
				r.git("tag", "v0.1.0", p)
				r.git("tag", "v0.2.0", r.commit("D"))
				r.git("checkout", "-q", "--detach", p)
				return p
			},
			inert: "no earlier release",
		},
		{
			name: "an old release checked out compares against the one before it, as its own release did",
			build: func(r *repo) string {
				r.git("tag", "v0.0.9", r.commit("Q"))
				p := r.commit("P")
				r.git("tag", "v0.1.0", p)
				r.git("tag", "v0.2.0", r.commit("D"))
				return p
			},
			want: "v0.0.9",
		},
		{
			name: "an ordinary pull request compares against the newest release it builds on",
			build: func(r *repo) string {
				r.git("tag", "v0.1.0", r.commit("P"))
				r.git("tag", "v0.2.0", r.commit("D"))
				return r.commit("F")
			},
			want: "v0.2.0",
		},
		{
			// Newest-not-on-HEAD would pick v0.2.0 and fail the hotfix on
			// every field 0.2.0 added.
			name: "a patch release on a side branch compares against the release it patches",
			build: func(r *repo) string {
				p := r.commit("P")
				r.git("tag", "v0.1.0", p)
				r.git("tag", "v0.2.0", r.commit("D"))
				r.git("checkout", "-q", "--detach", p)
				h := r.commit("H")
				r.git("tag", "v0.1.1", h)
				return h
			},
			want: "v0.1.0",
		},
		{
			name: "the first release is inert",
			build: func(r *repo) string {
				p := r.commit("P")
				r.git("tag", "v0.1.0", p)
				return p
			},
			inert: "no earlier release",
		},
		{
			name: "a second tag on the same commit is not a baseline",
			build: func(r *repo) string {
				r.git("tag", "v0.1.0", r.commit("P"))
				d := r.commit("D")
				r.git("tag", "v0.2.0", d)
				r.git("tag", "v0.1.9", d)
				return d
			},
			want: "v0.1.0",
		},
		{
			name: "nothing tagged is inert",
			build: func(r *repo) string {
				return r.commit("P")
			},
			inert: "nothing has been released",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			r.git("checkout", "-q", "--detach", tc.build(r))
			out, code := r.run("breaking.sh")
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if tc.want != "" {
				for _, doc := range []string{"protocol/v1/openapi.yaml", "protocol/hubapi/openapi.yaml"} {
					if !strings.Contains(out, doc+" — INERT, the document does not exist at "+tc.want+";") {
						t.Errorf("%s was not compared against %s:\n%s", doc, tc.want, out)
					}
				}
				return
			}
			if !strings.Contains(out, "check-breaking: INERT") || strings.Contains(out, "does not exist at") {
				t.Errorf("chose a baseline where there is none:\n%s", out)
			}
			if !strings.Contains(out, tc.inert) {
				t.Errorf("the inert message does not say %q:\n%s", tc.inert, out)
			}
		})
	}
}
