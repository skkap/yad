package runner

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/skkap/yad/internal/supervise"
)

// leftRunningNames is how many processes a left_running event names before
// it counts the rest: enough for a test run's workers, short enough to read.
const leftRunningNames = 20

// leftRunningText is the left_running event's text. Each process is named by
// the kernel's name for its command and how long it had run, and by nothing
// else: its argv and environment can hold a token, and its pid means nothing
// off this machine (decision 0083).
func leftRunningText(left []supervise.Leftover) string {
	var b strings.Builder
	if len(left) == 1 {
		b.WriteString("1 process the run's tool work started was still running when the turn ended: ")
	} else {
		fmt.Fprintf(&b, "%d processes the run's tool work started were still running when the turn ended: ", len(left))
	}
	for i, l := range left {
		if i == leftRunningNames {
			fmt.Fprintf(&b, ", and %d more", len(left)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (running %s)", l.Name, l.Age.Round(time.Second))
	}
	return b.String()
}

// leftRunningLog is the same list for the runner's own log, which stays on
// the machine and so may carry each pid.
func leftRunningLog(left []supervise.Leftover) string {
	parts := make([]string, len(left))
	for i, l := range left {
		parts[i] = l.Name + "[" + strconv.Itoa(l.PID) + "]"
	}
	return strings.Join(parts, " ")
}
