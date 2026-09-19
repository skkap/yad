package buildinfo

import (
	"strconv"
	"strings"
)

// A Number is the comparable core of a version string — major.minor.patch.
// yad stamps its version with `git describe`, so a build calls itself
// "v0.3.1-4-gabc1234": four commits *after* v0.3.1. Semver would read that
// suffix as a prerelease and sort it *before* v0.3.1, which would make a build
// newer than a release look older than it; reading both as 0.3.1 makes them
// equal, which is the smaller lie and the safer one.
type Number [3]int

// ParseNumber reads "v0.4", "0.4.1" or "v0.4.1-4-gabc1234"; a missing third
// component is zero, and anything else is not a version number — "dev", the
// unstamped default, among them.
//
// A bare number is refused on purpose. The Makefile stamps with
// `git describe --tags --always`, which falls back to a short SHA in a
// checkout with no reachable tag — an untagged repository, or a shallow clone
// — and about one short SHA in thirty is all decimal digits. Read as a version
// it would be an enormous major, which is worse than unreadable: it makes the
// build look newer than every release rather than unstamped.
func ParseNumber(s string) (Number, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return Number{}, false
	}
	var n Number
	for i, p := range parts {
		d, err := strconv.Atoi(p)
		if err != nil {
			return Number{}, false
		}
		n[i] = d
	}
	return n, true
}

// Older reports whether n names an earlier release than other.
func (n Number) Older(other Number) bool {
	for i := range n {
		if n[i] != other[i] {
			return n[i] < other[i]
		}
	}
	return false
}
