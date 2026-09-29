package opencode

import (
	"bufio"
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/supervise"
)

// PinnedVersion is the one OpenCode release the adapter was recorded
// against (testdata/opencode-<version>). OpenCode has no way to print the ACP
// surface it speaks, so the pin is the release: another one is driven, with
// a warning, as an unpinned Codex is (decisions 0037, 0067, 0072).
const PinnedVersion = "1.18.33"

// VersionWarning is the capability document's warning for an OpenCode that
// is not the pinned release, or "" for the pinned one. It never quotes what
// OpenCode printed beyond the version detection already reports (DEV-67).
func VersionWarning(version string) string {
	if version == PinnedVersion || version == "" {
		return ""
	}
	return "this OpenCode is " + version + ", and yad was recorded against OpenCode " + PinnedVersion +
		" — its ACP answers may differ, and a run that uses something it changed fails with its own words; install " + PinnedVersion + " to be sure, or upgrade yad"
}

// modelsArgs lists the models OpenCode offers the login it runs on, without
// a session — so without a token, and without the session/new that ACP's
// own config options need, which OpenCode would keep (decision 0072).
var modelsArgs = []string{"models"}

// modelName is the shape of what `opencode models` prints: provider/model.
// Anything else on its output is not a model and is not reported, since the
// capability document reaches every hub (DEV-67).
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)

// modelsOutputCap bounds what the list may print: a few hundred names for a
// machine with every provider logged in.
const modelsOutputCap = 1 << 20

// modelsExitGrace is how long `opencode models` gets to exit once it has
// printed; it exits at once.
var modelsExitGrace = 2 * time.Second

// ListModels asks OpenCode which models its login offers: `opencode models`,
// in the environment env points it at.
func ListModels(ctx context.Context, bin, dir string, env []string) ([]string, error) {
	p, err := supervise.Start(ctx, supervise.Spec{Path: bin, Args: modelsArgs, Dir: dir, Env: env})
	if err != nil {
		return nil, adapter.ModelsError(adapter.ErrModelsNoStart, "opencode would not start to list its models", err)
	}
	read := make(chan []string, 1)
	go func() {
		var names []string
		sc := bufio.NewScanner(io.LimitReader(p.Stdout(), modelsOutputCap))
		for sc.Scan() {
			if s := strings.TrimSpace(sc.Text()); modelName.MatchString(s) {
				names = append(names, s)
			}
		}
		read <- names
		io.Copy(io.Discard, p.Stdout())
	}()
	var names []string
	select {
	case names = <-read:
	case <-ctx.Done():
	}
	select {
	case <-p.Done():
	case <-time.After(modelsExitGrace):
		p.Stop(supervise.Ladder{TermGrace: termGrace})
	}
	p.Stdout().Close()
	werr := p.Wait()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case werr != nil:
		return nil, adapter.ModelsError(adapter.ErrModelsRefused, "opencode models failed", werr)
	case len(names) == 0:
		return nil, adapter.ModelsError(adapter.ErrModelsUnread, "opencode models named no model", errors.New("no provider/model line"))
	}
	return names, nil
}

// termGrace is how long OpenCode gets between SIGTERM and SIGKILL.
const termGrace = 5 * time.Second
