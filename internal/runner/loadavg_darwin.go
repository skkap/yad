package runner

import "syscall"

// loadAverage is the machine's one-minute load average, as uptime(1) prints
// it: runnable and uninterruptible processes, not scaled by CPU count and not
// only this runner's. A sysctl that failed reports 0 — health carries load on
// every sync, so it must not fail the sync.
func loadAverage() float64 {
	s, err := syscall.Sysctl("vm.loadavg")
	if err != nil {
		return 0
	}
	v, _ := parseSysctlLoadavg([]byte(s))
	return v
}
