// Package control is the runner's Unix control socket, the only socket it
// opens (decision 0004): `yad status` and `yad daemon stop|status` talk to the
// running process through it. The socket is 0600 in the profile's data
// directory, and its protocol is internal and unversioned — only this binary
// speaks it, one JSON request and one JSON answer per connection.
//
// Beside the socket sits yad.lock, which the daemon holds with flock(2) for
// its whole life. The lock, not the socket file, says whether a daemon is
// running: the kernel releases it however the process ends, so a crash leaves
// a socket file behind but never a held lock (decision 0026).
package control
