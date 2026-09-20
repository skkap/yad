package hub

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
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

// deregister retires a runner's credential. What it holds is settled first:
// runs it had claimed are lost, because it is not coming back to report them,
// and offers it never listed go back in the queue for another runner, because
// an offer was never a run held.
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
	now := store.Ms(h.now())
	var lost, requeued int64
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
		n, err := q.RetireRunnerCredential(ctx, db.RetireRunnerCredentialParams{
			CredentialHash: hashSecret(unheld), ID: runner.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			// Deregistered by another request between the check and here.
			return Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
				"this runner is no longer registered with this hub",
				"nothing more is needed here; `yad connect` registers it again")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Watchers of a lost run hear now rather than at the next sweep.
	if lost > 0 || requeued > 0 {
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
