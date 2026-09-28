package hub

import (
	"slices"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// offRunner is the first sync of a runner whose owner set path_sources =
// false.
func offRunner(id string, free int) v1.SyncRequest {
	r := first(id, free)
	r.Capabilities.PathSources = new(false)
	return r
}

// A run opening a session with a source on the machine is not offered to a
// runner that would refuse it, and waits for one that takes it; a run whose
// sources are all on the network goes to either (decision 0061).
func TestALocalSourceIsNotOfferedToARunnerWithPathSourcesOff(t *testing.T) {
	f := newFixture(t)
	off := f.register(t, "off")
	withPath := run("path", "s1")
	withPath.Sources = []v1.Source{{Path: "/home/me/src/app"}}
	localGit := run("local", "s2")
	localGit.Sources = []v1.Source{{Git: &v1.GitSource{URL: "FILE:///srv/git/app.git"}}}
	remote := run("remote", "s3")
	remote.Sources = []v1.Source{{Git: &v1.GitSource{URL: "https://github.com/skkap/yad.git"}}}
	f.enqueue(t, withPath, localGit, remote, run("plain", "s4"))

	got := ids(f.mustSync(t, "off", off, offRunner("off", 4)).Runs)
	slices.Sort(got)
	if !slices.Equal(got, []string{"plain", "remote"}) {
		t.Fatalf("offered %v to a runner with path_sources off, want [plain remote]", got)
	}
	for _, id := range []string{"path", "local"} {
		if s := f.state(t, id); s != "queued" {
			t.Errorf("run %s is %s, want queued", id, s)
		}
	}

	on := f.register(t, "on")
	got = ids(f.mustSync(t, "on", on, first("on", 4)).Runs)
	slices.Sort(got)
	if !slices.Equal(got, []string{"local", "path"}) {
		t.Fatalf("offered %v to a runner that takes path sources, want [local path]", got)
	}
}

// A session already on the runner can go nowhere else, so its next run is
// offered there whatever it names: the runner's refusal names the setting,
// where a run left queued for good would say nothing.
func TestABoundSessionsRunIsOfferedDespitePathSourcesOff(t *testing.T) {
	f := newFixture(t)
	off := f.register(t, "off")
	f.enqueue(t, run("a", "s1"))
	if got := ids(f.mustSync(t, "off", off, offRunner("off", 1)).Runs); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("offered %v, want [a]", got)
	}
	f.mustSync(t, "off", off, req("off", 0, claimed("a")...))
	f.finish(t, "a", v1.RunSucceeded, &v1.Result{State: v1.RunSucceeded})

	next := run("b", "s1")
	next.Session.New = false
	next.Sources = []v1.Source{{Path: "/home/me/src/app"}}
	f.enqueue(t, next)
	if got := ids(f.mustSync(t, "off", off, req("off", 1)).Runs); !slices.Equal(got, []string{"b"}) {
		t.Errorf("offered %v to the runner holding the session, want [b]", got)
	}
}
