// Package buildinfo carries the values stamped into the binary at link time.
package buildinfo

import v1 "github.com/skkap/yad/protocol/v1"

// Version is the release the binary was built from. The Makefile overrides it
// with `git describe`; a build with no ldflags at all says "dev" rather than
// lying. A *stamped* build is not always a version: `git describe --tags
// --always` falls back to a short SHA where no tag is reachable, which is what
// ParseNumber is written to refuse.
var Version = "dev"

// Commit is the git revision the binary was built from, or "" when unstamped.
var Commit = ""

// About is `yad version --json`: what a binary is, in a form another yad can
// read. A runner taking a release over asks the downloaded binary this before
// it puts it in place (decision 0071), so the fields are a contract with
// every later release: added to, never renamed or removed.
type About struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	// Protocols are the runner protocol majors this build speaks, each as its
	// Yad-Protocol header carries it: "1".
	Protocols []string `json:"protocols"`
}

// Current is this build's About. It speaks the one protocol major it is
// compiled against; a build that speaks two lists both.
func Current() About {
	return About{Version: Version, Commit: Commit, Protocols: []string{v1.Version}}
}
