package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
)

// A removal whose home waits for a run, or whose set-aside failed, is held
// only in the daemon's memory — the doomed flag — once config.toml has
// dropped the label. The marker the removal writes in the home first is what
// survives the daemon dying (DEV-160, decision 0070): the next start deletes
// every home that carries it and that config.toml no longer lists, and none
// that does not carry it.

// removeWith is each way a removal reaches the daemon: a hub's remove_account,
// and `yad account remove` with one running — which drops the label from
// config.toml and tells the daemon to reread it.
var removeWith = []struct {
	name   string
	remove func(t *testing.T, a *Accounts, p config.Paths, r account.Ref) error
}{
	{"a hub's removal", func(t *testing.T, a *Accounts, p config.Paths, r account.Ref) error {
		_, _, err := a.Remove(context.Background(), p, r)
		return err
	}},
	{"the owner's removal", func(t *testing.T, a *Accounts, p config.Paths, r account.Ref) error {
		if _, err := account.Unlist(context.Background(), p, r.Harness, r.Label, func(bool) bool { return true }); err != nil {
			t.Fatal(err)
		}
		_, err := a.Reread(context.Background(), p, r, true)
		return err
	}},
}

// restart is the daemon coming back after it died: a new Accounts from
// config.toml as it reads now, attached as Serve attaches it. Nothing of the
// one before survives but what is on disk.
func restart(t *testing.T, e *env) *Accounts {
	t.Helper()
	cfg, err := config.Load(e.paths)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAccounts(e.paths.Data, account.ListsOf(cfg))
	a.usePaths(e.paths)
	a.attach(context.Background(), e.store)
	return a
}

func removalDone(t *testing.T, data, label string) {
	t.Helper()
	home := account.HomeDir(data, "claude", label)
	if exists(home) {
		t.Errorf("%s is still there", home)
	}
	if refs, err := account.Unfinished(data); err != nil || slices.Contains(refs, account.Ref{Harness: "claude", Label: label}) {
		t.Errorf("the removal of %s is still unfinished: %v, %v", label, refs, err)
	}
}

// The acceptance: a removal waiting on a run, and a daemon killed before the
// run ends. The home and its login are deleted at the next start.
func TestARemovalWaitingOnARunIsFinishedAfterTheDaemonDies(t *testing.T) {
	for _, via := range removeWith {
		t.Run(via.name, func(t *testing.T) {
			e := newEnv(t)
			if err := config.Save(e.paths, accountConfig("work", "spare")); err != nil {
				t.Fatal(err)
			}
			home := plantCredential(t, e.paths.Data, "work")
			plantCredential(t, e.paths.Data, "spare")
			a := restart(t, e)
			ref := account.Ref{Harness: "claude", Label: "work"}
			if _, ok := a.take(ref, "run-1"); !ok {
				t.Fatal("the run could not take the account")
			}
			if err := via.remove(t, a, e.paths, ref); err != nil {
				t.Fatal(err)
			}
			if !exists(home) {
				t.Fatal("the home went while a run was on it")
			}
			if !account.Marked(e.paths.Data, "claude", "work") {
				t.Error("the home waiting on its run carries no marker")
			}
			// The daemon dies here: the hold is never released.
			restart(t, e)
			removalDone(t, e.paths.Data, "work")
			if !exists(account.HomeDir(e.paths.Data, "claude", "spare")) {
				t.Error("the start deleted a home config.toml still lists")
			}
		})
	}
}

// SetAside's rename failing leaves the home at its path with config.toml
// already changed. Neither the aside copies the start looked for nor a hub's
// repeat found it; the marker does.
func TestARemovalWhoseSetAsideFailedIsFinishedAtTheNextStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root renames in a directory it may not write")
	}
	for _, via := range removeWith {
		t.Run(via.name, func(t *testing.T) {
			e := newEnv(t)
			if err := config.Save(e.paths, accountConfig("work")); err != nil {
				t.Fatal(err)
			}
			home := plantCredential(t, e.paths.Data, "work")
			a := restart(t, e)
			ref := account.Ref{Harness: "claude", Label: "work"}
			dir := filepath.Dir(home)
			// The rename needs the accounts directory writable; the marker
			// needs only the home.
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			err := via.remove(t, a, e.paths, ref)
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err == nil {
				t.Fatal("the removal reported no error with its home still in place")
			}
			if got := listedInConfig(t, e.paths); len(got) != 0 {
				t.Fatalf("config.toml still lists %v", got)
			}
			if !exists(home) {
				t.Fatal("the home went although it could not be set aside")
			}
			restart(t, e)
			removalDone(t, e.paths.Data, "work")
		})
	}
}

// A hub repeating a removal whose set-aside failed finishes it there and
// then, as it finishes a home an earlier delete left aside.
func TestAHubRepeatFinishesAMarkedHome(t *testing.T) {
	e := newEnv(t)
	if err := config.Save(e.paths, accountConfig()); err != nil {
		t.Fatal(err)
	}
	home := plantCredential(t, e.paths.Data, "work")
	if err := account.Mark(e.paths.Data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	a := accountsOf(e.paths.Data, accountConfig())
	// Not attached: the start would finish it, and this is about the repeat.
	close(a.attached)
	a.store = e.store
	ref := account.Ref{Harness: "claude", Label: "work"}
	if _, removed, err := a.Remove(context.Background(), e.paths, ref); err != nil || removed {
		t.Fatalf("Remove = %v, %v; want nothing new removed and no error", removed, err)
	}
	if exists(home) {
		t.Error("a hub's repeat left the marked home")
	}
}

// A repeat that arrives while the run is still on the account waits for it
// with the removal before it: the run keeps its home.
func TestAHubRepeatLeavesAHomeARunIsStillIn(t *testing.T) {
	e := newEnv(t)
	if err := config.Save(e.paths, accountConfig("work")); err != nil {
		t.Fatal(err)
	}
	home := plantCredential(t, e.paths.Data, "work")
	a := restart(t, e)
	ref := account.Ref{Harness: "claude", Label: "work"}
	h, _ := a.take(ref, "run-1")
	for range 2 {
		if _, _, err := a.Remove(context.Background(), e.paths, ref); err != nil {
			t.Fatal(err)
		}
	}
	if !exists(home) {
		t.Fatal("a repeated removal deleted a home a run was still in")
	}
	a.release(h)
	removalDone(t, e.paths.Data, "work")
}

// A home with no marker and no label is a `yad account add` in progress or
// walked away from, kept on purpose for the next try (decision 0043). The
// start never touches it.
func TestAPendingAddsHomeIsNeverSwept(t *testing.T) {
	e := newEnv(t)
	if err := config.Save(e.paths, accountConfig()); err != nil {
		t.Fatal(err)
	}
	pending := plantCredential(t, e.paths.Data, "pending")
	plantCredential(t, e.paths.Data, "doomed")
	if err := account.Mark(e.paths.Data, "claude", "doomed"); err != nil {
		t.Fatal(err)
	}
	restart(t, e)
	removalDone(t, e.paths.Data, "doomed")
	if !exists(filepath.Join(pending, ".credentials.json")) {
		t.Error("the start touched a pending add's home")
	}
}

// Killed between the marker and config.toml's write, a removal has not
// happened: the label is still listed, and a listed account's home is never
// deleted. The start takes the marker off, so a later hand edit of
// config.toml is not read as this removal; running it again removes it.
func TestAKillBetweenTheMarkerAndConfigLeavesTheAccount(t *testing.T) {
	e := newEnv(t)
	if err := config.Save(e.paths, accountConfig("work")); err != nil {
		t.Fatal(err)
	}
	home := plantCredential(t, e.paths.Data, "work")
	// What `yad account remove` or a hub's removal had done when it died.
	if err := account.Mark(e.paths.Data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	a := restart(t, e)
	if !exists(filepath.Join(home, ".credentials.json")) {
		t.Fatal("the start deleted the home of an account config.toml still lists")
	}
	if account.Marked(e.paths.Data, "claude", "work") {
		t.Error("the start left the marker on a listed account's home")
	}
	ref := account.Ref{Harness: "claude", Label: "work"}
	if _, ok := a.take(ref, "run-1"); !ok {
		t.Error("the account is not taken as listed")
	}
	// The run lets go, and the owner runs the removal again.
	a2 := restart(t, e)
	if _, removed, err := a2.Remove(context.Background(), e.paths, ref); err != nil || !removed {
		t.Fatalf("Remove = %v, %v", removed, err)
	}
	removalDone(t, e.paths.Data, "work")
}

// An add of a label whose removal is waiting on a run takes the marker off
// with the pending deletion: the home is the one being logged in now, and a
// daemon dying before the add finishes must not delete it at the next start.
func TestAnAddTakesTheMarkerOff(t *testing.T) {
	for _, c := range []struct {
		name string
		add  func(a *Accounts, r account.Ref)
	}{
		{"yad account add", func(a *Accounts, r account.Ref) {
			if err := a.Keep(r); err != nil {
				t.Fatal(err)
			}
		}},
		{"a hub's add", func(a *Accounts, r account.Ref) {
			if _, err := a.takeForAdd(r, "lg1"); err != nil {
				t.Fatal(err)
			}
		}},
		{"a reload of the label added", func(a *Accounts, r account.Ref) {
			if _, err := a.Reload(context.Background(), claudeLists("work"), r, false); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if err := config.Save(e.paths, accountConfig("work")); err != nil {
				t.Fatal(err)
			}
			home := plantCredential(t, e.paths.Data, "work")
			a := restart(t, e)
			ref := account.Ref{Harness: "claude", Label: "work"}
			a.take(ref, "run-1")
			if _, _, err := a.Remove(context.Background(), e.paths, ref); err != nil {
				t.Fatal(err)
			}
			c.add(a, ref)
			if account.Marked(e.paths.Data, "claude", "work") {
				t.Error("the add left the marker")
			}
			restart(t, e)
			if !exists(home) {
				t.Error("the next start deleted a home an add had taken back")
			}
		})
	}
}

// The daemon reads config.toml before its socket opens and sweeps after, so
// `yad account remove` can mark a home and drop its label in between. The
// sweep's lists still name the label; the file it reads under the lock does
// not, and the removal is in flight rather than dead. Its marker stays, so a
// daemon that dies before the Reload it is waiting on still leaves a home the
// next start deletes.
func TestAStartSweepLeavesARemovalInFlightMarked(t *testing.T) {
	e := newEnv(t)
	if err := config.Save(e.paths, accountConfig("work")); err != nil {
		t.Fatal(err)
	}
	home := plantCredential(t, e.paths.Data, "work")
	// Read at start, before the removal.
	a := accountsOf(e.paths.Data, accountConfig("work"))
	a.usePaths(e.paths)
	if _, err := account.Unlist(context.Background(), e.paths, "claude", "work", func(bool) bool { return true }); err != nil {
		t.Fatal(err)
	}
	a.attach(context.Background(), e.store)
	if !account.Marked(e.paths.Data, "claude", "work") {
		t.Fatal("the start sweep took the marker off a removal in flight")
	}
	if !exists(home) {
		t.Fatal("the start sweep deleted a home its lists still name")
	}
	// The daemon dies before the CLI's word reaches Reload.
	restart(t, e)
	removalDone(t, e.paths.Data, "work")
}

// A hub's add whose marker cannot come off does not begin: its login would
// write a home the next start deletes. The removal it would have cancelled
// stands, and the last run on the account still deletes the home.
func TestAHubAddThatCannotUnmarkDoesNotBegin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root unlinks in a directory it may not write")
	}
	e := newEnv(t)
	if err := config.Save(e.paths, accountConfig("work")); err != nil {
		t.Fatal(err)
	}
	home := plantCredential(t, e.paths.Data, "work")
	a := restart(t, e)
	ref := account.Ref{Harness: "claude", Label: "work"}
	h, _ := a.take(ref, "run-1")
	if _, _, err := a.Remove(context.Background(), e.paths, ref); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	hold, err := a.takeForAdd(ref, "lg1")
	kerr := a.Keep(ref)
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err == nil || hold != nil {
		t.Fatalf("takeForAdd = %v, %v; want no hold and an error", hold, err)
	}
	if kerr == nil {
		t.Error("Keep said the pending deletion was cancelled with the marker still on")
	}
	if !account.Marked(e.paths.Data, "claude", "work") {
		t.Fatal("the marker went")
	}
	a.release(h)
	removalDone(t, e.paths.Data, "work")
}

// The other way round: the daemon loaded the lists without a label whose
// marked home was waiting, and the owner listed it again before the sweep —
// by hand, or with `yad account add`, whose word to the daemon waits for this
// start. The file lists it, so its home is kept and the marker comes off.
func TestAStartSweepKeepsAHomeTheFileListsAgain(t *testing.T) {
	e := newEnv(t)
	if err := config.Save(e.paths, accountConfig("work")); err != nil {
		t.Fatal(err)
	}
	home := plantCredential(t, e.paths.Data, "work")
	if err := account.Mark(e.paths.Data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	// Read at start, before the label was listed again.
	a := accountsOf(e.paths.Data, accountConfig())
	a.usePaths(e.paths)
	a.attach(context.Background(), e.store)
	if !exists(filepath.Join(home, ".credentials.json")) {
		t.Fatal("the start sweep deleted a home config.toml lists")
	}
	if account.Marked(e.paths.Data, "claude", "work") {
		t.Error("the marker is still on a home config.toml lists")
	}
}
