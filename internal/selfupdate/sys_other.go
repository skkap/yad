//go:build !unix

package selfupdate

import "io/fs"

// chownGroup does nothing where there are no Unix groups to share by.
func chownGroup(string, fs.FileInfo) {}

// CanReplace answers yes where it cannot ask, so a record that cannot be
// written is always a warning there.
func CanReplace(string) bool { return true }

// openNonblock is nothing where there are no FIFOs to block on.
const openNonblock = 0
