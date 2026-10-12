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
// off this machine (decision 0083). With stop, the owner's [supervise]
// stop_left_running, it ends by saying whether yad stopped them (0086).
func leftRunningText(left []supervise.Leftover, stop bool) string {
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
	if stop {
		b.WriteString("; " + stoppedText(left))
	}
	return b.String()
}

// stoppedText says how many of left yad stopped. One it did not was still
// running after the ladder, shared a group with the harness or a helper of
// its, which yad never signals, or went on its own before yad signalled it
// — or the process table could not be read to tell its group from
// another's. "Did not", not "could not": several of those are yad's choice.
func stoppedText(left []supervise.Leftover) string {
	n := 0
	for _, l := range left {
		if l.Stopped {
			n++
		}
	}
	switch {
	case n == len(left) && n == 1:
		return "yad stopped it"
	case n == len(left):
		return "yad stopped them"
	case n == 0 && len(left) == 1:
		return "yad did not stop it"
	case n == 0:
		return "yad did not stop them"
	default:
		return fmt.Sprintf("yad stopped %d of them and did not stop %d", n, len(left)-n)
	}
}

// leftRunningLog is the same list for the runner's own log, which stays on
// the machine and so may carry each pid, and with stop whether yad stopped
// it.
func leftRunningLog(left []supervise.Leftover, stop bool) string {
	parts := make([]string, len(left))
	for i, l := range left {
		parts[i] = l.Name + "[" + strconv.Itoa(l.PID)
		switch {
		case stop && l.Stopped:
			parts[i] += " stopped"
		case stop:
			parts[i] += " not stopped"
		}
		parts[i] += "]"
	}
	return strings.Join(parts, " ")
}
