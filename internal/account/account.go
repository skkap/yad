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
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
	// Windows is every usage window this harness has told the runner about
	// for this account, in name order. Kept beside the state rather than
	// inside it: a window at 96% says nothing about whether the account can
	// run a turn, and one at 100% outlives the limit it caused.
	Windows   []v1.AccountWindow
	UpdatedAt time.Time
}

// stateOf is the one rule for reading a stored account row, in one place so
// that Load, Report and anything reading either cannot disagree.
//
// LimitedUntil is the authority and the state is derived from it: a usage
// limit is a fact about time, and expecting a writer to come along and flip
// the row back to free would mean an account stayed parked for as long as
// nothing ran on it. Nothing writes the expiry back; the moment it passes,
// every reader sees free.
//
// Only limited is derived this way. free and needs_login say nothing about
// time and are returned as stored.
func stateOf(state v1.AccountState, until *time.Time, now time.Time) v1.AccountState {
	if state != v1.AccountLimited {
		return state
	}
	// A limited row with no reset at all is left limited. SetLimit dates every
	// limit it writes, so this is a row from another version or a hand-edited
	// database; reading it as free would send runs straight back at an account
	// that cannot take them, and the owner's `yad account add` is the way out,
	// as it is for needs_login.
	if until != nil && !now.Before(*until) {
		return v1.AccountFree
	}
	return v1.AccountLimited
}

// RefillAt is the soonest a window the harness called full will refill, across
// the window sets given, and the zero time when none of them is full or none
// carries a reset.
//
// It is how an undated usage limit gets a real date instead of a guessed one.
// A window at 100% is the harness's own statement that this account is out
// until that time, and both harnesses report their windows constantly - Codex
// with a snapshot on every turn, Claude in every rate_limit_event - so a limit
// that arrived without a reset is usually explained by a window that did.
//
// Soonest rather than latest: the account is offered again at the first
// moment it could work. Being early costs one turn that finds the limit still
// there and re-dates it; being late idles a subscription the owner pays for.
func RefillAt(sets ...[]v1.AccountWindow) time.Time {
	var soonest time.Time
	for _, ws := range sets {
		for _, w := range ws {
			if w.UsedPercent < 100 || w.ResetsAt == nil || w.ResetsAt.IsZero() {
				continue
			}
			if soonest.IsZero() || w.ResetsAt.Before(soonest) {
				soonest = *w.ResetsAt
			}
		}
	}
	return soonest
}

// limitWithoutReset dates a usage limit that neither the harness nor any
// window it ever reported could date - RefillAt found nothing, so this is a
// harness that said "out of quota" and no more, on an account no run has yet
// heard a window from. A bare 429 in a Claude result on a fresh account is the
// case that reaches it.
//
// The number is short on purpose, and the instinct to lengthen it is wrong.
// The two costs are not symmetric:
//
//   - Too long idles an account that may be perfectly good, for the whole
//     guess. Nothing shortens it; the owner pays for the subscription either
//     way.
//   - Too short costs one turn. The account is offered, the harness says it is
//     still out, and the limit is re-dated. Claude's own retry ladder takes
//     about three minutes to reach that answer.
//
// Thirty minutes puts the worst case of being wrong at roughly three minutes
// of a capacity slot per half hour - about a tenth of one slot, bounded - and
// caps the idle loss at half an hour. Five hours was considered, on the
// grounds that it is the shortest window either harness has (DOMAIN.md); it
// was rejected because that reasoning dates the limit by how long limits
// usually last rather than by anything this account said, and pays hours for
// the privilege.
const limitWithoutReset = 30 * time.Minute

// Report is the account as a hub sees it. Home is deliberately absent: a path
// inside the owner's machine is not a hub's business, and everything under it
// is the credential.
func (a Account) Report() v1.AccountReport {
	// state is optional in the schema but constrained by an enum, so an
	// Account built without one needs a value rather than an empty string the
	// enum does not allow. Optional is deliberate — see protocol/v1 — and this
	// default is what lets it be: every producer sets a real value, so nothing
	// on the wire depends on the field being required.
	//
	// It resolves to free, which fails open: the account gets tried. The
	// opposite — treating an unknown state as unusable "to be safe" — would
	// stop a runner claiming for a reason nothing ever established, which is
	// the same mistake as letting a login check that could not answer park an
	// account. Ambiguity never parks an account; only an answer does.
	state := stateOf(a.State, a.LimitedUntil, time.Now())
	if state == "" {
		state = v1.AccountFree
	}
	r := v1.AccountReport{Label: a.Label, State: state, Windows: a.Windows}
	// A reset in the past beside state: free is the disagreement a hub would
	// act on, and DEV-28 decides whether to claim from exactly this pair. The
	// window that caused the limit keeps its reset in Windows, so nothing a
	// hub needs is lost by dropping it here.
	if state == v1.AccountLimited {
		r.LimitedUntil = a.LimitedUntil
	}
	return r
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

// errRacedWhileLinking says another run changed the path mid-step. link
// normally absorbs it — it retries, and the goal-state check at the top of the
// next attempt ends it — but it does reach a caller when every attempt loses,
// which is why it reads as a sentence rather than as a marker.
var errRacedWhileLinking = errors.New("another run is preparing this home")

// linkOnce makes one attempt. keep is true for a failure retrying cannot
// change — a directory of real transcripts in the way — so the caller stops
// and reports it rather than trying again.
func linkOnce(from, to string) (keep bool, err error) {
	switch fi, err := os.Lstat(from); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return false, err
	case fi.Mode()&os.ModeSymlink != 0:
		if at, err := os.Readlink(from); err == nil && at == to {
			return false, nil // already what this function exists to make
		}
		// Deliberately not removed first: placeLink renames over an existing
		// symlink, and removing it would open exactly the gap that replacing
		// by rename exists to close.
	default:
		entries, err := os.ReadDir(from)
		if errors.Is(err, os.ErrNotExist) {
			break // another run removed it; that is where this was heading
		}
		if err != nil {
			return false, err
		}
		if len(entries) > 0 {
			// ReadDir follows a symlink, and another run of the same harness
			// may have replaced the directory with the link in the moment
			// since the Lstat above — in which case what was just read is the
			// shared transcript directory, not transcripts in the way. Only a
			// path that is still a real directory earns the refusal, which is
			// the one error here that does not get retried.
			if fi, err := os.Lstat(from); err != nil || fi.Mode()&os.ModeSymlink != 0 {
				return false, errRacedWhileLinking
			}
			return true, fmt.Errorf("%s is a directory of transcripts, not a link to %s — move it aside (its sessions can be copied into %s) and run this again", from, to, to)
		}
		// rmdir rather than os.Remove, and no Lstat first: the kernel refuses
		// rmdir on anything that is not a directory, so "remove it only if
		// another run has not replaced it with the link" is one syscall
		// instead of a check and an act with a gap between them. os.Remove
		// would unlink a replacement symlink and reopen the gap placeLink
		// exists to close, and an Lstat before it only narrows that window
		// rather than closing it.
		switch err := syscall.Rmdir(from); {
		case err == nil, errors.Is(err, syscall.ENOENT):
			// Gone, by us or by whoever got there first.
		case errors.Is(err, syscall.ENOTDIR):
			// Already a symlink: another run won. The goal-state check at the
			// top of the next attempt ends this.
			return false, errRacedWhileLinking
		default:
			return false, &os.PathError{Op: "rmdir", Path: from, Err: err}
		}
	}
	return false, placeLink(from, to)
}

// placeLink puts the link at from without ever leaving the path empty.
//
// Symlink-then-rename rather than remove-then-symlink: rename replaces
// atomically, so a concurrent run — or a harness the winning run has already
// started — never observes a missing projects/ and never gets to create a real
// directory in the gap, which would send that run's transcripts somewhere only
// it can see. The remove above is still needed for a real directory, since
// rename will not replace one, but that case happens once per home rather than
// on every race.
func placeLink(from, to string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(from), ".link-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	staged := filepath.Join(tmp, "link")
	if err := os.Symlink(to, staged); err != nil {
		return err
	}
	return os.Rename(staged, from)
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
// says otherwise. A home that is not there is needs_login rather than free —
// a directory that does not exist cannot hold a login — so a label
// added to config.toml by hand is not usable until `yad account add` has made
// its home and run the login in it.
func Load(ctx context.Context, q *db.Queries, data string, cfg config.Config) ([]Account, error) {
	var rows []db.Account
	var wrows []db.AccountWindow
	if q != nil {
		var err error
		if rows, err = q.ListAllAccounts(ctx); err != nil {
			return nil, err
		}
		if wrows, err = q.ListAllAccountWindows(ctx); err != nil {
			return nil, err
		}
	}
	type key struct{ harness, label string }
	held := make(map[key]db.Account, len(rows))
	for _, r := range rows {
		held[key{r.Harness, r.Label}] = r
	}
	// The query orders by name, so each account's windows keep a stable order
	// and two syncs of an unchanged account produce the same report.
	windows := make(map[key][]v1.AccountWindow, len(wrows))
	for _, w := range wrows {
		k := key{w.Harness, w.Label}
		aw := v1.AccountWindow{Name: w.Name, UsedPercent: w.UsedPercent}
		if w.ResetsAt.Valid {
			at := time.UnixMilli(w.ResetsAt.Int64).UTC()
			aw.ResetsAt = &at
		}
		windows[k] = append(windows[k], aw)
	}
	now := time.Now()
	var out []Account
	for _, id := range cfg.HarnessIDs() {
		for _, label := range cfg.Harness[id].Accounts {
			a := Account{
				Harness: id, Label: label,
				Home:  HomeDir(data, id, label),
				State: v1.AccountFree,
			}
			a.Windows = windows[key{id, label}]
			if r, ok := held[key{id, label}]; ok {
				if r.LimitedUntil.Valid {
					t := time.UnixMilli(r.LimitedUntil.Int64).UTC()
					a.LimitedUntil = &t
				}
				// stateOf, not the stored column: a limit whose reset has
				// passed is over, and nothing comes along to write that down.
				a.State = stateOf(v1.AccountState(r.State), a.LimitedUntil, now)
				if r.UpdatedAt > 0 {
					a.UpdatedAt = time.UnixMilli(r.UpdatedAt).UTC()
				}
			}
			// A home that is not on disk cannot hold a login, so an account
			// the store calls free does not get to be free. This is what a
			// label left in config.toml after `yad account remove` looks like
			// to a daemon still holding the config it started with: without
			// it the account reads free, a run rebuilds the empty home the
			// owner just deleted, and the turn fails against a logged-out
			// harness.
			//
			// Only free is downgraded. A limited account is already unusable
			// and its reset time is DEV-27's to keep; overwriting it here
			// would lose when it comes back.
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
// here; a usage limit goes through SetLimit, which writes the state and the
// reset together.
func SetState(ctx context.Context, q *db.Queries, harness, label string, state v1.AccountState, now time.Time) error {
	return q.SetAccountState(ctx, db.SetAccountStateParams{
		Harness: harness, Label: label, State: string(state), UpdatedAt: now.UnixMilli(),
	})
}

// SetLimit parks an account until its window resets: the state and the reset
// in one statement, because either without the other is a bug (queries.sql).
//
// A usage limit only. Transient API throttling the harness retried by itself
// is a rate limit, never reaches here, and costs the account nothing
// (DOMAIN.md, "Usage limit").
//
// A limit the harness could not date is still recorded, and still dated: the
// account is out of quota, which is the part that must not be lost, and a
// limit nothing ever ends is a park, not a limit. limitWithoutReset says how
// long such a limit is taken to last and why.
func SetLimit(ctx context.Context, q *db.Queries, harness, label string, resetAt time.Time, now time.Time) error {
	if resetAt.IsZero() {
		resetAt = now.Add(limitWithoutReset)
	}
	return q.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: harness, Label: label,
		LimitedUntil: sql.NullInt64{Int64: resetAt.UnixMilli(), Valid: true},
		UpdatedAt:    now.UnixMilli(),
	})
}

// SetWindows records the latest use and reset of every window a turn heard
// about, one upsert each. A window the turn did not hear about keeps what it
// had: Codex's updates are sparse by design, and a turn that learned only
// about the primary window must not erase what the last one knew about the
// secondary.
func SetWindows(ctx context.Context, q *db.Queries, harness, label string, windows []v1.AccountWindow, now time.Time) error {
	for _, w := range windows {
		if w.Name == "" {
			continue
		}
		var resets sql.NullInt64
		if w.ResetsAt != nil && !w.ResetsAt.IsZero() {
			resets = sql.NullInt64{Int64: w.ResetsAt.UnixMilli(), Valid: true}
		}
		err := q.SetAccountWindow(ctx, db.SetAccountWindowParams{
			Harness: harness, Label: label, Name: w.Name,
			UsedPercent: w.UsedPercent, ResetsAt: resets, UpdatedAt: now.UnixMilli(),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Read is Load against the state database on disk, opened read-only and closed
// again: for the capability document and the CLI, which need account states
// without owning the runner's store.
//
// A profile with no state database yet has no states, so every account the
// owner configured is read by the same rule as any other: free when its home
// is on disk, needs_login when it is not.
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
