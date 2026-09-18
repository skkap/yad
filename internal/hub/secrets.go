package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// Prefixes make a leaked secret recognisable to a secret scanner and to the
// person who finds it in a paste, and say which of the two it is.
const (
	registrationTokenPrefix = "yadreg_"
	runnerCredentialPrefix  = "yadrun_"
)

// Registration token lifetimes. A token is meant to be pasted into
// `yad connect` within minutes; an hour covers walking to the other machine,
// and a week is the most a token sitting in someone's notes should live.
const (
	DefaultTokenTTL = time.Hour
	MaxTokenTTL     = 7 * 24 * time.Hour
)

// newSecret returns 256 random bits behind a prefix.
func newSecret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("the OS entropy source failed: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSecret is how a secret is stored and looked up. The secrets are random,
// not chosen by a person, so a plain SHA-256 is enough.
func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// IssueRegistrationToken creates a one-time registration token valid for ttl
// and returns it with its expiry. Only its hash is stored: the returned string
// is the one copy there will ever be.
//
// forRunner empty makes a token for a new runner. A runner id makes a token
// that re-registers that runner, replacing its credential — and only that
// runner: a new-runner token refuses an id the hub already knows.
func IssueRegistrationToken(ctx context.Context, s *store.Store, ttl time.Duration, now time.Time, forRunner string) (string, time.Time, error) {
	if ttl <= 0 || ttl > MaxTokenTTL {
		return "", time.Time{}, fmt.Errorf("--ttl %s is out of range — pick a duration above zero and at most %s", ttl, MaxTokenTTL)
	}
	if forRunner != "" {
		if _, err := s.GetRunner(ctx, forRunner); errors.Is(err, sql.ErrNoRows) {
			return "", time.Time{}, fmt.Errorf("this hub has no runner %q — leave out --runner to register a new one", forRunner)
		} else if err != nil {
			return "", time.Time{}, err
		}
	}
	tok, err := newSecret(registrationTokenPrefix)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := now.Add(ttl)
	if err := s.CreateRegistrationToken(ctx, db.CreateRegistrationTokenParams{
		Hash: hashSecret(tok), CreatedAt: store.Ms(now), ExpiresAt: store.Ms(exp),
		ForRunner: sql.NullString{String: forRunner, Valid: forRunner != ""},
	}); err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}
