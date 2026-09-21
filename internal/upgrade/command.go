package upgrade

import "github.com/skkap/yad/internal/shellword"

// RepoEnv names the fork an install came from: scripts/install.sh reads it,
// and so does `yad upgrade`, since nothing records the repository at install
// time.
const RepoEnv = "YAD_REPO"

// Command is a `yad upgrade` for the owner to paste, from repo — the value of
// RepoEnv this upgrade ran with, empty for DefaultRepo. yad builds the `yad …`
// part, carrying the profile the upgrade ran under; nil builds a bare one.
//
// The variable is carried because an upgrade pasted without it fetches
// upstream's release over a fork's binary: the install source silently lost
// at the one moment the binary changes. The profile is carried because the
// upgrade ends by reading that profile's runner, to say which one still runs
// the old binary. A variable already exported in the reader's shell is
// repeated harmlessly.
func Command(repo string, yad func(args ...string) string, args ...string) string {
	argv := append([]string{"upgrade"}, args...)
	var cmd string
	if yad != nil {
		cmd = yad(argv...)
	} else {
		cmd = shellword.Command(append([]string{"yad"}, argv...)...)
	}
	if repo == "" {
		return cmd
	}
	return RepoEnv + "=" + shellword.Quote(repo) + " " + cmd
}
