package capability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/claude"
	"github.com/skkap/yad/internal/adapter/codex"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/probe"
)

// A harness's models are asked of the harness, for each login a run may use
// (DEV-50): Claude's list_models control request and Codex's model/list,
// neither of which runs a turn or spends a token. Asking starts the harness,
// so an answer is kept, a slow harness is not waited on past a bound, and one
// that cannot answer is reported from the catalog and said to be.

// lister asks one harness for the models the login env points it at.
type lister func(ctx context.Context, bin, dir string, env []string) ([]string, error)

var listers = map[string]lister{"claude": claude.ListModels, "codex": codex.ListModels}

// ListModelsForTests, when set, answers in place of every harness, so a test
// whose fake harness plays a run is not also started to list models.
var ListModelsForTests func(ctx context.Context, harnessID, bin, dir string, env []string) ([]string, error)

var (
	// modelsRecheck is how long a login's list is kept. It moves with a
	// harness release, which is part of what an answer is kept by, so an
	// upgrade asks again at once; otherwise with a plan, which is rare. The
	// daemon rebuilds the document every 15 seconds, and each ask starts a
	// harness — Claude takes seconds and a few hundred megabytes to answer.
	modelsRecheck = time.Hour
	// modelsRetry is how soon a login whose harness did not answer is asked
	// again. Its last answer, if it ever gave one, is reported meanwhile.
	modelsRetry = 5 * time.Minute
	// modelsTimeout bounds one ask. Claude 2.1.283 answered in about three
	// seconds, most of it its own start, and Codex 0.157.1 in a tenth of one;
	// past this the document goes out without that login's list rather than
	// hold a registration or a sync's document for a harness that hangs.
	modelsTimeout = 15 * time.Second
	modelsNow     = time.Now
)

type modelsAnswer struct {
	models []string
	until  time.Time
	// failed is why the last ask got no answer, in yad's words, and since
	// is when it first said so; "" is an ask that was answered. Kept with
	// the answer it leaves standing, so a document built from a kept answer
	// still says why that answer is old.
	failed string
	since  time.Time
}

// ModelsFailure is one login whose harness did not answer the last time it
// was asked for its models, as of the last document built. It is for the
// owner — the daemon's log and `yad doctor` — and never the document: a hub
// is told where the models came from (models_source), and why an ask failed
// on the machine is the machine's to fix.
type ModelsFailure struct {
	Harness string
	// Account is the account's label, or "" for the harness's own login.
	Account string
	// Reason is yad's words with the next action; never what the harness
	// printed, which may carry a path or a credential (DEV-60, DEV-67).
	Reason string
	Since  time.Time
}

var (
	modelsMu    sync.Mutex
	modelsAsked = map[string]modelsAnswer{}
	// modelsForgotten counts each harness's ForgetModels. An ask takes
	// seconds and a forget can land while it runs — a hub login finishing
	// mid-build — so an answer is kept only if no forget came after it
	// began: otherwise the old login's list would be written back and kept
	// for the hour the forget was meant to cut short.
	modelsForgotten = map[string]int{}
	// modelsFailing is the last document's failures, which ModelsFailures
	// hands out.
	modelsFailing []ModelsFailure
)

// ModelsFailures is every login the last document built in this process could
// not ask for its models, by harness and account. The daemon builds the
// document every interval, so for it this is now.
func ModelsFailures() []ModelsFailure {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	return slices.Clone(modelsFailing)
}

// ForgetModels drops the lists kept for a harness, so the next document asks
// again: a login has just changed, and with it, perhaps, the plan.
func ForgetModels(id string) {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	modelsForgotten[id]++
	for key := range modelsAsked {
		if strings.HasPrefix(key, id+"\x00") {
			delete(modelsAsked, key)
		}
	}
}

// login is one login a run may use: an account's home, or "" for the
// harness's own default one.
type login struct{ label, home string }

// logins is every login a run of this harness may use: each account's that
// can take a run, or with none configured the default home a run inherits.
// An owner who configured accounts whose states could not be read gets none:
// the default home would describe a login no run uses.
func logins(id string, cfg config.Config, accounts []account.Account) []login {
	mine := account.For(accounts, id)
	if len(mine) == 0 {
		if len(cfg.Harness[id].Accounts) > 0 {
			return nil
		}
		return []login{{}}
	}
	var out []login
	for _, a := range mine {
		// A login that is not there has nothing to list, and what the harness
		// says with none describes no account.
		if a.State != v1.AccountNeedsLogin {
			out = append(out, login{a.Label, a.Home})
		}
	}
	return out
}

// addModels fills in each harness's models from the harness: every login's
// list, asked side by side, merged in the harness's order, and each account's
// own. A harness nothing could be asked of keeps the catalog's list and says
// so; Codex's own cache of its list answers for a login it could not be asked
// about, since that is still the harness's word for that login.
func addModels(ctx context.Context, found []harness.Detected, cfg config.Config, accounts []account.Account) {
	type ask struct {
		i  int
		l  login
		ms []string
		// failed and since are the ask's modelsAnswer's.
		failed string
		since  time.Time
	}
	var asks []*ask
	for i, d := range found {
		if _, ok := listers[d.ID]; !ok || !d.Present {
			continue
		}
		for _, l := range logins(d.ID, cfg, accounts) {
			asks = append(asks, &ask{i: i, l: l})
		}
	}
	var wg sync.WaitGroup
	for _, a := range asks {
		if d := found[a.i]; d.Ready() {
			wg.Go(func() {
				got := modelsOf(ctx, d, a.l.home)
				a.ms, a.failed, a.since = got.models, got.failed, got.since
			})
		}
	}
	wg.Wait()
	var failing []ModelsFailure
	for _, a := range asks {
		if a.failed != "" {
			failing = append(failing, ModelsFailure{Harness: found[a.i].ID, Account: a.l.label, Reason: a.failed, Since: a.since})
		}
	}
	modelsMu.Lock()
	modelsFailing = failing
	modelsMu.Unlock()

	for i := range found {
		d := &found[i]
		var merged []string
		for _, a := range asks {
			if a.i != i {
				continue
			}
			if a.ms == nil && d.ID == "codex" {
				home := a.l.home
				if home == "" {
					home = codex.DefaultHome()
				}
				a.ms = codex.Models(home)
			}
			if a.ms == nil {
				continue
			}
			for _, m := range a.ms {
				if !slices.Contains(merged, m) {
					merged = append(merged, m)
				}
			}
			if a.l.label != "" {
				if d.AccountModels == nil {
					d.AccountModels = map[string][]string{}
				}
				d.AccountModels[a.l.label] = a.ms
			}
		}
		switch {
		case len(merged) > 0:
			d.Models, d.ModelsSource = merged, v1.ModelsFromHarness
		case len(d.Models) > 0:
			d.ModelsSource = v1.ModelsFromCatalog
		}
	}
}

// modelsOf is one login's list: kept from the last ask while it is fresh,
// asked again once it is not. Its models are nil for a harness that has never
// answered for this login, and its failed says why the last ask was not.
func modelsOf(ctx context.Context, d harness.Detected, home string) modelsAnswer {
	// The environment of a run on this login, and only that: the answer is
	// about the credential the run would use (DEV-62, and the rule
	// supervise.Spec states for what a run keeps).
	env := account.Env(d.ID, home)
	key := modelsKey(d, env)
	now := modelsNow()
	modelsMu.Lock()
	kept, ok := modelsAsked[key]
	gen := modelsForgotten[d.ID]
	modelsMu.Unlock()
	if ok && now.Before(kept.until) {
		return kept
	}
	askCtx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	// From the user's home, as the login check runs: a run's workdir is not
	// known here, and the home's is the user's own settings.
	dir, err := os.UserHomeDir()
	if err != nil {
		dir = os.TempDir()
	}
	var got []string
	if ListModelsForTests != nil {
		got, err = ListModelsForTests(askCtx, d.ID, d.Path, dir, env)
	} else {
		got, err = listers[d.ID](askCtx, d.Path, dir, env)
	}
	if ctx.Err() != nil {
		// The caller stopped asking; that says nothing about the harness.
		return kept
	}
	next := modelsAnswer{models: got, until: now.Add(modelsRecheck)}
	if err != nil {
		next = modelsAnswer{models: kept.models, until: now.Add(modelsRetry), failed: modelsReason(d, err, askCtx.Err() != nil), since: now}
		if kept.failed == next.failed {
			next.since = kept.since
		}
	}
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if modelsForgotten[d.ID] != gen {
		// Forgotten while it was asked: this answer may be the old login's,
		// so it is reported this once and not kept, and so is its failure.
		if err != nil {
			return modelsAnswer{failed: next.failed, since: now}
		}
		return modelsAnswer{models: got}
	}
	modelsAsked[key] = next
	return next
}

// modelsRequest is what each harness is asked, as its owner would look it up.
var modelsRequest = map[string]string{"claude": "list_models", "codex": "model/list"}

// modelsReason is why an ask got no answer, with what to do about it, built
// from what kind of failure err is and never from its text: the owner reads
// it in the daemon's log and in `yad doctor`, and what a harness printed is
// not for either (DEV-146). timedOut is the ask cut off at modelsTimeout,
// whatever the harness was doing when it was.
func modelsReason(d harness.Detected, err error, timedOut bool) string {
	// The binary as detection found it, so each command below runs the file
	// the runner does: by name on PATH, or through its override.
	f := probe.Find(d.EnvPath, d.Binary, d.VersionArgs)
	req := modelsRequest[d.ID]
	switch {
	case timedOut:
		return fmt.Sprintf("%s did not answer %s within %s — %s", d.Binary, req, modelsTimeout, f.Try(f.VersionCommand(), "whether it starts promptly"))
	case errors.Is(err, adapter.ErrModelsNoStart):
		return fmt.Sprintf("%s could not be started to ask it: %s", d.Binary, f.WontStart())
	case errors.Is(err, adapter.ErrModelsRefused) && d.ID == "claude":
		// The oldest Claude seen to answer it; when the request arrived is
		// not recorded anywhere yad can read.
		return fmt.Sprintf("%s refused %s, as a Claude Code older than the request does (2.1.283 answers it) — %s", d.Binary, req, f.Do(f.Command("update"), "upgrade it"))
	case errors.Is(err, adapter.ErrModelsRefused):
		// Every Codex the adapter is pinned to answers it (decision 0037),
		// so this one is older than those, and how it was installed — npm,
		// Homebrew, a release binary — is how it is upgraded.
		return fmt.Sprintf("%s refused %s, which every %s this yad was built against answers — upgrade %s the way it was installed", d.Binary, req, d.Label, d.Label)
	case errors.Is(err, adapter.ErrModelsUnread):
		return fmt.Sprintf("%s answered %s with no model this yad can read — a %s newer than this yad may answer in a shape it does not know, and upgrading yad is the fix", d.Binary, req, d.Label)
	}
	return fmt.Sprintf("%s stopped before it answered %s, and what it printed is not kept — %s", d.Binary, req, f.Try(f.VersionCommand(), "whether it starts"))
}

// modelsKey is what a list is kept by: the harness, the binary and its
// version, and the environment it was asked in — which names the account's
// home and, for a token account, its token. Hashed, so no token is held here
// as a key.
func modelsKey(d harness.Detected, env []string) string {
	sum := sha256.Sum256([]byte(strings.Join(env, "\x00")))
	return d.ID + "\x00" + d.Path + "\x00" + d.Version + "\x00" + hex.EncodeToString(sum[:])
}
