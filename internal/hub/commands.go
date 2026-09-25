package hub

import (
	"github.com/skkap/yad/internal/shellword"
	"github.com/skkap/yad/internal/upgrade"
)

// The yad commands a hub's answers name. Each is pasted where it is read, so
// each is built from argv (AGENTS.md) and carries what its reader's shell
// needs. What the hub cannot know — which profile on the runner's machine is
// this runner, which of the caller's profiles holds the admin token, the URL
// the caller reaches this hub by — is a placeholder saying what goes there:
// pasted unfilled it is refused as a profile or URL nobody has, rather than
// acting on the default runner or hub in its place.
const (
	runnerProfile = "<runner profile>"
	callerProfile = "<profile holding the admin token>"
	callerHubURL  = "<hub url>"
)

// runnerCommand is a yad command for the runner's owner, run on the runner's
// machine as that runner's profile.
func runnerCommand(args ...string) string {
	return shellword.Command(append([]string{"yad", "--profile", runnerProfile}, args...)...)
}

// serviceCommand is a `yad hub` command for whoever called the service API,
// run wherever they hold the admin token, against this hub.
func serviceCommand(verb string, args ...string) string {
	return shellword.Command(append([]string{"yad", "--profile", callerProfile, "hub", verb, "--hub", callerHubURL}, args...)...)
}

// upgradeCommand is the `yad upgrade` a runner below this hub's floor runs.
// A fork's install fetches from YAD_REPO, which only that machine knows.
func upgradeCommand() string {
	return upgrade.Command("<the repository yad was installed from>", runnerCommand)
}

// hubCommand is a yad command run on this hub's machine against the database
// it serves: Options.Command's when the hub was given one. Without it the hub
// knows neither the profile nor the --db it was opened with, and both are
// placeholders.
func (h *Hub) hubCommand(args ...string) string {
	if h != nil && h.command != nil {
		return h.command(args...)
	}
	argv := append([]string{"yad", "--profile", "<hub profile>"}, args...)
	return shellword.Command(append(argv, "--db", "<the database yad hub serve was given>")...)
}

// adminTokenAction is the next action for a service API call whose admin
// token this hub will not take. h may be nil, where no hub is at hand.
func adminTokenAction(h *Hub) string {
	return "create one on the hub's machine with `" + h.hubCommand("hub", "admin-token", "create") + "`, and send it as the bearer token"
}

// noRunnerAction is the next action for a runner id this hub does not know.
func noRunnerAction() string {
	return "check the runner id: its daemon prints it when it starts, and `" + serviceCommand("runners") + "` lists the runners this hub knows"
}
