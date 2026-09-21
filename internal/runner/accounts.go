package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// Accounts is the daemon's one copy of the owner's account lists, and the only
// place anything in the runner reads them from: the executor choosing an
// account, a sync reporting health, a parked run deciding whether it is due,
// the login probe and the capability document all ask it. It is what makes
// `yad account add` and `yad account remove` take effect without a restart
// (decision 0043): Reload swaps the lists here, and every reader sees the new
// ones from its next read.
//
// It also knows which runs are on which account, because a removed account
// cannot simply vanish. A run already on it finishes there — its turn is a
// harness process with that home open — and no new run takes it. The home is
// deleted when the last run on it lets go, not while a harness is using it.
//
// A nil *Accounts is a runner with no accounts: every harness runs on its own
// default home.
type Accounts struct {
	data string
	// Binary resolves a harness to its executable, for the login check a
	// newly added account gets; nil is harness.Locate.
	Binary func(harness string) (string, bool)
	Log    *slog.Logger

	mu    sync.Mutex
	lists account.Lists
	// held is every run on each account right now, by the hold it took.
	held map[account.Ref]map[*accountHold]bool
	// doomed are removed accounts whose home the owner asked to delete while
	// a run was still on it: the last release deletes it.
	doomed map[account.Ref]bool
	// store is the runner's, once Serve has opened it. Before that there is
	// nothing to write to, and Serve prunes at open what a reload could not.
	store *store.Store
	// attached is closed once Serve has given the source its store and the
	// prune at start is done; Reload waits for it.
	attached   chan struct{}
	attachOnce sync.Once
}

// accountHold is one run on one account, from the moment the account is
// chosen for it until the run moves off it, parks or ends.
type accountHold struct {
	ref account.Ref
	run string
}

// NewAccounts is the lists as the daemon starts with them. data is where the
// account homes live.
func NewAccounts(data string, lists account.Lists) *Accounts {
	return &Accounts{data: data, lists: lists, held: map[account.Ref]map[*accountHold]bool{}, doomed: map[account.Ref]bool{}, attached: make(chan struct{})}
}

// Lists is a copy of the current lists: the caller may keep it for as long
// as one decision takes, and a reload in the middle does not change it.
func (a *Accounts) Lists() account.Lists {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(account.Lists, len(a.lists))
	for id, labels := range a.lists {
		out[id] = slices.Clone(labels)
	}
	return out
}

// Load is account.Load over the current lists.
func (a *Accounts) Load(ctx context.Context, q *db.Queries, now time.Time) ([]account.Account, error) {
	if a == nil {
		return nil, nil
	}
	return account.Load(ctx, q, a.data, a.Lists(), now)
}

// take puts a run on an account, unless the account is no longer listed —
// removed between the read the choice was made from and now. The check and
// the hold are one step under the lock Reload swaps the lists under, so a
// removal either sees this hold or this take sees the removal; there is no
// order in which a run starts on an account whose home is being deleted.
func (a *Accounts) take(r account.Ref, run string) (*accountHold, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.lists.Has(r) {
		return nil, false
	}
	h := &accountHold{ref: r, run: run}
	if a.held[r] == nil {
		a.held[r] = map[*accountHold]bool{}
	}
	a.held[r][h] = true
	return h, true
}

// release ends a hold. The last hold on a removed account is what deletes its
// home, if the owner asked, and what forgets the rows the run wrote to it
// after the removal: a turn records its windows and checks its login as it
// ends, and a removed account must not come back as rows nothing reads.
func (a *Accounts) release(h *accountHold) {
	if a == nil || h == nil {
		return
	}
	log := a.log().With("harness", h.ref.Harness, "account", h.ref.Label)
	a.mu.Lock()
	delete(a.held[h.ref], h)
	last := len(a.held[h.ref]) == 0
	if last {
		delete(a.held, h.ref)
	}
	removed := last && !a.lists.Has(h.ref)
	doomed := removed && a.doomed[h.ref]
	var aside string
	var asideErr error
	if doomed {
		delete(a.doomed, h.ref)
		aside, asideErr = a.setAside(h.ref)
	}
	st := a.store
	a.mu.Unlock()
	if !removed {
		return
	}
	if st != nil {
		// Not the run's context: a run cancelled on the way down still lets
		// go of its account, and the rows it leaves must still go.
		if err := account.Forget(context.Background(), st.Queries, h.ref); err != nil {
			log.Warn("could not forget a removed account's state; the next start prunes it", "err", err)
		}
	}
	if !doomed {
		return
	}
	if err := errors.Join(asideErr, removeAside(aside)); err != nil {
		log.Warn("the last run on a removed account has ended and its home could not be deleted", "err", err)
		return
	}
	log.Info("the last run on a removed account has ended; its home is deleted")
}

// Keep cancels a pending deletion of an account's home: `yad account add` is
// about to log the owner in there again. Sent before the login rather than
// after it, because the login takes minutes and a run on the removed account
// could end in the middle of it and delete the home the owner is logging in.
func (a *Accounts) Keep(r account.Ref) error {
	if err := checkRef(r); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.doomed, r)
	return nil
}

func checkRef(r account.Ref) error {
	if err := config.ValidName(r.Harness); err != nil {
		return fmt.Errorf("harness: %w", err)
	}
	if err := config.ValidName(r.Label); err != nil {
		return fmt.Errorf("account label: %w", err)
	}
	return nil
}

// setAside moves a home out of the way under a.mu, so the decision to delete
// it and its disappearing from the account's path are one step: a Keep or an
// add that comes after it finds no home there and gets a new one, rather than
// logging into a directory about to be removed. A rename in one directory is
// immediate whatever the home holds; the delete itself, removeAside, runs
// after the lock is let go. Empty with no error is a home already gone.
func (a *Accounts) setAside(r account.Ref) (string, error) {
	home := account.HomeDir(a.data, r.Harness, r.Label)
	// A leading dot and a suffix no label can carry (config.ValidName), so
	// it can never be read as an account.
	aside := filepath.Join(filepath.Dir(home), fmt.Sprintf(".%s.removed-%d", r.Label, time.Now().UnixNano()))
	switch err := os.Rename(home, aside); {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	}
	return aside, nil
}

func removeAside(aside string) error {
	if aside == "" {
		return nil
	}
	return os.RemoveAll(aside)
}

// attach gives the source the runner's store once Serve has opened it, and
// prunes the rows of every account the lists no longer name (decision 0043):
// an account removed while no daemon ran left its rows behind, because the
// CLI does not write the database.
func (a *Accounts) attach(ctx context.Context, st *store.Store) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.store = st
	lists := a.lists
	a.mu.Unlock()
	if st == nil {
		return
	}
	defer a.attachOnce.Do(func() { close(a.attached) })
	gone, err := account.Prune(ctx, st.Queries, lists)
	if err != nil {
		a.log().Warn("could not prune the state of accounts config.toml no longer lists; nothing reads it, and the next start tries again", "err", err)
	}
	for _, r := range gone {
		a.log().Info("forgot the state of an account config.toml no longer lists", "harness", r.Harness, "account", r.Label)
	}
}

// Changed is what Reload did with the account it was told about.
type Changed struct {
	// State is an added account's state after its login check.
	State v1.AccountState
	// Runs are the runs still on a removed account.
	Runs []string
}

// Reload takes up new lists — config.toml as it reads now — and acts on the
// one account the owner changed.
//
// For an account the owner removed, which must no longer be listed: no new run
// takes it from here on, its rows are forgotten, and its home is deleted now
// if no run is on it, or when the last run on it ends.
//
// For an account the owner added, which must be listed: the harness's own
// login check is asked, as the probe asks it, and only a definite answer is
// written — a check that could not answer leaves the row as it is, and an
// account with no row and a home on disk reads free. The owner's CLI has just
// seen that same check say yes; asking again here is what writes the row, and
// what brings back an account a run had parked in needs_login.
func (a *Accounts) Reload(ctx context.Context, lists account.Lists, r account.Ref, removed bool) (Changed, error) {
	// The names come off the control socket and become a path that ends in
	// os.RemoveAll; the CLI checked them, and so does the end that deletes.
	if err := checkRef(r); err != nil {
		return Changed{}, err
	}
	if lists.Has(r) == removed {
		// Refused before anything is swapped: the CLI wrote config.toml
		// before asking, so this is a file changed again since.
		if removed {
			return Changed{}, fmt.Errorf("config.toml still lists %s account %q — remove it again", r.Harness, r.Label)
		}
		return Changed{}, fmt.Errorf("config.toml does not list %s account %q — add it again", r.Harness, r.Label)
	}
	// Once Serve has opened the store, so that the answer is the daemon's
	// record and not the default a runner with no database reads: in the
	// moment a daemon is starting, the socket answers before the store is
	// open. A daemon whose store never opens is exiting; the bound is ctx.
	select {
	case <-a.attached:
	case <-ctx.Done():
	}
	a.mu.Lock()
	a.lists = lists
	var runs []string
	var aside string
	var asideErr error
	if removed {
		for h := range a.held[r] {
			runs = append(runs, h.run)
		}
		slices.Sort(runs)
		if len(runs) > 0 {
			a.doomed[r] = true
		} else {
			aside, asideErr = a.setAside(r)
		}
	} else {
		// Logged in again under a label whose old home was waiting to go:
		// that home is now the one the owner just logged in.
		delete(a.doomed, r)
	}
	st := a.store
	a.mu.Unlock()

	log := a.log().With("harness", r.Harness, "account", r.Label)
	if st != nil {
		if _, err := account.Prune(ctx, st.Queries, lists); err != nil {
			log.Warn("could not prune the state of accounts config.toml no longer lists; nothing reads it, and the next start tries again", "err", err)
		}
	}
	if removed {
		if err := errors.Join(asideErr, removeAside(aside)); err != nil {
			return Changed{}, fmt.Errorf("%s account %q is removed, and its home could not be deleted: %w", r.Harness, r.Label, err)
		}
		log.Info("the owner removed the account", "runs_still_on_it", len(runs))
		return Changed{Runs: runs}, nil
	}
	state := a.checkAdded(ctx, st, r, log)
	log.Info("the owner added the account", "state", state)
	return Changed{State: state}, nil
}

// checkAdded asks the harness whether a newly added account is logged in,
// records a definite answer, and returns the account's state as a run would
// now read it.
func (a *Accounts) checkAdded(ctx context.Context, st *store.Store, r account.Ref, log *slog.Logger) v1.AccountState {
	var q *db.Queries
	if st != nil {
		q = st.Queries
	}
	binary := a.Binary
	if binary == nil {
		binary = harness.Locate
	}
	if bin, ok := binary(r.Harness); ok && q != nil && account.CanLogIn(r.Harness) {
		switch in, err := account.LoggedIn(ctx, r.Harness, bin, account.HomeDir(a.data, r.Harness, r.Label)); {
		case err != nil:
			log.Warn("could not check the added account's login; it is left as it reads", "err", err)
		default:
			state := v1.AccountNeedsLogin
			if in {
				state = v1.AccountFree
			}
			if err := account.SetState(ctx, q, r.Harness, r.Label, state, time.Now()); err != nil {
				log.Warn("could not record the added account's state", "state", state, "err", err)
			}
		}
	}
	accounts, err := account.Load(ctx, q, a.data, a.Lists(), time.Now())
	if err != nil {
		log.Warn("could not read the added account's state back", "err", err)
		return ""
	}
	for _, acct := range accounts {
		if acct.Harness == r.Harness && acct.Label == r.Label {
			return acct.State
		}
	}
	return ""
}

func (a *Accounts) log() *slog.Logger {
	if a.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return a.Log
}
