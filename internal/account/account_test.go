package account

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store"
	v1 "github.com/skkap/yad/protocol/v1"
)

func TestEnsureBuildsTheHomeAndItsTranscriptLink(t *testing.T) {
	for _, c := range []struct{ harness, link string }{
		{"claude", "projects"},
		{"codex", "sessions"},
	} {
		t.Run(c.harness, func(t *testing.T) {
			data := t.TempDir()
			home, err := Ensure(data, c.harness, "work")
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(data, "accounts", c.harness, "work"); home != want {
				t.Errorf("home %s, want %s", home, want)
			}
			fi, err := os.Stat(home)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o700 {
				t.Errorf("home is %v, want 0700 — it holds the credential the harness wrote there", fi.Mode().Perm())
			}
			at, err := os.Readlink(filepath.Join(home, c.link))
			if err != nil {
				t.Fatalf("%s/%s is not a link: %v", home, c.link, err)
			}
			if want := TranscriptDir(data, c.harness); at != want {
				t.Errorf("%s links to %s, want the shared %s", c.link, at, want)
			}
			// Twice is the normal case: every run calls it.
			if _, err := Ensure(data, c.harness, "work"); err != nil {
				t.Fatalf("second Ensure: %v", err)
			}
		})
	}
}

// Every account home reaches the same transcripts, which is what lets a
// session move between accounts (decision 0013, measured in DEV-24).
func TestEveryHomeSeesTheSameTranscripts(t *testing.T) {
	data := t.TempDir()
	a, err := Ensure(data, "claude", "personal")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Ensure(data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "projects", "one.jsonl"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b, "projects", "one.jsonl")); err != nil {
		t.Errorf("a session written in one home is not visible in the other: %v", err)
	}
}

// A directory of real transcripts is never deleted to make room for the link:
// those conversations cannot be rebuilt.
func TestEnsureRefusesToReplaceRealTranscripts(t *testing.T) {
	data := t.TempDir()
	home := HomeDir(data, "claude", "work")
	if err := os.MkdirAll(filepath.Join(home, "projects", "a-repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(data, "claude", "work")
	if err == nil {
		t.Fatal("a home with its own transcripts was linked over without a word")
	}
	if _, statErr := os.Stat(filepath.Join(home, "projects", "a-repo")); statErr != nil {
		t.Errorf("the transcripts were removed anyway: %v", statErr)
	}
}

// Removing an account logs it out of this machine. It must not take every
// other account's sessions with it: the link is deleted, never followed.
func TestRemoveKeepsTheSharedTranscripts(t *testing.T) {
	data := t.TempDir()
	home, err := Ensure(data, "codex", "work")
	if err != nil {
		t.Fatal(err)
	}
	shared := TranscriptDir(data, "codex")
	if err := os.WriteFile(filepath.Join(shared, "rollout.jsonl"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// What codex leaves behind: sqlite and its sidecars (DEV-24).
	for _, f := range []string{"state_5.sqlite", "state_5.sqlite-shm", "state_5.sqlite-wal"} {
		if err := os.WriteFile(filepath.Join(home, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Remove(data, "codex", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("the home survived the remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shared, "rollout.jsonl")); err != nil {
		t.Errorf("removing one account deleted the shared transcripts: %v", err)
	}
}

func TestEnv(t *testing.T) {
	for _, c := range []struct {
		harness, home string
		want          []string
	}{
		{"claude", "/h", []string{"CLAUDE_CONFIG_DIR=/h"}},
		{"codex", "/h", []string{"CODEX_HOME=/h"}},
		{"gemini", "/h", nil},
		{"claude", "", nil},
	} {
		got := Env(c.harness, c.home)
		if len(got) != len(c.want) || (len(got) == 1 && got[0] != c.want[0]) {
			t.Errorf("Env(%q, %q) = %v, want %v", c.harness, c.home, got, c.want)
		}
	}
}

// The owner's order decides, and an account that cannot run a turn is skipped
// whether it is limited or needs login.
func TestFirstTakesTheFirstFreeAccountInTheOwnersOrder(t *testing.T) {
	soon := time.Now().Add(time.Hour)
	mk := func(label string, s v1.AccountState) Account {
		a := Account{Harness: "claude", Label: label, State: s}
		if s == v1.AccountLimited {
			a.LimitedUntil = &soon
		}
		return a
	}
	for _, c := range []struct {
		name     string
		accounts []Account
		want     string
	}{
		{"all free takes the first", []Account{mk("a", v1.AccountFree), mk("b", v1.AccountFree)}, "a"},
		{"skips a limited one", []Account{mk("a", v1.AccountLimited), mk("b", v1.AccountFree)}, "b"},
		{"skips one that needs login", []Account{mk("a", v1.AccountNeedsLogin), mk("b", v1.AccountFree)}, "b"},
		{"skips both kinds", []Account{mk("a", v1.AccountNeedsLogin), mk("b", v1.AccountLimited), mk("c", v1.AccountFree)}, "c"},
		{"none usable", []Account{mk("a", v1.AccountNeedsLogin), mk("b", v1.AccountLimited)}, ""},
		{"no accounts", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := First(c.accounts, "claude")
			if c.want == "" {
				if ok {
					t.Errorf("took %q from accounts none of which can run", got.Label)
				}
				return
			}
			if !ok || got.Label != c.want {
				t.Errorf("took %q (%v), want %q", got.Label, ok, c.want)
			}
		})
	}
}

// Another harness's accounts are never borrowed.
func TestFirstStaysWithinItsHarness(t *testing.T) {
	accounts := []Account{
		{Harness: "codex", Label: "c1", State: v1.AccountFree},
		{Harness: "claude", Label: "a1", State: v1.AccountNeedsLogin},
	}
	if a, ok := First(accounts, "claude"); ok {
		t.Errorf("claude took %q, which is a %s account", a.Label, a.Harness)
	}
}

func TestLoadReadsTheOwnersOrderAndTheStoresStates(t *testing.T) {
	data := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := SetState(ctx, st.Queries, "claude", "work", v1.AccountNeedsLogin, time.UnixMilli(1000)); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{
		"claude": {Accounts: []string{"personal", "work"}},
	}
	// Both homes exist: a home that is not there is needs-login whatever the
	// store says, which TestAnAccountWhoseHomeIsGoneNeedsLogin covers.
	for _, label := range []string{"personal", "work"} {
		if _, err := Ensure(data, "claude", label); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Load(ctx, st.Queries, data, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d accounts, want 2", len(got))
	}
	// The order is the owner's, not the store's or the alphabet's.
	if got[0].Label != "personal" || got[1].Label != "work" {
		t.Errorf("order is %q, %q — want the owner's personal, work", got[0].Label, got[1].Label)
	}
	// An account the store never saw is free: absence is data.
	if got[0].State != v1.AccountFree {
		t.Errorf("an account with no row is %q, want free", got[0].State)
	}
	if got[1].State != v1.AccountNeedsLogin {
		t.Errorf("the stored state was lost: %q", got[1].State)
	}
	if got[1].Home != HomeDir(data, "claude", "work") {
		t.Errorf("home %s, want %s", got[1].Home, HomeDir(data, "claude", "work"))
	}
}

// A harness the owner gave no accounts reports none, and that is a state and
// not a failure: its runs use the harness's own login.
func TestNoAccountsIsNotAnError(t *testing.T) {
	cfg := config.Default()
	got, err := Load(context.Background(), nil, t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("a runner with no accounts and no state database failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d accounts, want none", len(got))
	}
	if reps := Reports(got, "claude"); reps != nil {
		t.Errorf("reported %v, want nothing to report", reps)
	}
}

// A hub sees the label and the state, and nothing else the home holds.
func TestReportCarriesTheLabelAndTheStateOnly(t *testing.T) {
	a := Account{Harness: "claude", Label: "work", Home: "/secret/place", State: v1.AccountNeedsLogin}
	r := a.Report()
	if r.Label != "work" || r.State != v1.AccountNeedsLogin {
		t.Errorf("report is %+v", r)
	}
	if r.LimitedUntil != nil {
		t.Errorf("an account that needs login carries a reset time: %v", r.LimitedUntil)
	}
}

func TestEnsureRefusesABadLabel(t *testing.T) {
	for _, label := range []string{"../escape", "Work", "", "a/b"} {
		if _, err := Ensure(t.TempDir(), "claude", label); err == nil {
			t.Errorf("label %q was accepted; it becomes a directory name", label)
		}
	}
}

// A label left in config.toml whose home is not on disk needs login. This is
// what `yad account remove` looks like to a daemon still holding the config it
// started with: without this the account reads free, the next run rebuilds the
// empty home the owner just deleted, and the turn fails against a logged-out
// harness.
func TestAnAccountWhoseHomeIsGoneNeedsLogin(t *testing.T) {
	data := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work"}}}

	// The store says free — it is what a fresh row, or no row at all, means.
	if err := SetState(ctx, st.Queries, "claude", "work", v1.AccountFree, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := Load(ctx, st.Queries, data, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != v1.AccountNeedsLogin {
		t.Fatalf("accounts = %+v, want the one account needing login", got)
	}
	if _, ok := First(got, "claude"); ok {
		t.Error("a run would have been given an account with no home")
	}

	// And once the home is there again, the stored state stands.
	if _, err := Ensure(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if got, err = Load(ctx, st.Queries, data, cfg); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != v1.AccountFree {
		t.Fatalf("accounts = %+v, want free once the home is back", got)
	}
}

// A path element that walks out of the data directory is refused before
// os.RemoveAll ever sees it — the harness id as well as the label, because
// filepath.Join cleans ".." and the result is outside <data>/accounts/.
func TestPathElementsAreGuarded(t *testing.T) {
	for _, c := range []struct{ harness, label string }{
		{"../../escapee", "work"},
		{"claude", "../../escapee"},
		{"Claude", "work"},
		{"", "work"},
		{"claude", ""},
	} {
		// The data directory sits inside a directory this test owns, so a
		// witness at the path the argument actually resolves to is still
		// cleaned up — and a witness at any other path would survive whether
		// or not the guard is there, which proves nothing.
		root := t.TempDir()
		data := filepath.Join(root, "one", "two")
		if err := os.MkdirAll(data, 0o700); err != nil {
			t.Fatal(err)
		}
		target := HomeDir(data, c.harness, c.label)
		if !strings.HasPrefix(target, root+string(filepath.Separator)) {
			t.Fatalf("the witness %s is outside the directory this test owns (%s)", target, root)
		}
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := Remove(data, c.harness, c.label); err == nil {
			t.Errorf("Remove(%q, %q) was accepted", c.harness, c.label)
		}
		if _, err := Ensure(data, c.harness, c.label); err == nil {
			t.Errorf("Ensure(%q, %q) was accepted", c.harness, c.label)
		}
		if _, err := os.Stat(target); err != nil {
			t.Errorf("Remove(%q, %q) deleted %s: %v", c.harness, c.label, target, err)
		}
	}
}

// Ensure is called by every run that takes an account, capacity is a shared
// pool, and nothing reserves an account — so several runs of one harness can
// prepare the same home at once. The tolerance in link() exists for that and
// had no evidence it worked.
//
// The reachable window is an existing home whose link is missing or points
// somewhere else: the data directory moved, or the link was removed outside
// YAD. A run refused because two of them raced would fail for a reason that
// has nothing to do with the run.
func TestConcurrentEnsureOnOneHome(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, data, home string)
	}{
		{"a home with no link yet", func(t *testing.T, data, home string) {
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"an empty real directory where the link belongs", func(t *testing.T, data, home string) {
			// Every racer takes the default branch: ReadDir, Remove, Symlink.
			// The loser of each step must not fail its run.
			if err := os.MkdirAll(filepath.Join(home, "projects"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"a link pointing somewhere else", func(t *testing.T, data, home string) {
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			elsewhere := filepath.Join(data, "moved")
			if err := os.MkdirAll(elsewhere, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, filepath.Join(home, "projects")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := t.TempDir()
			c.setup(t, data, HomeDir(data, "claude", "work"))

			const racers = 8
			var wg sync.WaitGroup
			errs := make([]error, racers)
			start := make(chan struct{})
			for i := range racers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start // let them collide rather than run in turn
					_, errs[i] = Ensure(data, "claude", "work")
				}()
			}
			close(start)
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Errorf("concurrent Ensure %d failed, which would refuse a run: %v", i, err)
				}
			}
			at, err := os.Readlink(filepath.Join(HomeDir(data, "claude", "work"), "projects"))
			if err != nil {
				t.Fatalf("no transcript link after the race: %v", err)
			}
			if want := TranscriptDir(data, "claude"); at != want {
				t.Errorf("link points at %s, want the shared %s", at, want)
			}
		})
	}
}

// Once the link exists it is never observed missing, however many runs are
// re-linking the same home at once.
//
// This is the invariant remove-then-symlink could not hold: in the gap between
// the two, a harness one run had already started could create a real directory
// at projects/ and write its transcripts somewhere no other account can see.
// Replacing by rename closes it, and this watches for the gap rather than
// checking the state once everyone has finished.
//
// A breaker keeps pointing the link at a decoy so the racers have real work —
// without it every Ensure returns at the already-correct check and the test
// proves nothing. The breaker replaces atomically itself, so any gap the
// observer sees belongs to the code under test.
func TestTheTranscriptLinkIsNeverObservedMissing(t *testing.T) {
	data := t.TempDir()
	home, err := Ensure(data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "projects")
	want := TranscriptDir(data, "claude")
	decoy := filepath.Join(data, "decoy")
	if err := os.MkdirAll(decoy, 0o700); err != nil {
		t.Fatal(err)
	}
	// Atomic replace, so the breaker never creates a gap of its own.
	swapTo := func(target string) {
		tmp, err := os.MkdirTemp(filepath.Dir(link), ".swap-")
		if err != nil {
			return
		}
		defer os.RemoveAll(tmp)
		staged := filepath.Join(tmp, "link")
		if os.Symlink(target, staged) == nil {
			os.Rename(staged, link)
		}
	}

	stop := make(chan struct{})
	gaps := make(chan string, 4)
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { // the observer
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Lstat, not Stat: the question is whether anything is at the
			// path at all, not whether it resolves.
			if _, err := os.Lstat(link); err != nil {
				select {
				case gaps <- err.Error():
				default:
				}
				return
			}
		}
	}()
	go func() { // the breaker
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			swapTo(decoy)
		}
	}()

	var racers sync.WaitGroup
	for range 4 {
		racers.Add(1)
		go func() {
			defer racers.Done()
			for range 60 {
				if _, err := Ensure(data, "claude", "work"); err != nil {
					t.Errorf("Ensure failed while another was running: %v", err)
					return
				}
			}
		}()
	}
	racers.Wait()
	close(stop)
	bg.Wait()

	select {
	case g := <-gaps:
		t.Errorf("the link was missing while a run was re-linking it: %s", g)
	default:
	}
	// The breaker may have won the last write; what matters is that a final
	// Ensure lands it back on the shared directory.
	if _, err := Ensure(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if at, err := os.Readlink(link); err != nil || at != want {
		t.Errorf("link is %q (%v), want %s", at, err, want)
	}
}
