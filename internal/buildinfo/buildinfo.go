// Package buildinfo carries the values stamped into the binary at link time.
package buildinfo

// Version is the release the binary was built from. The Makefile overrides it
// with `git describe`; an unstamped build says "dev" rather than lying.
var Version = "dev"

// Commit is the git revision the binary was built from, or "" when unstamped.
var Commit = ""
