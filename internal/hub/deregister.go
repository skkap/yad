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

// departure is how the hub settles a runner it has given up on: what each run
// it held says — lost, or cancelled for a claim a cancel was asked for — why
// its sessions closed, and what becomes of the runs still waiting in them. mayReturn is a runner that kept its credential — it only
// went silent — so its workdirs are still on its disk, and it is sent
// close_session for each of those sessions if it ever syncs again.
type departure struct {
	lost      string
	cancelled string
	sessions  v1.SessionCloseReason
	unstarted unstartedEnd
	mayReturn bool
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
		lost:      lost,
		cancelled: fmt.Sprintf("cancelled before runner %s started it; the runner deregistered", runner.ID),
		// The runner's owner took the machine away, and every session on
		// its disk with it.
		sessions:  v1.SessionClosedByOwner,
		unstarted: runnerDeregistered,
	}
	err = h.store.Tx(ctx, func(q *db.Queries) error {
		if _, err := h.current(ctx, q, runner); err != nil {
			return err
		}
		if err := abandon(ctx, q, runner.ID, gone, h.now()); err != nil {
			return err
		}
		if _, err := q.EndRunnerLogins(ctx, db.EndRunnerLoginsParams{
			Error: fmt.Sprintf("runner %s deregistered before the login ended", runner.ID), Now: store.Ms(h.now()), RunnerID: runner.ID,
		}); err != nil {
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
//   - a claim a cancel was asked for ends cancelled, as a sync leaving it out
//     would end it (decision 0061): no sync said it started, and that end is
//     the one asked for;
//   - the other runs it held are lost, because nobody is left to report them;
//   - its open sessions close, and the runs waiting in them end. Such a run
//     would otherwise be offerable to no one — OfferCandidates takes only
//     sessions unbound or bound to the asking runner — and would stay queued
//     for ever, since a queued run holds no lease for the sweep to lapse.
//     A session the runner already closed holds none: its report ended them.
//
// Two callers: deregister, which then retires the credential, and a runner
// silent past abandon-after (decision 0046), which keeps it.
func abandon(ctx context.Context, q *db.Queries, runnerID string, gone departure, now time.Time) error {
	me := sql.NullString{String: runnerID, Valid: true}
	if _, err := q.RequeueRunnerOffers(ctx, db.RequeueRunnerOffersParams{Now: store.Ms(now), RunnerID: me}); err != nil {
		return err
	}
	if err := cancelWithdrawn(ctx, q, runnerID, nil, gone.cancelled, now); err != nil {
		return err
	}
	if _, err := q.LoseRunnerRuns(ctx, db.LoseRunnerRunsParams{
		Reason: sql.NullString{String: gone.lost, Valid: true}, Now: store.Ms(now), RunnerID: me,
	}); err != nil {
		return err
	}
	sessions, err := q.SessionsToSettle(ctx, me)
	if err != nil {
		return err
	}
	for _, id := range sessions {
		if err := closeHere(ctx, q, id, gone.sessions, gone.unstarted, now); err != nil {
			return err
		}
		if gone.mayReturn {
			if err := q.OweSessionClose(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// abandonSilent gives up the sessions of every runner that has not synced for
// longer than abandon-after (decision 0046). Its offers and held runs went
// with their leases long before; what is left is the sessions bound to it,
// whose queued runs would otherwise wait for good, since a queued run holds no
// lease. The credential stays: a runner that was only switched off syncs
// again, hears close_session for each session, and takes new work.
//
// Silence is counted from the last sync this hub answered, and never from
// before the hub itself started: a hub that was down for a day has heard from
// nobody, and closing every session in the fleet on its first sweep back would
// punish the runners for the hub's own absence.
func (h *Hub) abandonSilent(ctx context.Context, q *db.Queries, now time.Time) error {
	cutoff := now.Add(-h.abandonAfter)
	if h.started.After(cutoff) {
		return nil
	}
	silent, err := q.SilentRunners(ctx, sql.NullInt64{Int64: store.Ms(cutoff), Valid: true})
	if err != nil {
		return err
	}
	for _, id := range silent {
		gone := departure{
			lost:      fmt.Sprintf("runner %s had not synced for more than %s while it held this run", id, h.abandonAfter),
			cancelled: fmt.Sprintf("cancelled before runner %s started it; the runner had not synced for more than %s", id, h.abandonAfter),
			sessions:  v1.SessionClosed,
			unstarted: unstartedEnd{v1.RunFailed, fmt.Sprintf(
				"its session's runner %s had not synced for more than %s, so the hub closed the session; submit the work to a new session", id, h.abandonAfter)},
			mayReturn: true,
		}
		if err := abandon(ctx, q, id, gone, now); err != nil {
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
