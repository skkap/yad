// Package hostool probes the non-harness tools a run may need — git, gh and
// docker — and how far each one actually works: whether gh is logged in and to
// which host, whether docker's daemon answers at all.
//
// Absence is data, exactly as for harnesses. A machine with no docker, a gh
// nobody has signed in, and a `--version` that hangs are three facts about the
// machine; each is reported in the capability document and none of them stops a
// runner registering.
//
// Epic E7 (Zumino yad/dev).
package hostool
