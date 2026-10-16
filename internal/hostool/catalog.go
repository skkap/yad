package hostool

import (
	"context"

	"github.com/skkap/yad/internal/probe"
)

// Tool is a non-harness executable YAD looks for.
//
// Only ID reaches a hub. How a binary is found and how it is questioned are
// this machine's business, and internal/capability translates a Detected into
// the capability document's HostTool.
type Tool struct {
	ID          string // stable; used in the protocol and on the CLI
	Binary      string
	VersionArgs []string
	EnvPath     string // env var overriding the binary path, for GUI-launched daemons with no shell PATH
	// status is the second probe — what "the binary is installed" does not
	// answer. nil for a tool where being installed is the whole story.
	status func(ctx context.Context, bin probe.Found, d *Detected)
}

// Catalog is every host tool that is one binary, in display order: the ones a
// run is handed by name (Locate, Links) and an owner points at with
// YAD_<ID>_PATH. The browser is a host tool too, reported after these, but it
// is no one binary — Chrome, Chromium or Playwright's own, wherever each
// installs itself — so it is searched for rather than catalogued (browser.go).
//
// git, gh and docker were three deliberately (decided 2026-09-19, YAD-31): what
// a run needs from the machine itself before it can do anything — clone a
// source, open a pull request, run a container. A run that wants anything else
// installs it in its own workdir. The browser is the one exception since
// (decision 0087): a headless browser is not something a run can install in
// a workdir on a machine whose owner has not, and a hub sending a verifier its
// screenshots has to know where one is.
func Catalog() []Tool {
	return []Tool{
		{ID: "git", Binary: "git", VersionArgs: []string{"--version"}, EnvPath: "YAD_GIT_PATH"},
		{ID: "gh", Binary: "gh", VersionArgs: []string{"--version"}, EnvPath: "YAD_GH_PATH", status: ghStatus},
		{ID: "docker", Binary: "docker", VersionArgs: []string{"--version"}, EnvPath: "YAD_DOCKER_PATH", status: dockerStatus},
	}
}

// IDs is every host tool a capability document reports, in its order: the
// catalog's, then the browser.
func IDs() []string {
	var ids []string
	for _, t := range Catalog() {
		ids = append(ids, t.ID)
	}
	return append(ids, BrowserID)
}

// Lookup returns the catalog entry with this id.
func Lookup(id string) (Tool, bool) {
	for _, t := range Catalog() {
		if t.ID == id {
			return t, true
		}
	}
	return Tool{}, false
}
