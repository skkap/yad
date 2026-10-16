package hub

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"
)

func gitRun(id, session, url string) v1.Run {
	r := run(id, session)
	r.Sources = []v1.Source{{Git: &v1.GitSource{URL: url}}}
	return r
}

func answer(url string, class v1.SourceReachError, at time.Time) v1.SourceReach {
	return v1.SourceReach{URL: url, Reachable: class == "", Error: class, CheckedAt: at}
}

// yad hub lists the repositories of its recent runs to every runner that
// advertises source_probe, and to no other; leaves out one with a token in
// it; and keeps a runner's answers only while it lists the URL (decision
// 0088).
func TestTheHubListsItsRecentRepositories(t *testing.T) {
	f := newFixture(t)
	const a, b = "https://git.example/acme/a.git", "git@git.example:acme/b.git"
	f.enqueue(t, gitRun("r-a", "s-a", a), gitRun("r-b", "s-b", b),
		gitRun("r-tok", "s-tok", "https://x-access-token:ghs_secret@git.example/acme/c.git"),
		gitRun("r-a2", "s-a2", a))
	f.clock.Advance(time.Minute)
	f.enqueue(t, gitRun("r-b2", "s-b2", b))

	cred := f.register(t, "r1")
	res := f.mustSync(t, "r1", cred, first("r1", 0))
	if !slices.Equal(res.ProbeSources, []v1.ProbeSource{{URL: b}, {URL: a}}) {
		t.Fatalf("probe_sources = %+v; want b, the most recently used, then a, and nothing with a token", res.ProbeSources)
	}

	old := f.register(t, "r-old")
	if res := f.mustSync(t, "r-old", old, stale("r-old", 0)); res.ProbeSources != nil {
		t.Errorf("a runner without source_probe was sent %+v", res.ProbeSources)
	}

	at := f.clock.Now()
	r := req("r1", 0)
	r.SourceReach = []v1.SourceReach{answer(a, v1.ReachAuth, at), answer(b, "", at), answer("https://not.listed/x.git", "", at)}
	f.mustSync(t, "r1", cred, r)
	got, err := sourceReachOf(t.Context(), f.store.Queries, "r1")
	if err != nil {
		t.Fatal(err)
	}
	// Held in URL order: b's "git@" sorts before a's "https".
	if len(got) != 2 || got[1].URL != a || got[1].Error != v1.ReachAuth || got[0].URL != b || !got[0].Reachable {
		t.Fatalf("held = %+v; want a unreachable for auth and b reachable, and nothing for a URL not listed", got)
	}
	// An answer older than the one held, arriving late, does not replace it.
	r.SourceReach = []v1.SourceReach{answer(a, "", at.Add(-time.Minute))}
	f.mustSync(t, "r1", cred, r)
	if got, _ := sourceReachOf(t.Context(), f.store.Queries, "r1"); got[1].Reachable {
		t.Errorf("an older answer replaced a newer one: %+v", got[1])
	}

	// A week on, nothing is listed and every answer is forgotten.
	f.clock.Advance(probeWindow + time.Hour)
	if res := f.mustSync(t, "r1", cred, req("r1", 0)); res.ProbeSources != nil {
		t.Errorf("a week on, probe_sources = %+v", res.ProbeSources)
	}
	if got, _ := sourceReachOf(t.Context(), f.store.Queries, "r1"); len(got) != 0 {
		t.Errorf("answers about URLs no longer listed = %+v", got)
	}
}

// A run opening a session on a repository a runner cannot reach goes to one
// that can; a missing ref is the repository's, and routes nowhere; and the
// service API shows each runner's answers.
func TestARunGoesToARunnerThatCanReachItsRepository(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	const repo, empty = "https://git.example/acme/private.git", "https://git.example/acme/empty.git"
	f.enqueue(t, gitRun("seed", "s0", repo), gitRun("seed2", "s00", empty))
	f.clock.Advance(time.Second)

	locked, open := f.register(t, "locked"), f.register(t, "open")
	f.mustSync(t, "locked", locked, first("locked", 0))
	f.mustSync(t, "open", open, first("open", 0))
	at := f.clock.Now()
	lr := req("locked", 0)
	lr.SourceReach = []v1.SourceReach{answer(repo, v1.ReachAuth, at), answer(empty, v1.ReachRefNotFound, at)}
	f.mustSync(t, "locked", locked, lr)

	// The two seeds are offered to nobody: they only made the list.
	f.enqueue(t, gitRun("a", "s-a", repo), gitRun("e", "s-e", empty))
	lr = req("locked", 4)
	got := ids(f.mustSync(t, "locked", locked, lr).Runs)
	if slices.Contains(got, "a") || slices.Contains(got, "seed") {
		t.Errorf("offered %v to a runner that cannot reach %s", got, repo)
	}
	if !slices.Contains(got, "e") {
		t.Errorf("offered %v; a missing ref routes nowhere, so e is this runner's to take", got)
	}
	if got := ids(f.mustSync(t, "open", open, req("open", 4)).Runs); !slices.Contains(got, "a") {
		t.Errorf("offered %v to the runner that can reach it, want a among them", got)
	}

	var list hubapi.RunnerList
	if code, e := f.api(t, "GET", "/runners", tok, nil, &list); code != http.StatusOK {
		t.Fatalf("list: %d %+v", code, e)
	}
	for _, r := range list.Runners {
		if r.RunnerID != "locked" {
			continue
		}
		if len(r.SourceReach) != 2 || r.SourceReach[1].URL != repo || r.SourceReach[1].Error != "auth" || r.SourceReach[1].Reachable {
			t.Errorf("locked's source_reach = %+v", r.SourceReach)
		}
	}
}

func TestMaySecret(t *testing.T) {
	for url, want := range map[string]bool{
		"https://ghp_x@github.com/a/b.git":     true,
		"https://u:p@github.com/a/b.git":       true,
		"ssh://git:pw@github.com/a/b.git":      true,
		"ssh://git@github.com/a/b.git":         false,
		"git@github.com:a/b.git":               false,
		"https://github.com/a/b.git":           false,
		"https://github.com/a/b.git?x=a@b":     true,
		"https://github.com/a/b.git?token=x":   true,
		"https://github.com/a/b.git#x":         true,
		"/srv/repos/acme.git":                  false,
		"file:///srv/repos/acme.git":           false,
		"https://github.com/user@example/repo": false,
	} {
		if got := maySecret(url); got != want {
			t.Errorf("maySecret(%q) = %v, want %v", url, got, want)
		}
	}
}

// A week of runs each carrying its own token in the URL is a distinct URL
// per run; they are left out before the cap, so they cannot crowd out the
// plain repository named before them.
func TestTokenedURLsDoNotCrowdOutThePlainOnes(t *testing.T) {
	f := newFixture(t)
	const plain = "https://git.example/acme/plain.git"
	f.enqueue(t, gitRun("plain", "s-plain", plain))
	for i := range 3 * v1.ProbeSourcesMax {
		f.clock.Advance(time.Second)
		f.enqueue(t, gitRun(fmt.Sprintf("tok-%d", i), fmt.Sprintf("s-tok-%d", i),
			fmt.Sprintf("https://x-access-token:ghs_%d@git.example/acme/plain.git", i)))
	}
	cred := f.register(t, "r1")
	if got := f.mustSync(t, "r1", cred, first("r1", 0)).ProbeSources; !slices.Equal(got, []v1.ProbeSource{{URL: plain}}) {
		t.Errorf("probe_sources = %+v, want the plain URL alone", got)
	}
}
