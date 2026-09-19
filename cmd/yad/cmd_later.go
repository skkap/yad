package main

import "fmt"

// arrivesIn maps each command that exists in the CLI but not yet in the runner
// to the epic that builds it, so the refusal names the next action rather than
// saying "not implemented".
var arrivesIn = map[string]string{
	"disconnect":  "E7",
	"account":     "E6",
	"conformance": "E7",
	"upgrade":     "E9 (backlog — decision 0018)",
}

func notYet(cmd string, _ []string) error {
	return fmt.Errorf("`yad %s` arrives in epic %s (Zumino yad/dev) — see ARCHITECTURE.md §9", cmd, arrivesIn[cmd])
}
