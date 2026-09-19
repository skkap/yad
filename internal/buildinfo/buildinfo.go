// Package buildinfo carries the values stamped into the binary at link time.
package buildinfo

// Version is the release the binary was built from. The Makefile overrides it
// with `git describe`; a build with no ldflags at all says "dev" rather than
// lying. A *stamped* build is not always a version: `git describe --tags
// --always` falls back to a short SHA where no tag is reachable, which is what
// ParseNumber is written to refuse.
var Version = "dev"

// Commit is the git revision the binary was built from, or "" when unstamped.
var Commit = ""
