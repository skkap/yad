package hostool

import "context"

// Tool is a non-harness executable YAD looks for.
//
// None of these fields reaches a hub: how a binary is found and how it is
// questioned are this machine's business, and Detected is translated into the
// capability document's HostTool by internal/capability.
type Tool struct {
	ID          string // stable; used in the protocol and on the CLI
	Binary      string
	VersionArgs []string
	EnvPath     string // env var overriding the binary path, for GUI-launched daemons with no shell PATH
	// status is the second probe — what "the binary is installed" does not
	// answer. nil for a tool where being installed is the whole story.
	status func(ctx context.Context, path string, d *Detected)
}

// Catalog is every host tool YAD probes, in display order.
//
// Three, and deliberately only three (decided 2026-09-19, Zumino yad/dev
// DEV-31): git, gh and docker are what a run needs from the machine itself
// before it can do anything — clone a source, open a pull request, run a
// container. A run that wants anything else installs it in its own workdir. A
// fourth entry here would cost every runner two more spawns every probe
// interval and every hub a field it has to learn.
func Catalog() []Tool {
	return []Tool{
		{ID: "git", Binary: "git", VersionArgs: []string{"--version"}, EnvPath: "YAD_GIT_PATH"},
		{ID: "gh", Binary: "gh", VersionArgs: []string{"--version"}, EnvPath: "YAD_GH_PATH", status: ghStatus},
		{ID: "docker", Binary: "docker", VersionArgs: []string{"--version"}, EnvPath: "YAD_DOCKER_PATH", status: dockerStatus},
	}
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
