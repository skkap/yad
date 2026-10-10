package account

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A token account's home carries its token as the login a claude started by
// the run finds there, since Claude Code hands CLAUDE_CODE_OAUTH_TOKEN to no
// child (DEV-172). Only a login yad marked, with no refresh token, is yad's:
// any other is never touched, and what Claude keeps beside the login stays.
func TestATokenAccountsHomeCarriesItsLoginForANestedClaude(t *testing.T) {
	const first, second = "sk-ant-oat01-first", "sk-ant-oat01-second"
	const claudes = `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1}}`
	const mcp = `{"mcpOAuth":{"server":{"accessToken":"m"}}}`
	set := func(tok string) func(*testing.T, string, string) {
		return func(t *testing.T, data, home string) {
			if err := SetToken(home, tok); err != nil {
				t.Fatal(err)
			}
		}
	}
	clear := func(t *testing.T, data, home string) {
		if err := ClearToken(home); err != nil {
			t.Fatal(err)
		}
	}
	prepare := func(t *testing.T, data, home string) {
		if _, err := Ensure(data, "claude", "tl"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		before string // the credentials file before, "" for none
		steps  []func(t *testing.T, data, home string)
		// login is the token the file's login must carry, "" for no token
		// login; same says the file must be exactly as it was before.
		login string
		same  bool
		// others are the entries besides the login the file must still hold.
		others []string
	}{
		{name: "a token stored", steps: []func(*testing.T, string, string){set(first)}, login: first},
		{name: "a token replaced", steps: []func(*testing.T, string, string){set(first), set(second)}, login: second},
		{name: "a token replaced keeps what claude kept beside it", before: mcp,
			steps: []func(*testing.T, string, string){set(first), set(second)}, login: second, others: []string{"mcpOAuth"}},
		{name: "a token cleared", steps: []func(*testing.T, string, string){set(first), clear}},
		{name: "a token cleared keeps what claude kept beside it", before: mcp,
			steps: []func(*testing.T, string, string){set(first), clear}, others: []string{"mcpOAuth"}},
		{name: "claude's own login is never replaced", before: claudes,
			steps: []func(*testing.T, string, string){set(first), prepare}, same: true},
		{name: "claude's own login is never removed", before: claudes,
			steps: []func(*testing.T, string, string){set(first), clear, prepare}, same: true},
		{name: "a login with no refresh token that yad did not write is left alone",
			before: `{"claudeAiOauth":{"accessToken":"someone-elses"}}`,
			steps:  []func(*testing.T, string, string){set(first), clear, prepare}, same: true},
		{name: "claude's own login made over yad's is claude's",
			before: `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r"},"yadTokenLogin":"stale"}`,
			steps:  []func(*testing.T, string, string){set(first), set(second), clear, prepare}, same: true},
		{name: "a file that is not claude's format is left alone", before: "not json",
			steps: []func(*testing.T, string, string){set(first)}, same: true},
		{name: "a token stored before this existed gets its login at the next run",
			steps: []func(*testing.T, string, string){
				func(t *testing.T, data, home string) {
					if err := os.WriteFile(filepath.Join(home, tokenFile), []byte(first), 0o600); err != nil {
						t.Fatal(err)
					}
				},
				prepare,
			}, login: first},
		{name: "the login, once others can read it, is written 0600 again",
			steps: []func(*testing.T, string, string){
				set(first),
				func(t *testing.T, data, home string) {
					if err := os.Chmod(filepath.Join(home, credentialsFile), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				prepare,
			}, login: first},
		{name: "a token others can read reaches no nested claude",
			steps: []func(*testing.T, string, string){
				set(first),
				func(t *testing.T, data, home string) {
					if err := os.Chmod(filepath.Join(home, tokenFile), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				prepare,
			}},
		{name: "a check of a login beside the token goes without it, and the next run has it again",
			steps: []func(*testing.T, string, string){
				set(first),
				func(t *testing.T, data, home string) {
					if err := dropTokenLogin(home); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Lstat(filepath.Join(home, credentialsFile)); err == nil {
						t.Error("the token's login is still in the home while a login beside it is checked")
					}
				},
				prepare,
			}, login: first},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			data := t.TempDir()
			home, err := Ensure(data, "claude", "tl")
			if err != nil {
				t.Fatal(err)
			}
			f := filepath.Join(home, credentialsFile)
			if tc.before != "" {
				if err := os.WriteFile(f, []byte(tc.before), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, step := range tc.steps {
				step(t, data, home)
			}
			b, err := os.ReadFile(f)
			if tc.same {
				if string(b) != tc.before {
					t.Fatalf("the file is %q, want it left as %q", b, tc.before)
				}
				return
			}
			if tc.login == "" && len(tc.others) == 0 {
				if err == nil {
					t.Fatalf("the home still holds %s: %s", credentialsFile, b)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(f); err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("%s is %v (%v), want 0600: it holds the token", credentialsFile, info.Mode().Perm(), err)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatalf("%s is not JSON: %v", credentialsFile, err)
			}
			for _, k := range tc.others {
				if _, ok := doc[k]; !ok {
					t.Errorf("%s lost %q, which claude kept there", credentialsFile, k)
				}
			}
			got, ok := doc[credentialsKey]
			_, marked := doc[markerKey]
			if tc.login == "" {
				if ok || marked {
					t.Errorf("%s still holds yad's login: %s", credentialsFile, b)
				}
				return
			}
			if !marked {
				t.Errorf("%s does not say yad wrote it: %s", credentialsFile, b)
			}
			// Exactly what claude makes of CLAUDE_CODE_OAUTH_TOKEN in memory,
			// so a nested claude runs on the same login as the run.
			want := `{"accessToken":"` + tc.login + `","refreshToken":null,"expiresAt":null,"scopes":["user:inference"],"subscriptionType":null,"rateLimitTier":null}`
			if string(got) != want {
				t.Errorf("the login is %s, want %s", got, want)
			}
		})
	}
}

// A link at the credentials file's name is not a file yad wrote, and what it
// points at is never written through it.
func TestATokenLoginNeverWritesThroughALink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, err := Ensure(t.TempDir(), "claude", "tl")
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(elsewhere, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, credentialsFile)); err != nil {
		t.Fatal(err)
	}
	if err := SetToken(home, "sk-ant-oat01-x"); err != nil {
		t.Fatal(err)
	}
	if at, err := os.Readlink(filepath.Join(home, credentialsFile)); err != nil || at != elsewhere {
		t.Errorf("the link is %q (%v), want it left pointing at %s", at, err, elsewhere)
	}
	if b, _ := os.ReadFile(elsewhere); string(b) != `{}` {
		t.Errorf("the file the link points at was written: %s", b)
	}
}

// Codex takes no token, so its home never gets a claude login.
func TestACodexHomeGetsNoTokenLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	data := t.TempDir()
	home, err := Ensure(data, "codex", "tl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, tokenFile), []byte("sk-ant-oat01-x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(data, "codex", "tl"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, credentialsFile)); err == nil {
		t.Error("a codex home was given a claude login")
	}
}
