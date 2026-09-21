package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
	"github.com/skkap/yad/internal/shellword"
)

// runnerIDPattern bounds what a runner may call itself. The id becomes a path
// segment, a log field and a database key; yad's own ids are 16 hex digits.
// It starts with a letter or digit so "." and ".." can never be one.
var runnerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

const newTokenAction = "create a new registration token (`yad hub token create`, or the hub's Add runner) and run `yad connect` again"

func (h *Hub) registerRunner(ctx context.Context, in *registerInput) (*registerOutput, error) {
	tok := bearer(ctx)
	if tok == "" {
		return nil, Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "register needs the registration token as the bearer", newTokenAction)
	}
	caps := in.Body.Capabilities
	if !runnerIDPattern.MatchString(caps.RunnerID) {
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid, "capabilities.runner_id must be 1–128 letters, digits, dots, dashes or underscores, starting with a letter or digit",
			"send the id from the profile's runner-id file")
	}
	if caps.Name == "" {
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid, "capabilities.name is required", "set name in config.toml, or give the machine a hostname")
	}
	// Before the token is looked at, so a runner refused for its version
	// leaves the token unburned for the upgraded runner to use.
	if err := h.refuseOld(caps.YadVersion); err != nil {
		return nil, err
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
		t, err := q.GetRegistrationToken(ctx, hash)
		if err != nil {
			return refusedToken(t, err, now)
		}
		if err := refusedToken(t, nil, now); err != nil {
			return err
		}
		// Checked before the burn, so a refused request leaves the token
		// usable for what it was issued for.
		if t.ForRunner.Valid && t.ForRunner.String != caps.RunnerID {
			return Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
				fmt.Sprintf("this registration token re-registers runner %q, not %q", t.ForRunner.String, caps.RunnerID), newTokenAction)
		}
		if _, err := q.GetRunner(ctx, caps.RunnerID); err == nil && !t.ForRunner.Valid {
			// Runner ids are not secret. Replacing a known runner's
			// credential with any token would hand its sessions, and their
			// grants, to whoever holds one.
			return Fail(http.StatusConflict, v1.CodeConflict,
				fmt.Sprintf("runner %q is already registered with this hub, and this token is for a new runner", caps.RunnerID),
				fmt.Sprintf("to replace that runner's credential, create a token for it (`%s`) and run `yad connect` again", shellword.Command("yad", "hub", "token", "create", "--runner", caps.RunnerID)))
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		n, err := q.BurnRegistrationToken(ctx, db.BurnRegistrationTokenParams{Now: sql.NullInt64{Int64: store.Ms(now), Valid: true}, RunnerID: sql.NullString{String: caps.RunnerID, Valid: true}, Hash: hash})
		if err != nil {
			return err
		}
		if n == 0 {
			// Burned by a concurrent exchange between the read and here.
			return Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "the registration token was already used; a token registers one runner once", newTokenAction)
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
	// HubFeatures stays empty: this hub has no feature beyond the v1 baseline,
	// and a string nothing acts on would be one for a runner to guess about.
	return &registerOutput{Body: v1.RegisterResponse{
		RunnerCredential: cred,
		SyncIntervalMS:   int(h.interval / time.Millisecond),
		LeaseMS:          int(h.lease / time.Millisecond),
		MinVersion:       h.minVersion,
	}}, nil
}

// refusedToken says why a token cannot register anything, or returns nil when
// it can. Which reason it was is safe to tell the caller — they hold the
// token — and saves the owner a guess.
func refusedToken(t db.RegistrationToken, lookup error, now time.Time) error {
	switch {
	case errors.Is(lookup, sql.ErrNoRows):
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "this hub never issued that registration token", newTokenAction)
	case lookup != nil:
		return lookup
	case t.UsedAt.Valid:
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
			"the registration token was already used, at "+time.UnixMilli(t.UsedAt.Int64).UTC().Format(time.RFC3339)+"; a token registers one runner once", newTokenAction)
	case t.ExpiresAt <= store.Ms(now):
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
			"the registration token expired at "+time.UnixMilli(t.ExpiresAt).UTC().Format(time.RFC3339), newTokenAction)
	}
	return nil
}
