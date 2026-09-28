package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/supervise"
)

// Claude's model list, asked for rather than assumed (DEV-50). The
// list_models control request is answered from Claude's own model options —
// the login's plan, the settings cascade, the provider — and no user message
// is sent, so no turn runs and no token is spent. Measured on 2.1.283 against
// a local server standing in for the API: the one request Claude made while
// answering was a HEAD of /api/hello. Which models it lists does differ by
// login: a subscription and an API provider were offered different lists.

// modelsArgs is a Claude that reads stream-json and is never given a user
// message, so it answers control requests and never takes a turn.
var modelsArgs = []string{
	"-p",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
	// Nothing is said, so there is nothing to keep, and a transcript per
	// capability probe would fill the account's history.
	"--no-session-persistence",
	// The owner's hooks are for runs: SessionStart and SessionEnd would
	// otherwise fire on every probe. Settings stay the owner's in every other
	// respect, since they decide which models Claude offers.
	"--settings", `{"disableAllHooks":true}`,
}

// modelsRequestID names the one control request a probe sends.
const modelsRequestID = requestPrefix + "list-models"

// modelsExitGrace is how long Claude gets to exit once its input closes after
// answering. With no session to save it exits at once; past this it is
// stopped.
var modelsExitGrace = 3 * time.Second

// beforeModelsAsked, set only by tests, runs between Claude's start and the
// list_models request, so a test can have Claude exit first — the order the
// OS picks only sometimes.
var beforeModelsAsked func(*supervise.Process)

// maxModels bounds what one login reports. Claude lists about a dozen; the
// bound keeps a strange answer from growing every capability document.
const maxModels = 64

// modelName is what a model Claude lists looks like: opus, claude-opus-4-8,
// sonnet[1m]. The charset is the guarantee that nothing else Claude says — a
// description, a path, a URL — reaches the capability document every
// connected hub reads (DEV-67).
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}(\[[A-Za-z0-9]{1,16}\])?$`)

// ListModels asks the claude at bin for the models it offers the login env
// points it at, in Claude's own order — its picker's — without the ones it
// lists but will not run on this login. ctx bounds the whole of it.
//
// env is the one a run on that login gets (account.Env), so the answer is
// about the credential the run would use (DEV-62: the rule supervise.Spec states for what a run keeps).
// The error is yad's own words and never Claude's.
func ListModels(ctx context.Context, bin, dir string, env []string) ([]string, error) {
	return listModels(ctx, bin, dir, env, nil)
}

func listModels(ctx context.Context, bin, dir string, env []string, raw io.Writer) ([]string, error) {
	p, err := supervise.Start(ctx, supervise.Spec{Path: bin, Args: modelsArgs, Dir: dir, Env: runEnv(env), Stdin: true, NoTTY: true})
	if err != nil {
		return nil, fmt.Errorf("claude would not start to list its models: %w", err)
	}
	answer := make(chan listAnswer, 1)
	read := make(chan struct{})
	go func() {
		defer close(read)
		answer <- readModels(p.Stdout(), raw)
		// Read on to the end, so Claude is never held writing into a full
		// pipe when it should be exiting.
		io.Copy(io.Discard, p.Stdout())
	}()
	defer func() {
		p.Stdin().Close()
		select {
		case <-p.Done():
		case <-time.After(modelsExitGrace):
			p.Stop(supervise.Ladder{TermGrace: termGrace})
		}
		select {
		case <-read:
		case <-time.After(drainGrace):
		}
		p.Stdout().Close()
	}()

	req, _ := json.Marshal(map[string]any{
		"type": "control_request", "request_id": modelsRequestID,
		"request": map[string]string{"subtype": "list_models"},
	})
	if beforeModelsAsked != nil {
		beforeModelsAsked(p)
	}
	// A write that fails is a Claude already gone, and one gone a moment
	// later is found by the reader instead; which of the two comes first is
	// the OS's choice. The reader decides both, from the end of Claude's
	// output, so a Claude that died says the same thing either way (DEV-149)
	// — and one that closed its input yet lives on is given up on with ctx.
	_, _ = p.Stdin().Write(append(req, '\n'))
	select {
	case a := <-answer:
		return a.models, a.err
	case <-ctx.Done():
		return nil, fmt.Errorf("claude did not list its models in time: %w", ctx.Err())
	}
}

type listAnswer struct {
	models []string
	err    error
}

// readModels reads Claude's output to the answer to the request, skipping
// everything else it writes first.
func readModels(r io.Reader, raw io.Writer) listAnswer {
	lr := adapter.NewLineReader(r)
	for {
		line, err := lr.Next()
		if _, tooLong := errors.AsType[*adapter.ErrLineTooLong](err); tooLong {
			continue
		}
		if err != nil {
			return listAnswer{err: errors.New("claude ended its output without listing its models")}
		}
		if raw != nil {
			raw.Write(bytes.Clone(line))
			raw.Write(newline)
		}
		var f struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Response  struct {
					Models []struct {
						Value    string `json:"value"`
						Disabled bool   `json:"disabled"`
					} `json:"models"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &f) != nil || f.Type != "control_response" || f.Response.RequestID != modelsRequestID {
			continue
		}
		if f.Response.Subtype != "success" {
			// A Claude from before list_models answers with an error. What it
			// says is not kept.
			return listAnswer{err: errors.New("claude refused list_models — a Claude older than the request does")}
		}
		seen := map[string]bool{}
		var out []string
		for _, m := range f.Response.Response.Models {
			// Disabled is a model Claude shows and will not run on this login.
			if m.Disabled || seen[m.Value] || !modelName.MatchString(m.Value) || len(out) == maxModels {
				continue
			}
			seen[m.Value] = true
			out = append(out, m.Value)
		}
		if len(out) == 0 {
			return listAnswer{err: errors.New("claude listed no models")}
		}
		return listAnswer{models: out}
	}
}
