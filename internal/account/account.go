// Package account holds a runner's accounts per harness: each account's harness
// home, the transcript directory they share, the state a hub sees, and which
// account a run takes (ARCHITECTURE.md §3, decisions 0013 and 0039).
//
// The one rule the package exists to keep: YAD stores no account tokens of its
// own. The harness logs itself in, inside its own home, and nothing here reads,
// copies, moves or prints what that login writes. A label is not a secret and a
// credential is — the label travels in events, in health and in
// `yad account list`; nothing else from a home travels anywhere.
package account

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
	v1 "github.com/skkap/yad/protocol/v1"
)

// Account is one harness login on this runner.
type Account struct {
	Harness string
	Label   string
	// Home is what the harness is given as CLAUDE_CONFIG_DIR or CODEX_HOME.
	Home         string
	State        v1.AccountState
	LimitedUntil *time.Time
	UpdatedAt    time.Time
}

// Report is the account as a hub sees it. Home is deliberately absent: a path
// inside the owner's machine is not a hub's business, and everything under it
// is the credential.
func (a Account) Report() v1.AccountReport {
	// state is a required field of a public type, so an Account built without
	// one needs a value rather than an empty string no hub's enum allows.
	//
	// It resolves to free, which fails open: the account gets tried. The
	// opposite — treating an unknown state as unusable "to be safe" — would
	// stop a runner claiming for a reason nothing ever established, which is
	// the same mistake as letting a login check that could not answer park an
	// account. Ambiguity never parks an account; only an answer does.
	state := a.State
	if state == "" {
		state = v1.AccountFree
	}
	return v1.AccountReport{Label: a.Label, State: state, LimitedUntil: a.LimitedUntil}
}

// homeVar is the environment variable each harness reads its home from.
// Verified per-home on macOS (DEV-24) and on Linux (DEV-26): on Linux Claude's
// credential is a plain file inside the home and no keyring is consulted, so
// the isolation is a property of the path rather than of the platform.
var homeVar = map[string]string{
	"claude": "CLAUDE_CONFIG_DIR",
	"codex":  "CODEX_HOME",
}

// transcriptDir is the directory inside a harness home that holds its
// transcripts, and which YAD replaces with a link to the one shared directory
// so any account can resume any session (decision 0013, measured in DEV-24).
var transcriptDir = map[string]string{
	"claude": "projects",
	"codex":  "sessions",
}

// Supported says whether YAD knows where this harness keeps its home and its
// transcripts. A harness it does not is not an error: its runs use the
// harness's own default home, as they did before accounts existed.
func Supported(harness string) bool {
	_, ok := homeVar[harness]
	return ok
}

// HomeDir is where one account's harness home lives.
func HomeDir(data, harness, label string) string {
	return filepath.Join(data, "accounts", harness, label)
}

// TranscriptDir is where one harness's transcripts live: once, for every
// account, linked into each home.
func TranscriptDir(data, harness string) string {
	return filepath.Join(data, "transcripts", harness)
}

// Env is the single variable that points a harness at an account's home, ready
// to append to a child's environment. Empty for a harness with no home of its
// own, whose runs use the harness's default.
func Env(harness, home string) []string {
	v, ok := homeVar[harness]
	if !ok || home == "" {
		return nil
	}
	return []string{v + "=" + home}
}

// Ensure creates an account's harness home and links the shared transcript
// directory into it, and may be called again on a home that already exists.
//
// Both directories are 0700: a harness home holds the credential the harness
// wrote there, and the transcripts hold whole conversations.
func Ensure(data, harness, label string) (string, error) {
	if err := checkNames(harness, label); err != nil {
		return "", err
	}
	home := HomeDir(data, harness, label)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return "", err
	}
	name, ok := transcriptDir[harness]
	if !ok {
		return home, nil
	}
	shared := TranscriptDir(data, harness)
	if err := os.MkdirAll(shared, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(shared, 0o700); err != nil {
		return "", err
	}
	return home, link(filepath.Join(home, name), shared)
}

// link points one home's transcript directory at the shared one.
//
// A real directory already sitting there is never deleted: it is a harness's
// own transcripts, written before this home was linked, and removing it would
// throw away conversations YAD cannot rebuild. The owner is told to move it.
//
// Several runs of one harness prepare the same home at once — capacity is a
// shared pool and nothing reserves an account — so every step below can lose a
// race to another run doing exactly the same thing. Rather than enumerate the
// error each loser sees, which differs by platform and by step (macOS reports
// EPERM, not ENOENT, for unlink on a directory another run has already
// replaced), the work is retried and the goal state is what decides: if the
// link is already what it should be, whoever made it did this function's job.
func link(from, to string) error {
	var err error
	for range linkAttempts {
		if at, rerr := os.Readlink(from); rerr == nil && at == to {
			return nil
		}
		var keep bool
		if keep, err = linkOnce(from, to); err == nil || keep {
			return err
		}
	}
	return err
}

// linkAttempts bounds the retry. Each losing step is one other run getting
// there first, and a home is prepared by at most the runner's capacity at
// once, so a handful is plenty; the bound is here so a genuine failure ends
// as an error rather than a spin.
const linkAttempts = 5

// linkOnce makes one attempt. keep is true for a failure retrying cannot
// change — a directory of real transcripts in the way — so the caller stops
// and reports it rather than trying again.
func linkOnce(from, to string) (keep bool, err error) {
	switch fi, err := os.Lstat(from); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return false, err
	case fi.Mode()&os.ModeSymlink != 0:
		if err := os.Remove(from); err != nil {
			return false, err
		}
	default:
		entries, err := os.ReadDir(from)
		if errors.Is(err, os.ErrNotExist) {
			break // another run removed it; that is where this was heading
		}
		if err != nil {
			return false, err
		}
		if len(entries) > 0 {
			return true, fmt.Errorf("%s is a directory of transcripts, not a link to %s — move it aside (its sessions can be copied into %s) and run this again", from, to, to)
		}
		if err := os.Remove(from); err != nil {
			return false, err
		}
	}
	return false, os.Symlink(to, from)
}

// Remove deletes an account's harness home, and nothing else.
//
// The shared transcripts are reached through a symlink, and RemoveAll deletes
// the link rather than following it, so every other account keeps every
// session. Codex leaves sqlite -shm and -wal sidecars beside its databases
// (~1.8 MB per home before any work, DEV-24); RemoveAll takes them with the
// rest, which is why the home is a directory to delete and not a file to
// unlink.
func Remove(data, harness, label string) error {
	if err := checkNames(harness, label); err != nil {
		return err
	}
	return os.RemoveAll(HomeDir(data, harness, label))
}

// checkNames guards the two strings that become path elements under
// <data>/accounts/. The harness id is checked as well as the label because
// filepath.Join cleans "..", so an unchecked id resolves outside the data
// directory — and the caller on the other end of that is os.RemoveAll. Only
// the owner's own argv reaches here today; a package that builds paths
// defends them regardless of who calls it.
func checkNames(harness, label string) error {
	if err := config.ValidName(harness); err != nil {
		return fmt.Errorf("harness: %w", err)
	}
	if err := config.ValidName(label); err != nil {
		return fmt.Errorf("account label: %w", err)
	}
	return nil
}

// Load is every account the owner configured, per harness in the owner's
// order, with its state. A nil q is a runner with no state database yet,
// which has no states to hold.
//
// The rule, stated once here because several places used to state it
// differently: an account is free only when its home is on disk and nothing
// says otherwise. A home that is not there is needs_login whatever the store
// holds — a directory that does not exist cannot hold a login — so a label
// added to config.toml by hand is not usable until `yad account add` has made
// its home and run the login in it.
func Load(ctx context.Context, q *db.Queries, data string, cfg config.Config) ([]Account, error) {
	var rows []db.Account
	if q != nil {
		var err error
		if rows, err = q.ListAllAccounts(ctx); err != nil {
			return nil, err
		}
	}
	type key struct{ harness, label string }
	held := make(map[key]db.Account, len(rows))
	for _, r := range rows {
		held[key{r.Harness, r.Label}] = r
	}
	var out []Account
	for _, id := range cfg.HarnessIDs() {
		for _, label := range cfg.Harness[id].Accounts {
			a := Account{
				Harness: id, Label: label,
				Home:  HomeDir(data, id, label),
				State: v1.AccountFree,
			}
			if r, ok := held[key{id, label}]; ok {
				a.State = v1.AccountState(r.State)
				if r.LimitedUntil.Valid {
					t := time.UnixMilli(r.LimitedUntil.Int64).UTC()
					a.LimitedUntil = &t
				}
				if r.UpdatedAt > 0 {
					a.UpdatedAt = time.UnixMilli(r.UpdatedAt).UTC()
				}
			}
			// A home that is not on disk cannot hold a login, so the account
			// needs one whatever the store last recorded. This is what a label
			// left in config.toml after `yad account remove` looks like to a
			// daemon still holding the config it started with: without it the
			// account reads free, a run rebuilds the empty home the owner just
			// deleted, and the turn fails against a logged-out harness.
			if a.State == v1.AccountFree {
				if _, err := os.Stat(a.Home); errors.Is(err, os.ErrNotExist) {
					a.State = v1.AccountNeedsLogin
				}
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// For is one harness's accounts, in the owner's order.
func For(accounts []Account, harness string) []Account {
	var out []Account
	for _, a := range accounts {
		if a.Harness == harness {
			out = append(out, a)
		}
	}
	return out
}

// First is the account a run takes: the first in the owner's order that is
// free. A limited account and one that needs login are skipped alike — neither
// can run a turn, and neither is an error.
//
// The owner's order is the whole of the choice here. Preferring the free
// account whose window resets soonest (decision 0039), and moving a run off an
// account that becomes limited mid-turn, are DEV-28's.
func First(accounts []Account, harness string) (Account, bool) {
	for _, a := range For(accounts, harness) {
		if a.State == v1.AccountFree {
			return a, true
		}
	}
	return Account{}, false
}

// Reports is one harness's accounts as a hub sees them, in the owner's order.
// Nil for a harness the owner gave no accounts: no accounts is a reportable
// state, not a failure, and such a harness runs on its own default home.
func Reports(accounts []Account, harness string) []v1.AccountReport {
	var out []v1.AccountReport
	for _, a := range For(accounts, harness) {
		out = append(out, a.Report())
	}
	return out
}

// SetState records an account's state. Only free and needs_login are written
// here; limited and its reset time are DEV-27's, through SetAccountLimit.
func SetState(ctx context.Context, q *db.Queries, harness, label string, state v1.AccountState, now time.Time) error {
	return q.SetAccountState(ctx, db.SetAccountStateParams{
		Harness: harness, Label: label, State: string(state), UpdatedAt: now.UnixMilli(),
	})
}

// Read is Load against the state database on disk, opened read-only and closed
// again: for the capability document and the CLI, which need account states
// without owning the runner's store.
//
// A profile with no state database yet has no states, and every account the
// owner configured is free — the same answer a runner that has never limited
// anything would give.
func Read(ctx context.Context, paths config.Paths, cfg config.Config) ([]Account, error) {
	st, err := store.OpenReadOnly(ctx, paths.StateDB())
	if errors.Is(err, store.ErrNoState) {
		return Load(ctx, nil, paths.Data, cfg)
	}
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return Load(ctx, st.Queries, paths.Data, cfg)
}
