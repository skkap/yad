package main

import (
	"fmt"

	"github.com/skkap/yad/internal/config"
)

// The commands the CLI names but does not build. Each refusal says where the
// work actually is and what to do meanwhile, so an owner who types one is not
// sent to an epic that has already finished.

// disconnectTask is where `yad disconnect` is being designed: retiring a
// connection while a daemon is running it is the open question, not the code.
const disconnectTask = "DEV-81"

func cmdDisconnect(g global) error {
	return fmt.Errorf("`yad disconnect` is not built: how it retires a connection while the daemon is running it is still being designed, in %s (Zumino yad/dev). Meanwhile `%s` stops this runner taking work from every hub, letting the runs it holds finish first",
		disconnectTask, g.paths.Command("daemon", "stop"))
}

// accountUseRefusal is for `yad account use`, which no task plans: the order
// runs take is config.toml's own list, and editing that list is how it is
// rearranged.
func accountUseRefusal(p config.Paths, args []string) error {
	section := "[harness.<id>]"
	if len(args) > 0 && checkHarness(args[0]) == nil {
		section = "[harness." + args[0] + "]"
	}
	return fmt.Errorf("`yad account use` does not exist: the order of `accounts` under %s in %s is the order runs take, so put the account you want first there. `%s` shows the accounts, and `%s` makes a running daemon read the new order",
		section, p.ConfigFile(), p.Command("account", "list"), p.Command("daemon", "restart"))
}
