package account

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/supervise"
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

// Every variable that points a harness at an account's home is one a hub may
// not grant: a grant is delivered beside the home, and when the harness has no
// account configured nothing overrides it at all (decision 0040). The list
// lives in protocol/v1 so the hub and the runner refuse the same names; this
// keeps a harness that gains account homes from being missed there.
func TestHomeVariablesAreNotGrantable(t *testing.T) {
	for harness, name := range homeVar {
		err := v1.Grant{Name: name, Value: "/elsewhere", As: v1.GrantEnv}.Validate()
		if err == nil || !strings.Contains(err.Error(), "0040") {
			t.Errorf("%s's home variable %s as a grant: err %v, want the account refusal", harness, name, err)
		}
	}
}

// The names a hub may not grant and the names removed from the owner's own
// environment are one list, protocol/v1's (DEV-62), and the only names on it
// that reach a child from the owner are the harness home variables — the
// harness's own login when it has no accounts, overridden by an account's
// home when it has one. The list is read back from the Grant.Name doc, which
// protocol/v1's tests hold to the list itself, so a name added there is
// checked here without anyone adding it twice.
func TestScrubRemovesWhatAGrantMayNotCarry(t *testing.T) {
	f, _ := reflect.TypeFor[v1.Grant]().FieldByName("Name")
	var names []string
	for _, w := range regexp.MustCompile(`[A-Z][A-Z0-9_]*`).FindAllString(f.Tag.Get("doc"), -1) {
		if _, ok := v1.AccountVariable(w); ok {
			names = append(names, w)
		}
	}
	// The doc names the provider switches by their prefix; one of them stands
	// for the family.
	names = append(names, "CLAUDE_CODE_USE_SOMETHING_NEW")
	if len(names) < 15 {
		t.Fatalf("read only %v from the Grant.Name doc — has its wording changed?", names)
	}
	homes := map[string]bool{}
	for _, v := range homeVar {
		homes[v] = true
	}
	for _, name := range names {
		env := []string{name + "=owner", "PATH=/bin"}
		kept := len(supervise.Scrub(env, nil)) == 2
		if kept != homes[name] {
			t.Errorf("%s: kept by Scrub = %v, want %v — every name a grant may not carry is removed from the owner's environment but a harness home", name, kept, homes[name])
		}
	}
	for name := range homes {
		if !slices.Contains(names, name) {
			t.Errorf("home variable %s is not on protocol/v1's list", name)
		}
	}
}

// With nothing to choose between them on reset times, the owner's order
// decides, and an account that cannot run a turn is skipped whether it is
// limited or needs login.
func TestSoonestFallsBackToTheOwnersOrder(t *testing.T) {
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
			got, ok := Soonest(c.accounts, "claude", time.Now())
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
func TestSoonestStaysWithinItsHarness(t *testing.T) {
	accounts := []Account{
		{Harness: "codex", Label: "c1", State: v1.AccountFree},
		{Harness: "claude", Label: "a1", State: v1.AccountNeedsLogin},
	}
	if a, ok := Soonest(accounts, "claude", time.Now()); ok {
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
	got, err := Load(ctx, st.Queries, data, ListsOf(cfg), time.Now())
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
	got, err := Load(context.Background(), nil, t.TempDir(), ListsOf(cfg), time.Now())
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
	got, err := Load(ctx, st.Queries, data, ListsOf(cfg), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != v1.AccountNeedsLogin {
		t.Fatalf("accounts = %+v, want the one account needing login", got)
	}
	if _, ok := Soonest(got, "claude", time.Now()); ok {
		t.Error("a run would have been given an account with no home")
	}

	// And once the home is there again, the stored state stands.
	if _, err := Ensure(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if got, err = Load(ctx, st.Queries, data, ListsOf(cfg), time.Now()); err != nil {
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
// Replacing by rename closes it.
//
// Nothing here waits on the scheduler to produce the race. Each round points
// the link at a decoy before releasing the racers, so at least one of them
// has to re-link — without that every Ensure returns at the already-correct
// check and the test proves nothing, which is how its first version passed
// against the code it was written to catch. The decoy is put in place by an
// atomic replace, so any gap belongs to the code under test. beforeReplace
// then looks at the path from inside every re-link, at the instant a
// remove-first version would have it empty, and a free-running observer
// watches the rest of the time.
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
	pointAtDecoy := func() {
		tmp, err := os.MkdirTemp(filepath.Dir(link), ".swap-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmp)
		staged := filepath.Join(tmp, "link")
		if err := os.Symlink(decoy, staged); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(staged, link); err != nil {
			t.Fatal(err)
		}
	}

	gaps := make(chan string, 1)
	sawGap := func(when string, err error) {
		select {
		case gaps <- when + ": " + err.Error():
		default:
		}
	}
	var relinks atomic.Int64
	beforeReplace = func(path string) {
		if path != link {
			return
		}
		relinks.Add(1)
		if _, err := os.Lstat(path); err != nil {
			sawGap("as a racer was about to re-link it", err)
		}
	}
	t.Cleanup(func() { beforeReplace = nil })

	stop := make(chan struct{})
	var observer sync.WaitGroup
	observer.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Lstat, not Stat: the question is whether anything is at the
			// path at all, not whether it resolves.
			if _, err := os.Lstat(link); err != nil {
				sawGap("between re-links", err)
				return
			}
		}
	})
	stopObserver := sync.OnceFunc(func() { close(stop); observer.Wait() })
	defer stopObserver()

	const rounds, racers = 50, 4
	for round := range rounds {
		pointAtDecoy()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range racers {
			wg.Go(func() {
				<-start // let them collide rather than run in turn
				if _, err := Ensure(data, "claude", "work"); err != nil {
					t.Errorf("Ensure failed while another was running: %v", err)
				}
			})
		}
		close(start)
		wg.Wait()
		if at, err := os.Readlink(link); err != nil || at != want {
			t.Fatalf("after round %d the link is %q (%v), want %s", round, at, err, want)
		}
	}
	stopObserver()

	select {
	case g := <-gaps:
		t.Errorf("the link was missing %s", g)
	default:
	}
	// Every round began at the decoy and ended at the shared directory, so
	// some racer re-linked in each. Fewer calls than rounds means they did it
	// by a path beforeReplace does not stand in — a remove-and-symlink put
	// back in place of placeLink — and the check above watched nothing.
	if n := relinks.Load(); n < rounds {
		t.Errorf("%d of %d rounds re-linked through placeLink; the rest replaced the link some other way, which nothing here watched", n, rounds)
	}
}

// Decision 0039's ordering, which is the whole of how a run picks among free
// accounts: the one whose window refills soonest goes first, because the
// quota left in it is about to be thrown away and the others' is not.
func TestSoonestPrefersTheAccountWhoseWindowRefillsFirst(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	free := func(label string, ws ...v1.AccountWindow) Account {
		return Account{Harness: "claude", Label: label, State: v1.AccountFree, Windows: ws}
	}
	for _, c := range []struct {
		name     string
		accounts []Account
		want     string
	}{
		{
			"the sooner refill wins, whatever the owner's order says",
			[]Account{
				free("first", v1.AccountWindow{Name: "five_hour", UsedPercent: 30, ResetsAt: at(4 * time.Hour)}),
				free("second", v1.AccountWindow{Name: "five_hour", UsedPercent: 30, ResetsAt: at(20 * time.Minute)}),
			},
			"second",
		},
		{
			// A window at 0% has no quota to waste, so a refill it does not
			// need must not pull it to the front.
			"an untouched window does not count as a refill",
			[]Account{
				free("first", v1.AccountWindow{Name: "five_hour", UsedPercent: 0, ResetsAt: at(10 * time.Minute)}),
				free("second", v1.AccountWindow{Name: "five_hour", UsedPercent: 80, ResetsAt: at(2 * time.Hour)}),
			},
			"second",
		},
		{
			// A reset already past is the residue of a limit that is over
			// and says nothing about the future.
			"an elapsed reset is not a refill to come",
			[]Account{
				free("first", v1.AccountWindow{Name: "five_hour", UsedPercent: 90, ResetsAt: at(-time.Hour)}),
				free("second", v1.AccountWindow{Name: "five_hour", UsedPercent: 90, ResetsAt: at(time.Hour)}),
			},
			"second",
		},
		{
			// An account nothing is known about keeps whatever it has; the
			// one with expiring quota is spent first.
			"an account with no windows sorts behind one with a refill to come",
			[]Account{
				free("first"),
				free("second", v1.AccountWindow{Name: "five_hour", UsedPercent: 50, ResetsAt: at(time.Hour)}),
			},
			"second",
		},
		{
			"with nothing dated at all, the owner's order breaks the tie",
			[]Account{free("first"), free("second")},
			"first",
		},
		{
			"a limited account is skipped however soon it refills",
			[]Account{
				{Harness: "claude", Label: "first", State: v1.AccountLimited, LimitedUntil: at(time.Hour),
					Windows: []v1.AccountWindow{{Name: "five_hour", UsedPercent: 100, ResetsAt: at(time.Minute)}}},
				free("second", v1.AccountWindow{Name: "five_hour", UsedPercent: 10, ResetsAt: at(3 * time.Hour)}),
			},
			"second",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Soonest(c.accounts, "claude", now)
			if !ok || got.Label != c.want {
				t.Errorf("took %q (%v), want %q", got.Label, ok, c.want)
			}
		})
	}
}

// NextFree is what a waiting run's resumes_at is taken from: the earliest
// reset among the accounts that have one. An account needing a login comes
// back when the owner acts, which is not a moment anything here can name, so
// it never dates a wait.
func TestNextFreeIsTheEarliestResetAndNothingElse(t *testing.T) {
	now := time.Now()
	soon, later := now.Add(time.Hour), now.Add(4*time.Hour)
	limited := func(label string, until *time.Time) Account {
		return Account{Harness: "claude", Label: label, State: v1.AccountLimited, LimitedUntil: until}
	}
	for _, c := range []struct {
		name     string
		accounts []Account
		want     time.Time
	}{
		{"the earliest of several", []Account{limited("a", &later), limited("b", &soon)}, soon},
		{"another harness's resets are not borrowed",
			[]Account{{Harness: "codex", Label: "c", State: v1.AccountLimited, LimitedUntil: &soon}}, time.Time{}},
		{"an account needing login dates nothing",
			[]Account{{Harness: "claude", Label: "a", State: v1.AccountNeedsLogin}}, time.Time{}},
		{"a limit with no reset dates nothing", []Account{limited("a", nil)}, time.Time{}},
		{"no accounts", nil, time.Time{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := NextFree(c.accounts, "claude")
			if !got.Equal(c.want) {
				t.Errorf("NextFree = %v, want %v", got, c.want)
			}
		})
	}
}
