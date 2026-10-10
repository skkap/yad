//go:build !darwin && !linux

package main

import "os"

// terminalEchoOff knows no terminal off darwin and linux, where yad publishes
// no release: stdin is read as a pipe is.
func terminalEchoOff(*os.File) (restore func(), isTerm bool, err error) {
	return nil, false, nil
}
