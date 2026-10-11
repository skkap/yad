//go:build !unix

package selfupdate

import "io/fs"

// chownGroup does nothing where there are no Unix groups to share by.
func chownGroup(string, fs.FileInfo) {}

// openNonblock is nothing where there are no FIFOs to block on.
const openNonblock = 0
