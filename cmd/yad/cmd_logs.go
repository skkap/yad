package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/skkap/yad/internal/logfile"
)

// daemonLogs prints the daemon's log: the last -n lines, then with -f what
// follows, until Ctrl-C.
func daemonLogs(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("daemon logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "keep printing lines as the daemon writes them")
	n := fs.Int("n", 50, "how many lines to print first")
	raw := fs.Bool("json", false, "print the JSON lines as written")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *n < 0 {
		return fmt.Errorf("-n must be zero or more, got %d", *n)
	}
	out := io.Writer(&logPrinter{w: w, raw: *raw})
	lines, err := logfile.Tail(g.paths.Log(), *n)
	if err != nil {
		return err
	}
	if len(lines) == 0 && !*follow && *n > 0 {
		fmt.Fprintf(w, "no log yet at %s — the daemon writes it once started\n", g.paths.Log())
		return nil
	}
	for _, l := range lines {
		io.WriteString(out, l+"\n")
	}
	if !*follow {
		return nil
	}
	return logfile.Follow(ctx, g.paths.Log(), out)
}

// logPrinter renders JSON log lines for reading: time, level, message, then
// the attributes. A line that is not JSON is printed as it is.
type logPrinter struct {
	w   io.Writer
	raw bool
}

func (p *logPrinter) Write(b []byte) (int, error) {
	for line := range strings.Lines(string(b)) {
		if p.raw {
			io.WriteString(p.w, line)
			continue
		}
		io.WriteString(p.w, renderLogLine(strings.TrimRight(line, "\n"))+"\n")
	}
	return len(b), nil
}

func renderLogLine(line string) string {
	var rec map[string]any
	if json.Unmarshal([]byte(line), &rec) != nil {
		return line
	}
	var b strings.Builder
	if s, ok := rec["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			s = t.Local().Format("2006-01-02 15:04:05")
		}
		b.WriteString(s)
	}
	fmt.Fprintf(&b, " %-5v %v", rec["level"], rec["msg"])
	keys := make([]string, 0, len(rec))
	for k := range rec {
		if k != "time" && k != "level" && k != "msg" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := fmt.Sprint(rec[k])
		if s, ok := rec[k].(string); ok && (s == "" || strings.ContainsAny(s, " \"=")) {
			v = fmt.Sprintf("%q", s)
		}
		fmt.Fprintf(&b, " %s=%s", k, v)
	}
	return b.String()
}
