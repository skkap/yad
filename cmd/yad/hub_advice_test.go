package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hub"
	hubstore "github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// A hub refusing to hand a known runner's credential to a new-runner token
// says how to issue one for it — on the database `yad hub serve` opened, and
// its profile. A bare `yad hub token create` issues into the default
// profile's database, and the token it prints is refused here as one this hub
// never issued. The --db path itself stays on the hub's machine.
func TestReRegistrationAdviceNamesTheServedDatabase(t *testing.T) {
	p := config.Paths{Profile: "side", Config: t.TempDir(), Data: t.TempDir()}
	// The profile's own database, but under a YAD_DATA_DIR the answer cannot
	// carry: a bare shell would look in the default place.
	t.Setenv("YAD_DATA_DIR", t.TempDir())
	relocated, err := config.Resolve("side")
	if err != nil {
		t.Fatal(err)
	}
	placeholder := []string{"yad", "--profile", "side", "hub", "token", "create", "--runner", "r1", "--db", "<the database yad hub serve was given>"}
	for _, tc := range []struct {
		name string
		p    config.Paths
		db   string
		want []string
	}{
		{"the profile's own database", p, p.HubDB(), []string{"yad", "--profile", "side", "hub", "token", "create", "--runner", "r1"}},
		{"a database named with --db", p, filepath.Join(t.TempDir(), "other hub.db"), placeholder},
		{"the profile's own database under YAD_DATA_DIR", relocated, relocated.HubDB(), placeholder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := hubstore.Open(context.Background(), tc.db)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h := hub.New(hub.Options{Store: s, Command: hubAnswerCommand(tc.p, tc.db)})
			register := func(tok string) v1.ErrorEnvelope {
				t.Helper()
				body, _ := json.Marshal(v1.RegisterRequest{Capabilities: v1.Capabilities{
					RunnerID: "r1", Name: "r1", YadVersion: "dev", OS: "linux", Arch: "amd64",
					ProtocolFeatures: capability.Features(), Capacity: v1.Capacity{Total: 1}, Harnesses: []v1.HarnessReport{},
				}})
				req := httptest.NewRequest("POST", hub.BasePath+"/runners/register", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+tok)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set(v1.HeaderProtocol, v1.Version)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				var env v1.ErrorEnvelope
				json.Unmarshal(rec.Body.Bytes(), &env)
				return env
			}
			for i := range 2 {
				tok, _, err := hub.IssueRegistrationToken(context.Background(), s, time.Hour, time.Now(), "")
				if err != nil {
					t.Fatal(err)
				}
				env := register(tok)
				if i == 0 {
					if env.Error.Code != "" {
						t.Fatalf("first registration refused: %+v", env.Error)
					}
					continue
				}
				if env.Error.Code != v1.CodeConflict {
					t.Fatalf("a new-runner token for a known runner: %+v", env.Error)
				}
				if strings.Contains(env.Error.NextAction, tc.db) {
					t.Errorf("the advice carries the hub's database path to the runner: %s", env.Error.NextAction)
				}
				shellwordtest.Check(t, onlyCommand(t, env.Error.NextAction, "yad --profile side hub"), tc.want...)
			}
		})
	}
}

// Revoking a name the hub does not hold points at the list of the names it
// does — in the database the revoke opened.
func TestAdminTokenRevokeMissCommandCarriesTheDatabase(t *testing.T) {
	p := newProfile(t)
	db := filepath.Join(t.TempDir(), "it's a hub.db")
	code, _, errs := p.yad("", "--profile", "side", "hub", "admin-token", "revoke", "nope", "--db", db)
	if code == 0 {
		t.Fatal("revoked a token the hub does not have")
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile side hub"), dirsEnv(t),
		"yad", "--profile", "side", "hub", "admin-token", "list", "--db", db)
}
