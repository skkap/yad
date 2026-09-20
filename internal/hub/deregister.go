package hub

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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

// deregisteredUnstartedReason is what a run waiting in a session says when
// that session's runner disconnects. It names the next action, because the
// submitter's work is not lost — it has to be filed again in a new session,
// since the one it named lived on the machine that has gone.
const deregisteredUnstartedReason = "the runner holding this session deregistered before this run started; submit it again with a new session"

// deregister retires a runner's credential. What it holds is settled first:
// runs it had claimed are lost, because it is not coming back to report them,
// and offers it never listed go back in the queue for another runner, because
// an offer was never a run held.
//
// Its open sessions close, and the runs waiting in them are cancelled. A
// session is resumable only on the runner that holds it (DOMAIN.md), and that
// runner has just gone: a run queued in one would be offerable to nobody,
// because OfferCandidates takes only sessions unbound or bound to the asking
// runner and a queued run holds no lease for the sweep to lapse. This is the
// close 0011 gives the hub and 0035 spells out — "a session no runner has
// claimed closes there at once, and its unstarted runs are cancelled" — for a
// runner that has gone rather than one that never arrived. The binding is
// never cleared instead: the session's transcript is on that machine's disk,
// so offering it to another runner would be offering a resume that cannot
// succeed. The reason is the owner's, the same one `yad disconnect` records on
// the runner, so both sides say the same thing about the same session — before
// this, the runner closed them and deleted their workdirs while the hub kept
// them open, and the sync that would have reconciled the two is the one
// deregistering ends.
//
// The runner's row stays. Its sessions and runs reference it, and a runner
// that comes back registers under the same id with a token issued for it —
// the recovery [0020] already describes. What goes is the credential, replaced
// by the hash of a secret nobody was given, so nothing can authenticate as
// that runner again and no row has to be deleted to say so.
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
	reason := fmt.Sprintf("runner %s deregistered while it held this run", runner.ID)
	if why := cleanReason(in.Body.Reason); why != "" {
		reason += ": " + why
	}
	at := h.now()
	now := store.Ms(at)
	var lost, requeued int64
	var closed int
	err = h.store.Tx(ctx, func(q *db.Queries) error {
		var err error
		requeued, err = q.RequeueRunnerOffers(ctx, db.RequeueRunnerOffersParams{
			Now: now, RunnerID: sql.NullString{String: runner.ID, Valid: true}})
		if err != nil {
			return err
		}
		lost, err = q.LoseRunnerRuns(ctx, db.LoseRunnerRunsParams{
			Reason: sql.NullString{String: reason, Valid: true}, Now: now,
			RunnerID: sql.NullString{String: runner.ID, Valid: true}})
		if err != nil {
			return err
		}
		sessions, err := q.OpenSessionsOfRunner(ctx, sql.NullString{String: runner.ID, Valid: true})
		if err != nil {
			return err
		}
		for _, id := range sessions {
			n, err := q.RecordSessionClosed(ctx, db.RecordSessionClosedParams{
				ClosedAt: sql.NullInt64{Int64: now, Valid: true},
				Reason:   sql.NullString{String: string(v1.SessionClosedByOwner), Valid: true},
				ID:       id, RunnerID: sql.NullString{String: runner.ID, Valid: true},
			})
			if err != nil {
				return err
			}
			if n == 0 {
				continue // closed between the listing and here
			}
			closed++
			if err := cancelUnstarted(ctx, q, id, deregisteredUnstartedReason, at); err != nil {
				return err
			}
		}
		// The row stays; only the credential dies. RetireRunnerCredential
		// matches on the id alone, and the foreign keys hold the row in
		// place, so it cannot report no rows changed.
		if _, err := q.RetireRunnerCredential(ctx, db.RetireRunnerCredentialParams{
			CredentialHash: hashSecret(unheld), ID: runner.ID}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Watchers of a lost or cancelled run hear now rather than at the next sweep.
	if lost > 0 || requeued > 0 || closed > 0 {
		h.bell.ring()
	}
	return &ackOutput{Body: v1.Ack{OK: true}}, nil
}

// cleanReason makes a runner's words safe to store beside a run: one line,
// printable, and bounded. A hub is untrusted input to a runner, and a runner
// is untrusted input to a hub.
func cleanReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < ' ' || r == utf8.RuneError {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > maxDeregisterReason {
		// On a rune boundary: a reason cut mid-rune is not valid JSON text.
		for len(s) > maxDeregisterReason || !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return strings.TrimSpace(s)
}
