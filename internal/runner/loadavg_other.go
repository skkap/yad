//go:build !linux && !darwin

package runner

// loadAverage is 0 where the machine has no load average this binary can read.
// The shipped targets are linux and darwin; this keeps the package building
// for anyone compiling it elsewhere, and 0 is what health then carries.
func loadAverage() float64 { return 0 }
