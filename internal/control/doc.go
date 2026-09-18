// Package control is the runner's Unix control socket, the only socket it
// opens (decision 0004): `yad status`, `yad daemon stop`, `yad sessions` and
// `yad account use` talk to the running process through it. The socket is 0600
// in the profile's data directory, and its protocol is internal and
// unversioned — only this binary speaks it.
//
// Epic E3 (Zumino yad/dev).
package control
