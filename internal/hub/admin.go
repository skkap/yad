package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// adminTokenPrefix marks the third kind of secret, so a leaked one says what
// it opens: the service API, never the runner protocol.
const adminTokenPrefix = "yadadm_"

// adminNamePattern bounds a token's name: a person types it to revoke one.
var adminNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

const newAdminTokenAction = "create one on the hub's machine with `yad hub admin-token create`, and send it as `Authorization: Bearer <token>`"

// ErrAdminTokenNameTaken is a create under a name the hub already holds. The
// next action is left to the caller: it is a command naming this hub's database
// and profile, which only the CLI that opened the store knows.
var ErrAdminTokenNameTaken = errors.New("revoke that token first, or pick another --name")

// IssueAdminToken creates an admin token called name and returns it. Only its
// hash is stored: the returned string is the one copy there will ever be.
func IssueAdminToken(ctx context.Context, s *store.Store, name string, now time.Time) (string, error) {
	if !adminNamePattern.MatchString(name) {
		return "", fmt.Errorf("admin token name %q: use 1–64 lowercase letters, digits, dots, dashes or underscores, starting with a letter or digit", name)
	}
	tok, err := newSecret(adminTokenPrefix)
	if err != nil {
		return "", err
	}
	err = s.Tx(ctx, func(q *db.Queries) error {
		existing, err := q.ListAdminTokens(ctx)
		if err != nil {
			return err
		}
		for _, t := range existing {
			if t.Name == name {
				return fmt.Errorf("this hub already has an admin token called %q: %w", name, ErrAdminTokenNameTaken)
			}
		}
		return q.CreateAdminToken(ctx, db.CreateAdminTokenParams{Hash: hashSecret(tok), Name: name, CreatedAt: store.Ms(now)})
	})
	if err != nil {
		return "", err
	}
	return tok, nil
}

// RevokeAdminToken deletes the admin token called name. It stops working on
// the next request.
func RevokeAdminToken(ctx context.Context, s *store.Store, name string) error {
	n, err := s.RevokeAdminToken(ctx, name)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("this hub has no admin token called %q — `yad hub admin-token list` shows the ones it has", name)
	}
	return nil
}

// authenticateAdmin accepts only an admin token. A runner credential is a
// different table: a runner can never submit work, even to itself.
func (h *Hub) authenticateAdmin(ctx context.Context, tok string) error {
	if tok == "" {
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "no admin token", newAdminTokenAction)
	}
	_, err := h.store.GetAdminToken(ctx, hashSecret(tok))
	if errors.Is(err, sql.ErrNoRows) {
		return Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
			"this hub does not know that admin token — it was revoked, or it belongs to another hub", newAdminTokenAction)
	}
	return err
}
