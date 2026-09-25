package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/skkap/yad/internal/hub/store/db"
)

// A hub login's token is held for one delivery and its code until the runner
// takes it (decision 0055): the schema blanks each as the login's state moves
// past where it is needed, whoever moves it. Neither survives in the file
// once the store has closed, as a grant's value does not (decision 0041).
func TestALoginsSecretsLeaveTheFile(t *testing.T) {
	s, file := open(t)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO runners (id, name, credential_hash, capabilities, fingerprint, registered_at) VALUES ('r1', 'r1', 'h', '{}', 'fp', 0)`); err != nil {
		t.Fatal(err)
	}
	setLogin := func(id, state string) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, `UPDATE logins SET state = ? WHERE id = ?`, state, id); err != nil {
			t.Fatal(err)
		}
	}
	row := func(id string) db.Login {
		t.Helper()
		l, err := s.GetLogin(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	var secrets []string
	for i := range 200 {
		tok := fmt.Sprintf("sk-ant-oat01-token-%03d", i)
		code := fmt.Sprintf("code-%03d-%s", i, string(bytes.Repeat([]byte("c"), i)))
		secrets = append(secrets, tok, code)
		tokenID, linkID := fmt.Sprintf("tok-%03d", i), fmt.Sprintf("link-%03d", i)
		if err := s.CreateLogin(ctx, db.CreateLoginParams{ID: tokenID, RunnerID: "r1", Harness: "claude", Account: "work", Method: "token", Token: tok, CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateLogin(ctx, db.CreateLoginParams{ID: linkID, RunnerID: "r1", Harness: "claude", Method: "link", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		// Held while it is still to be delivered or taken.
		setLogin(tokenID, "requested")
		setLogin(linkID, "waiting")
		if err := s.SetLoginCode(ctx, db.SetLoginCodeParams{Code: code, ID: linkID}); err != nil {
			t.Fatal(err)
		}
		if row(tokenID).Token != tok || row(linkID).Code != code {
			t.Fatalf("a secret was blanked before its runner had it")
		}
		setLogin(tokenID, "starting")
		setLogin(linkID, "checking")
		if row(tokenID).Token != "" || row(linkID).Code != "" {
			t.Fatalf("a secret outlived its delivery: %+v %+v", row(tokenID), row(linkID))
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, file + "-wal"} {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		left := 0
		for _, v := range secrets {
			if bytes.Contains(b, []byte(v)) {
				left++
			}
		}
		if left > 0 {
			t.Errorf("%s still holds %d of %d login secrets", filepath.Base(path), left, len(secrets))
		}
	}
}
