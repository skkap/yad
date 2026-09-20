package runner

import "os"

// loadAverage is the machine's one-minute load average, as uptime(1) prints
// it: runnable and uninterruptible processes, not scaled by CPU count and not
// only this runner's. A machine with no /proc reports 0 — health carries load
// on every sync, so a read that failed must not fail the sync.
func loadAverage() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	v, _ := parseProcLoadavg(string(b))
	return v
}
