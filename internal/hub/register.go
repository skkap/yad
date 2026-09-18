package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// runnerIDPattern bounds what a runner may call itself. The id becomes a path
// segment, a log field and a database key; yad's own ids are 16 hex digits.
var runnerIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

const newTokenAction = "create a new registration token (`yad hub token create`, or the hub's Add runner) and run `yad connect` again"

func (h *Hub) registerRunner(ctx context.Context, in *registerInput) (*registerOutput, error) {
	tok := bearer(ctx)
	if tok == "" {
		return nil, Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "register needs the registration token as the bearer", newTokenAction)
	}
	caps := in.Body.Capabilities
	if !runnerIDPattern.MatchString(caps.RunnerID) {
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid, "capabilities.runner_id must be 1–128 letters, digits, dots, dashes or underscores",
			"send the id from the profile's runner-id file")
	}
	if caps.Name == "" {
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid, "capabilities.name is required", "set name in config.toml, or give the machine a hostname")
	}
	doc, err := json.Marshal(caps)
	if err != nil {
		return nil, err
	}
	cred, err := newSecret(runnerCredentialPrefix)
	if err != nil {
		return nil, err
	}
	now := h.now()
	err = h.store.Tx(ctx, func(q *db.Queries) error {
		hash := hashSecret(tok)
		n, err := q.BurnRegistrationToken(ctx, db.BurnRegistrationTokenParams{Now: sql.NullInt64{Int64: store.Ms(now), Valid: true}, RunnerID: sql.NullString{String: caps.RunnerID, Valid: true}, Hash: hash})
		if err != nil {
			return err
		}
		if n == 0 {
			return refusedToken(ctx, q, hash)
		}
		// The fingerprint is left empty: the runner's first sync carries its
		// document and the fingerprint that goes with it, and hashing the
		// document here would bind every hub to how yad computes it.
		return q.UpsertRunner(ctx, db.UpsertRunnerParams{
			ID: caps.RunnerID, Name: caps.Name, CredentialHash: hashSecret(cred),
			Capabilities: string(doc), RegisteredAt: store.Ms(now),
		})
	})
	if err != nil {
		return nil, err
	}
	return &registerOutput{Body: v1.RegisterResponse{
		RunnerCredential: cred,
		SyncIntervalMS:   int(h.interval / time.Millisecond),
		LeaseMS:          int(h.lease / time.Millisecond),
	}}, nil
}

// refusedToken says why a token did not burn. Which of the three it was is
// safe to tell the caller — they hold the token — and saves the owner a guess.
func refusedToken(ctx context.Context, q *db.Queries, hash string) error {
	t, err := q.GetRegistrationToken(ctx, hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "this hub never issued that registration token", newTokenAction)
	case err != nil:
		return err
	case t.UsedAt.Valid:
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
			"the registration token was already used, at "+time.UnixMilli(t.UsedAt.Int64).UTC().Format(time.RFC3339)+"; a token registers one runner once", newTokenAction)
	default:
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
			"the registration token expired at "+time.UnixMilli(t.ExpiresAt).UTC().Format(time.RFC3339), newTokenAction)
	}
}
