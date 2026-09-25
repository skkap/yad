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

// connectAgain is the `yad connect` a refused runner's owner runs on the
// runner. The hub knows none of what it needs: the profile the runner is, the
// URL it dials this hub by — behind a proxy, not the one this hub listens on —
// or the name it gave the connection. So each is a placeholder saying what
// goes there, and pasted unfilled it is refused as a profile no runner can
// have, rather than registering the default runner, or a new one, in its
// place.
var connectAgain = shellword.Command("yad", "--profile", "<runner profile>", "connect", "<hub url>", "--name", "<connection name>", "--token", "<new token>")

// tokenCommand is the command, run on the hub's machine, that issues a
// registration token this hub will accept: Options.Command's when the hub was
// given one. Without it the hub knows neither the profile nor the --db it was
// opened with, and a bare `yad hub token create` issues into the default
// profile's database, whose token this hub refuses; both are placeholders.
func (h *Hub) tokenCommand(args ...string) string {
	return h.hubCommand(append([]string{"hub", "token", "create"}, args...)...)
}

// newTokenAction is the next action for a runner whose token or credential
// this hub will not take. It reaches the runner's owner, who may be at the
// runner and not the hub, so it says which command runs on which machine.
func (h *Hub) newTokenAction() string {
	return "create a new registration token on the hub's machine (`" + h.tokenCommand() + "`), then on the runner run `" + connectAgain + "`, filled in with its profile and the URL and connection name it reaches this hub by"
}

func (h *Hub) registerRunner(ctx context.Context, in *registerInput) (*registerOutput, error) {
	tok := bearer(ctx)
	if tok == "" {
		return nil, Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "register needs the registration token as the bearer", h.newTokenAction())
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
			return h.refusedToken(t, err, now)
		}
		if err := h.refusedToken(t, nil, now); err != nil {
			return err
		}
		// Checked before the burn, so a refused request leaves the token
		// usable for what it was issued for.
		if t.ForRunner.Valid && t.ForRunner.String != caps.RunnerID {
			return Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
				fmt.Sprintf("this registration token re-registers runner %q, not %q", t.ForRunner.String, caps.RunnerID), h.newTokenAction())
		}
		if _, err := q.GetRunner(ctx, caps.RunnerID); err == nil && !t.ForRunner.Valid {
			// Runner ids are not secret. Replacing a known runner's
			// credential with any token would hand its sessions, and their
			// grants, to whoever holds one.
			return Fail(http.StatusConflict, v1.CodeConflict,
				fmt.Sprintf("runner %q is already registered with this hub, and this token is for a new runner", caps.RunnerID),
				h.replaceCredentialAction(caps.RunnerID))
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		n, err := q.BurnRegistrationToken(ctx, db.BurnRegistrationTokenParams{Now: sql.NullInt64{Int64: store.Ms(now), Valid: true}, RunnerID: sql.NullString{String: caps.RunnerID, Valid: true}, Hash: hash})
		if err != nil {
			return err
		}
		if n == 0 {
			// Burned by a concurrent exchange between the read and here.
			return Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "the registration token was already used; a token registers one runner once", h.newTokenAction())
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
func (h *Hub) refusedToken(t db.RegistrationToken, lookup error, now time.Time) error {
	switch {
	case errors.Is(lookup, sql.ErrNoRows):
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "this hub never issued that registration token", h.newTokenAction())
	case lookup != nil:
		return lookup
	case t.UsedAt.Valid:
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
			"the registration token was already used, at "+time.UnixMilli(t.UsedAt.Int64).UTC().Format(time.RFC3339)+"; a token registers one runner once", h.newTokenAction())
	case t.ExpiresAt <= store.Ms(now):
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
			"the registration token expired at "+time.UnixMilli(t.ExpiresAt).UTC().Format(time.RFC3339), h.newTokenAction())
	}
	return nil
}

// replaceCredentialAction is the way to re-register a runner the hub already
// knows: a token issued for it, from this hub's database. A bare `yad hub
// token create` opens the default profile's database, which need not be this
// one, and a token issued there is refused here as one this hub never issued.
func (h *Hub) replaceCredentialAction(runnerID string) string {
	return "to replace that runner's credential, create a token for it on the hub's machine (`" + h.tokenCommand("--runner", runnerID) + "`), then on the runner run `" + connectAgain + "` with the profile, URL and connection name it already uses"
}
