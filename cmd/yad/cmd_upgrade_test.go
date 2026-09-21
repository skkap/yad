package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/upgrade"
)

// bareUpgrade builds the upgrade commands of a default-profile, upstream install.
func bareUpgrade(args ...string) string { return upgrade.Command("", nil, args...) }

// `yad upgrade` is the next action the hub's version_too_old refusal names, so
// it has to be the command rather than the placeholder that named epic E9.
// With no gh on PATH — what yad() gives every test — it fails on gh.
func TestUpgradeIsBuilt(t *testing.T) {
	code, _, errs := yad(t, "upgrade", "--check")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %q", code, errs)
	}
	if strings.Contains(errs, "arrives in epic") {
		t.Errorf("yad upgrade still refers itself to a later epic: %q", errs)
	}
	if !strings.Contains(errs, "gh auth login") {
		t.Errorf("yad upgrade = %q, want it to name the tool it needs", errs)
	}
}

func TestUpgradeRejectsUnknownFlags(t *testing.T) {
	if code, _, _ := yad(t, "upgrade", "--yes-please"); code != 1 {
		t.Errorf("an unknown flag exited %d", code)
	}
}

// TestUpgradeDoesNotSilentlyBecomeARealUpgrade is named for what went wrong
// rather than for the flag that caused it. `flag` stops parsing at the first
// positional argument, so `yad upgrade v0.3.1 --check` never saw the --check:
// the flag whose whole purpose is "change nothing" was dropped, on the one
// command that overwrites the binary the operator is running. That is an
// irreversible action taken in place of a question being answered, and no
// wording of the refusal matters beside it — what matters is that nothing is
// installed.
func TestUpgradeDoesNotSilentlyBecomeARealUpgrade(t *testing.T) {
	for _, args := range [][]string{
		{"upgrade", "v0.3.1", "--check"},
		{"upgrade", "v0.3.1"},
		{"upgrade", "--check", "extra"},
	} {
		code, out, errs := yad(t, args...)
		if code != 1 {
			t.Fatalf("yad %v: exit %d, want a refusal: %q", args, code, errs)
		}
		// It must not have gone on to resolve a release or touch anything: the
		// refusal comes before any of that, so stdout carries none of the
		// lines a real run prints.
		if strings.Contains(out, "installed ") || strings.Contains(out, "replaced ") {
			t.Errorf("yad %v printed %q — it acted on a command it could not read", args, out)
		}
	}
	_, _, errs := yad(t, "upgrade", "v0.3.1", "--check")
	if !strings.Contains(errs, "--tag v0.3.1") {
		t.Errorf("yad upgrade v0.3.1 = %q, want it to name the flag that means it", errs)
	}
}

// scripts/install.sh installs a fork with YAD_REPO. An upgrade that ignored it
// would fetch upstream and replace the fork's binary with it.
func TestUpgradeFollowsTheRepositoryItWasInstalledFrom(t *testing.T) {
	t.Setenv("YAD_REPO", "someone/fork")
	if got := releaseSource().Repo; got != "someone/fork" {
		t.Errorf("releaseSource().Repo = %q, want the fork the operator installed from", got)
	}
	t.Setenv("YAD_REPO", "")
	if got := releaseSource().Repo; got != "" {
		t.Errorf("releaseSource().Repo = %q, want empty so upgrade.GH falls back to its default", got)
	}
}

// everyState is each answer Compare can give. A state added without a line of
// its own falls to the default and says "this build is <tag>", which is the
// one wrong answer these tests exist to catch.
var everyState = []upgrade.State{upgrade.Behind, upgrade.Current, upgrade.Ahead, upgrade.Unstamped, upgrade.UnreadableTag}

// Every state names what the owner would type next, because "up to date" with
// no way to override it is what sends someone to reinstall by hand.
func TestCheckLineCarriesANextAction(t *testing.T) {
	for _, state := range everyState {
		for _, named := range []bool{false, true} {
			line := checkLine(state, "v0.4.0", named, bareUpgrade)
			if !strings.Contains(line, "yad upgrade") {
				t.Errorf("state %v (named=%v) says %q, with nothing to type next", state, named, line)
			}
			if !strings.Contains(line, "v0.4.0") {
				t.Errorf("state %v (named=%v) says %q, without naming the release", state, named, line)
			}
		}
	}
}

// A tag given with --tag is the release the owner asked about, not the newest
// one — and the command offered has to install that tag rather than whatever
// `Latest` would resolve to, which is a different release.
// A tag that is not a version number is the operator's typo or a repository
// that tags "latest", and blaming the installed build for it contradicts the
// `installed  v0.4.0` line printed two lines above.
func TestCheckLineBlamesTheUnreadableSide(t *testing.T) {
	if line := checkLine(upgrade.UnreadableTag, "stable", true, bareUpgrade); strings.Contains(line, "this build carries no release version") {
		t.Errorf("an unreadable tag says %q, blaming the build for it", line)
	} else if !strings.Contains(line, `"stable" is not a version number`) {
		t.Errorf("an unreadable tag says %q, without naming the tag as the problem", line)
	}
	if line := checkLine(upgrade.Unstamped, "v0.4.0", false, bareUpgrade); !strings.Contains(line, "this build carries no release version") {
		t.Errorf("an unstamped build says %q, without naming the build as the problem", line)
	}
}

// The restart advice names a profile, so the commands it offers have to act on
// that profile: a bare `yad service install` would bootstrap a supervised unit
// for the default profile, which is a state change nobody asked for (0028).
func TestRestartAdviceCarriesTheProfile(t *testing.T) {
	for _, c := range []struct{ profile, wantService, wantDaemon string }{
		{"work", "`yad --profile work service install`", "`yad --profile work daemon restart`"},
		{"default", "`yad service install`", "`yad daemon restart`"},
	} {
		line := restartAdvice(config.Paths{Profile: c.profile, Config: t.TempDir(), Data: t.TempDir()})
		if !strings.Contains(line, c.wantService) {
			t.Errorf("profile %s: advice %q, want it to offer %s", c.profile, line, c.wantService)
		}
		if !strings.Contains(line, c.wantDaemon) {
			t.Errorf("profile %s: advice %q, want it to offer %s", c.profile, line, c.wantDaemon)
		}
	}
}

// Three rounds of review found three different defects in this one sentence,
// each time because it offered a command built from a profile that was not the
// profile needing the command. The rule that ends that: offer one only where
// the profile whose lock was read is the profile that needs restarting.
//
// The branch that must offer nothing is the one where no runner was found —
// there the runner in question is under some other profile, and every command
// available here *starts* something rather than restarting it.
func TestRestartNoteOffersACommandOnlyWhereItKnowsTheProfile(t *testing.T) {
	commands := []string{"service install", "daemon restart"}

	t.Run("no runner under this profile", func(t *testing.T) {
		note := restartNote(config.Paths{Profile: "default"}, 0, false, nil)
		for _, c := range commands {
			if strings.Contains(note, c) {
				t.Errorf("note %q offers %q against the one profile that does not need it", note, c)
			}
		}
		if !strings.Contains(note, "under that profile") {
			t.Errorf("note %q does not say where the restart has to happen", note)
		}
	})

	t.Run("a runner is running here", func(t *testing.T) {
		note := restartNote(config.Paths{Profile: "work"}, 4711, true, nil)
		if !strings.Contains(note, "pid 4711") {
			t.Errorf("note %q does not name the runner it found", note)
		}
		for _, want := range []string{"`yad --profile work service install`", "`yad --profile work daemon restart`"} {
			if !strings.Contains(note, want) {
				t.Errorf("note %q, want it to offer %s", note, want)
			}
		}
		// A machine with two runners is where this matters most, and it is the
		// branch the warning went missing from.
		if !strings.Contains(note, "any other profile") {
			t.Errorf("note %q drops the warning about other profiles exactly where a second runner is most likely", note)
		}
	})

	t.Run("the lock could not be read", func(t *testing.T) {
		// A runner may be there, and if it is, it is this profile's — so the
		// advice is the right advice, and the uncertainty is stated.
		note := restartNote(config.Paths{Profile: "work"}, 0, false, errors.New("permission denied"))
		if !strings.Contains(note, "could not tell") || !strings.Contains(note, "permission denied") {
			t.Errorf("note %q does not report that the state is unknown", note)
		}
		if !strings.Contains(note, "--profile work") {
			t.Errorf("note %q drops the profile it was asked about", note)
		}
	})
}

func TestCheckLineOnANamedTag(t *testing.T) {
	for _, state := range everyState {
		line := checkLine(state, "v0.1.0", true, bareUpgrade)
		if strings.Contains(line, "newest release") {
			t.Errorf("state %v says %q, calling a named tag the newest release", state, line)
		}
		if !strings.Contains(line, "--tag v0.1.0") {
			t.Errorf("state %v says %q, offering a command that installs a different release", state, line)
		}
	}
}
