package adapter

import "github.com/skkap/yad/internal/shellword"

// YadCommand is a yad command for the runner that ran the turn: Spec.Yad when
// the runner set it, which carries its profile, and a bare one otherwise.
func (s Spec) YadCommand(args ...string) string {
	if s.Yad != nil {
		return s.Yad(args...)
	}
	return shellword.Command(append([]string{"yad"}, args...)...)
}

// HarnessCheck is a harness command for the owner to run by hand at the
// machine, set apart in backticks, pointed at the home the turn ran in.
//
// Without the home variable the check reads the owner's own default home and
// answers about a different login entirely — worse than no suggestion, since
// it looks like it worked. The home itself is not printed: it is a path under
// the owner's home, and a run's error goes to a hub (DEV-67). It stays a
// placeholder, and the sentence names the yad command that shows it.
//
// The harness is named, not its resolved path, for the same reason; a runner
// pointed at another binary by the harness's path variable is one whose owner
// knows it is.
func (s Spec) HarnessCheck(harness string, args ...string) string {
	cmd := shellword.Command(append([]string{harness}, args...)...)
	if s.Home == "" || s.HomeVar == "" {
		return "`" + cmd + "`"
	}
	account := "the run's account"
	if s.Account != "" {
		account = "account " + shellword.Quote(s.Account)
	}
	return "`" + s.HomeVar + "=" + shellword.Quote("<home>") + " " + cmd + "`, with <home> the HOME `" +
		s.YadCommand("account", "list") + "` shows for " + account
}
