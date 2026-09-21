package upgrade

import "github.com/skkap/yad/internal/shellword"

// RepoEnv names the fork an install came from: scripts/install.sh reads it,
// and so does `yad upgrade`, since nothing records the repository at install
// time.
const RepoEnv = "YAD_REPO"

// Command is a `yad upgrade` for the owner to paste, from repo — the value of
// RepoEnv this upgrade ran with, empty for DefaultRepo.
//
// The variable is carried because an upgrade pasted without it fetches
// upstream's release over a fork's binary: the install source silently lost
// at the one moment the binary changes. A variable already exported in the
// reader's shell is repeated harmlessly.
func Command(repo string, args ...string) string {
	cmd := shellword.Command(append([]string{"yad", "upgrade"}, args...)...)
	if repo == "" {
		return cmd
	}
	return RepoEnv + "=" + shellword.Quote(repo) + " " + cmd
}
