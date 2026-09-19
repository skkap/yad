package hub

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	v1 "github.com/skkap/yad/protocol/v1"
)

// A version floor is compared on the release core alone — major.minor.patch.
// yad stamps its version with `git describe`, so a build calls itself
// "v0.3.1-4-gabc1234": four commits *after* v0.3.1. Semver would read that
// suffix as a prerelease and sort it *before* v0.3.1, refusing a runner that
// is in fact newer than the floor; reading both as 0.3.1 refuses neither.
type version [3]int

// meetsFloor reports whether a runner's reported version is at or above the
// hub's floor. A version either side cannot parse — "dev" from an unstamped
// build, a floor an operator mistyped — meets it: locking a machine out over a
// string nobody understands costs more than the stale runner it might catch.
func meetsFloor(reported, floor string) bool {
	want, ok := parseVersion(floor)
	if !ok {
		return true
	}
	got, ok := parseVersion(reported)
	if !ok {
		return true
	}
	return !older(got, want)
}

func older(a, b version) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// parseVersion reads "v0.4", "0.4.1" or "v0.4.1-4-gabc1234"; missing
// components are zero, and anything else is not a version.
func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		v[i] = n
	}
	return v, true
}

// ValidateMinVersion refuses a floor the hub could not enforce, so an operator
// who mistypes one hears it at start rather than never seeing a refusal.
func ValidateMinVersion(s string) error {
	if s == "" {
		return nil
	}
	if _, ok := parseVersion(s); !ok {
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
		fmt.Sprintf("run `yad upgrade` on that machine to %s or newer, then start the runner again", h.minVersion))
}
