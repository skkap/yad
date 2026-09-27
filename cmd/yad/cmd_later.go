package main

import (
	"fmt"

	"github.com/skkap/yad/internal/config"
)

// The commands the CLI names but does not build. Each refusal says where the
// work actually is and what to do meanwhile, so an owner who types one is not
// sent to an epic that has already finished.

// cmdDisconnect names no ticket: the tracker it is designed in is not public,
// and a reference the reader cannot open is not a place the work is.
func cmdDisconnect(g global) error {
	return fmt.Errorf("`yad disconnect` is not built: how it retires a connection while the daemon is running it is still being designed. Meanwhile `%s` stops this runner taking work from every hub, letting the runs it holds finish first",
		g.paths.Command("daemon", "stop"))
}

// accountUseRefusal is for `yad account use`, which no task plans, because
// there is nothing for it to set: a run takes the free account whose usage
// window refills soonest (decision 0039), and config.toml's list only says
// which accounts take part and breaks ties. Telling the owner to reorder that
// list would promise a choice the runner does not make.
func accountUseRefusal(p config.Paths) error {
	return fmt.Errorf("`yad account use` does not exist, because no account is picked by hand: of a harness's free accounts, a run takes the one whose usage window refills soonest, so quota about to refill is spent rather than wasted (decision 0039). The order of `accounts` in config.toml only breaks ties. `%s` shows each account's state and when it refills",
		p.Command("account", "list"))
}
