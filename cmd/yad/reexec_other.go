//go:build !unix

package main

import "fmt"

// reexec has no exec(2) to call off unix, where yad publishes no release.
var reexec = func(path string) error {
	return fmt.Errorf("self-update re-executes the runner in place, which needs a unix system — %s is installed; start the runner again to run it", path)
}
