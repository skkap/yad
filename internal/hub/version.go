package hub

import (
	"fmt"
	"net/http"

	"github.com/skkap/yad/internal/buildinfo"
	v1 "github.com/skkap/yad/protocol/v1"
)

// meetsFloor reports whether a runner's reported version is at or above the
// hub's floor, compared on the release core alone (buildinfo.Number says why).
// A version either side cannot parse — "dev" from an unstamped build, a floor
// an operator mistyped — meets it: locking a machine out over a string nobody
// understands costs more than the stale runner it might catch.
func meetsFloor(reported, floor string) bool {
	want, ok := buildinfo.ParseNumber(floor)
	if !ok {
		return true
	}
	got, ok := buildinfo.ParseNumber(reported)
	if !ok {
		return true
	}
	return !got.Older(want)
}

// ValidateMinVersion refuses a floor the hub could not enforce, so an operator
// who mistypes one hears it at start rather than never seeing a refusal.
func ValidateMinVersion(s string) error {
	if s == "" {
		return nil
	}
	if _, ok := buildinfo.ParseNumber(s); !ok {
		return fmt.Errorf("min version %q is not a version number — write it as 0.4.0 or v0.4.0", s)
	}
	return nil
}

// refuseOld is the refusal a runner below this hub's floor gets at register
// and at every sync, or nil when it may carry on. 426 is the status the
// protocol already uses to say "upgrade and come back" (ARCHITECTURE.md §2),
// and both numbers are named: "too old" without the floor leaves the operator
// guessing what to upgrade to.
func (h *Hub) refuseOld(runnerVersion string) error {
	if meetsFloor(runnerVersion, h.minVersion) {
		return nil
	}
	return Fail(http.StatusUpgradeRequired, v1.CodeVersionTooOld,
		fmt.Sprintf("this hub takes runners from yad %s; this one is %s", h.minVersion, runnerVersion),
		fmt.Sprintf("on that machine, `%s` to %s or newer, then start the runner again", upgradeCommand(), h.minVersion))
}
