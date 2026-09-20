package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubapiclient"
)

// hubFlags are how `submit` and `watch` find a hub and prove they may use it.
// The token comes from a 0600 file and never from argv or the environment:
// unlike a registration token (decision 0020) it is long-lived and opens every
// run on the hub.
type hubFlags struct {
	url, tokenFile *string
}

func addHubFlags(fs *flag.FlagSet, g global) hubFlags {
	def := os.Getenv("YAD_HUB_URL")
	if def == "" {
		def = "http://" + defaultHubListen
	}
	return hubFlags{
		url:       fs.String("hub", def, "the hub's URL, as `yad hub serve` prints it, without /api/v1 ($YAD_HUB_URL)"),
		tokenFile: fs.String("token-file", g.paths.HubAdminToken(), "file holding the admin token (0600)"),
	}
}

func (f hubFlags) client() (*hubapiclient.Client, error) {
	tok, err := config.ReadSecret(*f.tokenFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no admin token at %s — on the hub's machine, `yad hub admin-token create` saves one there; elsewhere, save one at 0600 and pass --token-file", *f.tokenFile)
	}
	if err != nil {
		return nil, fmt.Errorf("admin token file %s: %w — revoke that token on the hub and create another", *f.tokenFile, err)
	}
	return hubapiclient.New(*f.url, tok)
}

const submitUsage = "usage: yad hub submit --harness h --model m [--context text | --context-file f] [--session id | --new-session id] [--git url [--base ref] [--branch name] | --path dir] [--run-id id] [--watch] <instruction | ->"

// cmdHubSubmit queues a run and prints its id — alone on stdout, so a script
// can capture it — or, with --watch, follows it to its end.
func cmdHubSubmit(ctx context.Context, g global, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hub submit", flag.ContinueOnError)
	hf := addHubFlags(fs, g)
	harness := fs.String("harness", "", "the harness to run, e.g. claude or codex (required)")
	model := fs.String("model", "", "the model to point it at, e.g. haiku for claude or gpt-5.6-luna for codex (required)")
	contextText := fs.String("context", "", "context appended to the harness's system prompt")
	contextFile := fs.String("context-file", "", "read the context from this file")
	session := fs.String("session", "", "continue this session, which the hub already has")
	newSession := fs.String("new-session", "", "start a session with this id (default: a new one with a generated id)")
	runID := fs.String("run-id", "", "the run's id, to make a retried submit safe (default: generated)")
	watch := fs.Bool("watch", false, "follow the run until it ends, as `yad hub watch` does")
	gitURL := fs.String("git", "", "a repository the run works in, checked out as a worktree on the runner")
	base := fs.String("base", "", "with --git: where a new branch is cut from (default: the repository's default branch)")
	branch := fs.String("branch", "", "with --git: the branch the run works on (default: one named after the session)")
	path := fs.String("path", "", "a directory on the runner's machine the run works in, in place — the runner's owner must allow it")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *harness == "" || *model == "" {
		return errors.New(submitUsage)
	}
	if *session != "" && *newSession != "" {
		return errors.New("--session continues a session and --new-session starts one — pass one of them")
	}
	if *contextText != "" && *contextFile != "" {
		return errors.New("pass --context or --context-file, not both")
	}
	if *gitURL != "" && *path != "" {
		return errors.New("--git and --path each name where the run works — pass one of them")
	}
	if *gitURL == "" && (*base != "" || *branch != "") {
		return errors.New("--base and --branch belong to a --git repository — name it with --git")
	}
	instruction := pos[0]
	if instruction == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("read the instruction from stdin: %w", err)
		}
		instruction = string(b)
	}
	if strings.TrimSpace(instruction) == "" {
		return errors.New("the instruction is empty — say what the run should do")
	}
	brief := v1.Brief{Instruction: instruction, Context: *contextText}
	if *contextFile != "" {
		b, err := os.ReadFile(*contextFile)
		if err != nil {
			return fmt.Errorf("read --context-file: %w", err)
		}
		brief.Context = string(b)
	}
	req := hubapi.SubmitRequest{RunID: *runID, Harness: *harness, Model: *model, Brief: brief}
	switch {
	case *gitURL != "":
		req.Sources = []v1.Source{{Git: &v1.GitSource{URL: *gitURL, Base: *base, Branch: *branch}}}
	case *path != "":
		req.Sources = []v1.Source{{Path: *path}}
	}
	switch {
	case *session != "":
		req.Session = &hubapi.SessionChoice{ID: *session}
	case *newSession != "":
		req.Session = &hubapi.SessionChoice{ID: *newSession, New: true}
	}
	c, err := hf.client()
	if err != nil {
		return err
	}
	run, err := c.Submit(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, run.RunID)
	fmt.Fprintf(stderr, "queued in session %s — follow it with `yad hub watch %s`\n", run.SessionID, run.RunID)
	if !*watch {
		return nil
	}
	return follow(ctx, c, run, stdout)
}

// cmdHubWatch shows a run's events as they arrive and ends with its result.
// It exits non-zero unless the run succeeded, so a script can chain on it.
func cmdHubWatch(ctx context.Context, g global, args []string, stdout, _ io.Writer) error {
	fs := flag.NewFlagSet("hub watch", flag.ContinueOnError)
	hf := addHubFlags(fs, g)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: yad hub watch [--hub url] [--token-file f] <run>")
	}
	c, err := hf.client()
	if err != nil {
		return err
	}
	run, err := c.Run(ctx, pos[0])
	if err != nil {
		return err
	}
	return follow(ctx, c, run, stdout)
}

// cmdHubControl is `yad hub cancel`, `interrupt` and `steer`. Each prints the
// run's state after the hub took the request; a run a runner holds stops or
// takes the steer at that runner's next sync, which `yad hub watch` shows.
func cmdHubControl(ctx context.Context, g global, verb string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hub "+verb, flag.ContinueOnError)
	hf := addHubFlags(fs, g)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	want := 1
	usage := "usage: yad hub " + verb + " [--hub url] [--token-file f] <run>"
	if verb == "steer" {
		want = 2
		usage = "usage: yad hub steer [--hub url] [--token-file f] <run> <text | ->"
	}
	if len(pos) != want {
		return errors.New(usage)
	}
	var text string
	if verb == "steer" {
		text = pos[1]
		if text == "-" {
			b, err := io.ReadAll(stdin)
			if err != nil {
				return fmt.Errorf("read the steer from stdin: %w", err)
			}
			text = string(b)
		}
		if strings.TrimSpace(text) == "" {
			return errors.New("the steer is empty — say what the harness should take into account")
		}
	}
	c, err := hf.client()
	if err != nil {
		return err
	}
	var run hubapi.Run
	switch verb {
	case "cancel":
		run, err = c.Cancel(ctx, pos[0])
	case "interrupt":
		run, err = c.Interrupt(ctx, pos[0])
	case "steer":
		run, err = c.Steer(ctx, pos[0], text)
	}
	if err != nil {
		return err
	}
	switch {
	case run.State.Terminal():
		fmt.Fprintf(stdout, "run %s is %s\n", run.RunID, run.State)
	case verb == "cancel":
		fmt.Fprintf(stdout, "run %s is %s; its runner cancels it at its next sync — `yad hub watch %s` shows the end\n", run.RunID, run.State, run.RunID)
	case verb == "interrupt":
		fmt.Fprintf(stdout, "run %s is %s; its runner interrupts the turn at its next sync\n", run.RunID, run.State)
	default:
		fmt.Fprintf(stdout, "run %s is %s; its runner hands the steer to the harness at its next sync\n", run.RunID, run.State)
	}
	return nil
}

// cmdHubDrain is `yad hub drain`: the runner stops taking runs at its next
// sync, finishes those it holds and exits. It prints where that stands.
func cmdHubDrain(ctx context.Context, g global, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hub drain", flag.ContinueOnError)
	hf := addHubFlags(fs, g)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: yad hub drain [--hub url] [--token-file f] <runner>")
	}
	c, err := hf.client()
	if err != nil {
		return err
	}
	r, err := c.Drain(ctx, pos[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "runner %s (%s) drains at its next sync: it takes no new runs, finishes those it holds — cancelling them after its drain wait — and exits\n", r.RunnerID, r.Name)
	if r.Draining {
		fmt.Fprintln(stdout, "its last sync already said it was draining")
	}
	return nil
}

// cmdHubCloseSession is `yad hub close-session`: the session takes no new
// run, and its runner deletes its workdir. It prints where that stands.
func cmdHubCloseSession(ctx context.Context, g global, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hub close-session", flag.ContinueOnError)
	hf := addHubFlags(fs, g)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: yad hub close-session [--hub url] [--token-file f] <session>")
	}
	c, err := hf.client()
	if err != nil {
		return err
	}
	s, err := c.CloseSession(ctx, pos[0])
	if err != nil {
		return err
	}
	switch s.State {
	case hubapi.SessionClosed:
		fmt.Fprintf(stdout, "session %s is closed (%s)\n", s.SessionID, s.CloseReason)
	default:
		fmt.Fprintf(stdout, "session %s is closing: runner %s deletes its workdir at its next sync, once no run of it is held there\n", s.SessionID, s.RunnerID)
	}
	return nil
}

func follow(ctx context.Context, c *hubapiclient.Client, run hubapi.Run, w io.Writer) error {
	p := &printer{w: w}
	p.state(run)
	end, err := c.Follow(ctx, run.RunID, 0, func(page hubapi.EventPage) error {
		for _, ev := range page.Events {
			p.event(ev)
		}
		p.state(page.Run)
		return nil
	})
	if err != nil {
		return err
	}
	return p.result(end)
}

// printer renders a run for a person. Everything it prints from the run is
// harness output, which is data: control characters are replaced, so a
// harness cannot drive the terminal it is watched on.
type printer struct {
	w           io.Writer
	shown       hubapi.RunState
	cancelShown bool
	lastText    string
}

func (p *printer) state(r hubapi.Run) {
	if r.State == p.shown {
		p.cancelling(r)
		return
	}
	p.shown = r.State
	line := "── " + string(r.State)
	if r.RunnerID != "" && !r.State.Terminal() && r.State != hubapi.RunQueued {
		line += " on runner " + clean(r.RunnerID)
	}
	if r.Reason != "" {
		line += " — " + clean(r.Reason)
	}
	if r.ResumesAt != nil {
		line += ", resumes " + r.ResumesAt.Local().Format(time.DateTime)
	}
	fmt.Fprintln(p.w, line)
	p.cancelling(r)
}

// cancelling says once that a cancel is on its way to the runner, since the
// run's state does not move until the runner acts on it.
func (p *printer) cancelling(r hubapi.Run) {
	if r.CancelRequestedAt != nil && !r.State.Terminal() && !p.cancelShown {
		p.cancelShown = true
		fmt.Fprintln(p.w, "── cancel requested; the runner stops the run at its next sync")
	}
}

// toolPreview bounds what a tool call shows: the whole input is in the
// hub, and a person watching needs to recognise the call, not read it.
const toolPreview = 200

func (p *printer) event(ev v1.Event) {
	switch ev.Kind {
	case v1.EventText:
		p.lastText = ev.Text
		fmt.Fprintln(p.w, clean(ev.Text))
	case v1.EventThinking:
		fmt.Fprintln(p.w, "· "+short(ev.Text, toolPreview))
	case v1.EventToolCall:
		if ev.Tool != nil {
			fmt.Fprintf(p.w, "→ %s %s\n", clean(ev.Tool.Name), short(ev.Tool.Input, toolPreview))
		}
	case v1.EventToolResult:
		if ev.Tool != nil {
			fmt.Fprintf(p.w, "← %s\n", short(ev.Tool.Output, toolPreview))
		}
	case v1.EventStatus:
		fmt.Fprintln(p.w, "· "+clean(ev.Status))
	case v1.EventUsage:
		if ev.Usage != nil {
			fmt.Fprintf(p.w, "· usage %s: %d in, %d out\n", clean(ev.Usage.Model), ev.Usage.Input, ev.Usage.Output)
		}
	case v1.EventError:
		if ev.Error != nil {
			fmt.Fprintf(p.w, "! %s: %s\n", clean(ev.Error.Class), clean(ev.Error.Message))
		}
	}
}

func (p *printer) result(r hubapi.Run) error {
	p.state(r)
	res := r.Result
	if res == nil {
		if r.State == hubapi.RunState(v1.RunSucceeded) {
			return nil
		}
		if r.Reason != "" {
			return fmt.Errorf("run %s ended %s: %s", r.RunID, r.State, clean(r.Reason))
		}
		return fmt.Errorf("run %s ended %s with no result from its runner", r.RunID, r.State)
	}
	// The final text is usually the last thing the harness said, already
	// shown; repeating it would print the answer twice.
	if res.FinalText != "" && res.FinalText != p.lastText {
		fmt.Fprintln(p.w, clean(res.FinalText))
	}
	fmt.Fprintf(p.w, "── %s in %s, %d tool calls\n", res.State, (time.Duration(res.Metrics.DurationMS) * time.Millisecond).Round(100*time.Millisecond), res.Metrics.ToolCalls)
	if res.State == v1.RunSucceeded {
		return nil
	}
	msg := fmt.Sprintf("run %s %s", r.RunID, res.State)
	if res.Error != nil {
		msg += ": " + clean(res.Error.Class) + ": " + clean(res.Error.Message)
	}
	return errors.New(msg)
}

// short is one line of at most n runes.
func short(s string, n int) string {
	s = strings.Join(strings.Fields(clean(s)), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// clean replaces control characters, keeping newlines and tabs.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return '�'
	}, s)
}

// cmdHubRunners is `yad hub runners`: the fleet as this hub last heard it.
// Every runner's own health, which is the answer to the question an operator
// asks first — why is that machine slow, or idle.
func cmdHubRunners(ctx context.Context, g global, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hub runners", flag.ContinueOnError)
	hf := addHubFlags(fs, g)
	asJSON := fs.Bool("json", false, "print the runners and their health as JSON")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("usage: yad hub runners [--hub url] [--token-file f] [--json] [runner]")
	}
	c, err := hf.client()
	if err != nil {
		return err
	}
	var runners []hubapi.Runner
	if len(pos) == 1 {
		r, err := c.Runner(ctx, pos[0])
		if err != nil {
			return err
		}
		runners = []hubapi.Runner{r}
	} else if runners, err = c.Runners(ctx); err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(hubapi.RunnerList{Runners: runners})
	}
	printRunners(stdout, runners, time.Now())
	return nil
}

// printRunners writes what a hub knows of each runner. Everything printed here
// came over the wire from a machine the hub does not own, so every string of
// it goes through cleanLine: a newline or an escape in a label would otherwise
// draw rows of its own in the operator's terminal.
func printRunners(w io.Writer, runners []hubapi.Runner, now time.Time) {
	if len(runners) == 0 {
		fmt.Fprintln(w, "no runners registered — `yad hub token create` makes a registration token for one")
		return
	}
	for i, r := range runners {
		if i > 0 {
			fmt.Fprintln(w)
		}
		when := "never synced"
		if r.LastSyncAt != nil {
			when = "synced " + now.Sub(*r.LastSyncAt).Round(time.Second).String() + " ago"
		}
		state := ""
		switch {
		case r.Draining:
			state = " — draining"
		case r.DrainRequestedAt != nil:
			state = " — drain asked for, not yet acknowledged"
		}
		fmt.Fprintf(w, "%s (%s) — %s%s\n", cleanLine(r.Name), cleanLine(r.RunnerID), when, state)
		if r.Health == nil {
			fmt.Fprintln(w, "  no health yet: this runner registered and has not synced")
			continue
		}
		h := r.Health
		// Free capacity, not total: health carries what is free, and the
		// capability document is where the pool's size lives.
		fmt.Fprintf(w, "  load %.2f · %d free · disk %.1f GiB · spool %d · outbox %d\n",
			h.Load, h.FreeCapacity.Total, float64(h.DiskFreeBytes)/(1<<30), h.SpoolDepth, h.OutboxDepth)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, hh := range h.Harnesses {
			ready := "not ready"
			if hh.Ready {
				ready = "ready"
			}
			if n, ok := h.FreeCapacity.ByHarness[hh.ID]; ok {
				ready += fmt.Sprintf(", %d free", n)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", cleanLine(hh.ID), ready, accountLine(hh.Accounts))
		}
		tw.Flush()
		if len(h.RecentErrors) > 0 {
			fmt.Fprintln(w, "  recent errors, newest first (the runner's own words — `yad daemon logs` on that machine has everything)")
			for _, e := range h.RecentErrors {
				fmt.Fprintf(w, "    %s\n", cleanLine(e))
			}
		}
	}
}

// accountLine is one harness's accounts as one cell: each label with its state
// and, where the harness said, the window nearest its limit. A harness with no
// accounts runs on the harness's own login, which is a state and not a gap.
func accountLine(accounts []v1.AccountReport) string {
	if len(accounts) == 0 {
		return "no accounts configured; runs on the harness's own login"
	}
	parts := make([]string, 0, len(accounts))
	for _, a := range accounts {
		part := cleanLine(a.Label) + " " + accountStateWord(a.State)
		if a.LimitedUntil != nil {
			part += " until " + a.LimitedUntil.Local().Format(time.DateTime)
		}
		if w, ok := busiestWindow(a.Windows); ok {
			part += fmt.Sprintf(" (%s %.0f%%)", cleanLine(w.Name), w.UsedPercent)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// accountStateWord is how one account's state reads in the listing. Each state
// of the closed set is written out here rather than printed from the value, so
// for a state this binary knows, no byte of the answer reaches the terminal at
// all. The hub validates the enum on the way in, and that is not what protects
// the operator: `yad hub runners --hub <url>` will talk to any hub, and
// hubapiclient decodes the answer with encoding/json, which enforces nothing.
// A state from outside the set is news — the hub is newer than this binary —
// so it is shown, cleaned.
//
// Absent is its own answer and not a blank: state is omitempty because a
// runner from before the field cannot say (ARCHITECTURE.md §2), and an empty
// cell beside a label would read as free.
func accountStateWord(s v1.AccountState) string {
	switch s {
	case v1.AccountFree:
		return "free"
	case v1.AccountLimited:
		return "limited"
	case v1.AccountNeedsLogin:
		return "needs_login"
	case "":
		return "state unknown"
	}
	return "state " + cleanLine(string(s))
}

// busiestWindow is the account's fullest usage window: the one that will stop
// it first, which is the only one worth a line of a summary.
func busiestWindow(windows []v1.AccountWindow) (v1.AccountWindow, bool) {
	var out v1.AccountWindow
	found := false
	for _, w := range windows {
		if !found || w.UsedPercent > out.UsedPercent {
			out, found = w, true
		}
	}
	return out, found
}
