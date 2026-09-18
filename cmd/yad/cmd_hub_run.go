package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
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

// parseInterleaved parses flags on both sides of the positional arguments,
// since flag stops at the first one and the usage puts them first.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
}

const submitUsage = "usage: yad hub submit --harness h --model m [--context text | --context-file f] [--session id | --new-session id] [--run-id id] [--watch] <instruction | ->"

// cmdHubSubmit queues a run and prints its id — alone on stdout, so a script
// can capture it — or, with --watch, follows it to its end.
func cmdHubSubmit(ctx context.Context, g global, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hub submit", flag.ContinueOnError)
	hf := addHubFlags(fs, g)
	harness := fs.String("harness", "", "the harness to run, e.g. claude (required)")
	model := fs.String("model", "", "the model to point it at, e.g. opus (required)")
	contextText := fs.String("context", "", "context appended to the harness's system prompt")
	contextFile := fs.String("context-file", "", "read the context from this file")
	session := fs.String("session", "", "continue this session, which the hub already has")
	newSession := fs.String("new-session", "", "start a session with this id (default: a new one with a generated id)")
	runID := fs.String("run-id", "", "the run's id, to make a retried submit safe (default: generated)")
	watch := fs.Bool("watch", false, "follow the run until it ends, as `yad hub watch` does")
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
	w        io.Writer
	shown    hubapi.RunState
	lastText string
}

func (p *printer) state(r hubapi.Run) {
	if r.State == p.shown {
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
