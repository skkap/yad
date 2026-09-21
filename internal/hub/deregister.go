package hub

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// maxDeregisterReason bounds the runner's own words for why it is going. They
// end up in every lost run's reason, which a submitter reads: long enough for
// a sentence, short enough that a hostile runner cannot write an essay into
// every run it held.
const maxDeregisterReason = 200

// departure is how the hub settles a runner that is not coming back: what
// each run it held says, why its sessions closed, and what becomes of the
// runs still waiting in them.
type departure struct {
	lost      string
	sessions  v1.SessionCloseReason
	unstarted unstartedEnd
}

// deregister retires a runner's credential, after settling everything the hub
// was holding for it.
//
// The runner's row stays. Its sessions and runs reference it, and a runner
// that comes back registers under the same id with a token issued for it,
// which is the recovery UpsertRunner's own comment describes. What goes is
// the credential, replaced by the hash of a secret nobody was given, so
// nothing can authenticate as that runner again and no row has to be deleted
// to say so.
func (h *Hub) deregister(ctx context.Context, in *deregisterInput) (*ackOutput, error) {
	runner, err := h.authenticate(ctx, in.Runner)
	if err != nil {
		return nil, err
	}
	// Never handed out: a credential nobody holds is how this row says it has
	// no runner, while credential_hash stays unique and NOT NULL.
	unheld, err := newSecret(runnerCredentialPrefix)
	if err != nil {
		return nil, err
	}
	lost := fmt.Sprintf("runner %s deregistered while it held this run", runner.ID)
	if why := cleanReason(in.Body.Reason); why != "" {
		lost += ": " + why
	}
	gone := departure{
		lost: lost,
		// The runner's owner took the machine away, and every session on
		// its disk with it.
		sessions:  v1.SessionClosedByOwner,
		unstarted: runnerDeregistered,
	}
	err = h.store.Tx(ctx, func(q *db.Queries) error {
		if err := abandon(ctx, q, runner.ID, gone, h.now()); err != nil {
			return err
		}
		return q.RetireRunnerCredential(ctx, db.RetireRunnerCredentialParams{CredentialHash: hashSecret(unheld), ID: runner.ID})
	})
	if err != nil {
		return nil, err
	}
	// Watchers of a lost or failed run hear now rather than at their next poll.
	h.bell.ring()
	return &ackOutput{Body: v1.Ack{OK: true}}, nil
}

// abandon settles everything the hub holds for a runner that is not coming
// back, in the one order that leaves nothing stranded:
//
//   - offers it never claimed go back in the queue first, because an offer
//     was never a run held — and one in a session of its own is then ended
//     with that session below, rather than re-offered to nobody;
//   - runs it held are lost, because nobody is left to report them;
//   - its open sessions close, and the runs waiting in them end. Such a run
//     would otherwise be offerable to no one — OfferCandidates takes only
//     sessions unbound or bound to the asking runner — and would stay queued
//     for ever, since a queued run holds no lease for the sweep to lapse.
//
// Deregistering is today's only caller. A runner that simply stops syncing
// strands its sessions the same way, and the day the protocol says when a hub
// may decide such a runner has gone, that decision calls this with its own
// departure and leaves the credential alone.
func abandon(ctx context.Context, q *db.Queries, runnerID string, gone departure, now time.Time) error {
	me := sql.NullString{String: runnerID, Valid: true}
	if _, err := q.RequeueRunnerOffers(ctx, db.RequeueRunnerOffersParams{Now: store.Ms(now), RunnerID: me}); err != nil {
		return err
	}
	if _, err := q.LoseRunnerRuns(ctx, db.LoseRunnerRunsParams{
		Reason: sql.NullString{String: gone.lost, Valid: true}, Now: store.Ms(now), RunnerID: me,
	}); err != nil {
		return err
	}
	sessions, err := q.OpenSessionsOfRunner(ctx, me)
	if err != nil {
		return err
	}
	for _, id := range sessions {
		if err := closeHere(ctx, q, id, gone.sessions, gone.unstarted, now); err != nil {
			return err
		}
	}
	return nil
}

// cleanReason makes a runner's words safe to store beside a run: one line,
// printable, and bounded. A hub is untrusted input to a runner, and a runner
// is untrusted input to a hub.
func cleanReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < ' ' || r == 0x7f || r == utf8.RuneError {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > maxDeregisterReason {
		// On a rune boundary: a reason cut mid-rune is not valid UTF-8.
		for len(s) > maxDeregisterReason || !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return strings.TrimSpace(s)
}
