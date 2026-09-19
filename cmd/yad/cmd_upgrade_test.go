package main

import (
	"strings"
	"testing"

	"github.com/skkap/yad/internal/upgrade"
)

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

// Every state names what the owner would type next, because "up to date" with
// no way to override it is what sends someone to reinstall by hand.
func TestCheckLineCarriesANextAction(t *testing.T) {
	for _, state := range []upgrade.State{upgrade.Behind, upgrade.Current, upgrade.Ahead, upgrade.Unknown} {
		line := checkLine(state, "v0.4.0")
		if !strings.Contains(line, "yad upgrade") {
			t.Errorf("state %v says %q, with nothing to type next", state, line)
		}
		if !strings.Contains(line, "v0.4.0") {
			t.Errorf("state %v says %q, without naming the release", state, line)
		}
	}
}
