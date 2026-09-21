package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hub"
	"github.com/skkap/yad/internal/hub/store"
)

// profile runs yad commands against one profile across calls.
type profile struct {
	t            *testing.T
	config, data string
}

func newProfile(t *testing.T) *profile {
	noTools(t)
	p := &profile{t: t, config: t.TempDir(), data: shortDir(t)}
	return p
}

func (p *profile) yad(in string, args ...string) (int, string, string) {
	p.t.Helper()
	p.t.Setenv("YAD_CONFIG_DIR", p.config)
	p.t.Setenv("YAD_DATA_DIR", p.data)
	old := stdin
	stdin = strings.NewReader(in)
	defer func() { stdin = old }()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// The operator's path end to end: a token from `yad hub token create`, a hub
// serving that database, `yad connect` on the runner — and no secret on any
// terminal but the token's one printing.
func TestTokenCreateAndConnect(t *testing.T) {
	hubSide, runnerSide := newProfile(t), newProfile(t)
	code, tok, errs := hubSide.yad("", "hub", "token", "create", "--ttl", "10m")
	if code != 0 {
		t.Fatalf("token create: exit %d: %s", code, errs)
	}
	tok = strings.TrimSpace(tok)
	if !strings.HasPrefix(tok, "yadreg_") || strings.Contains(errs, tok) || !strings.Contains(errs, "once") {
		t.Fatalf("stdout %q stderr %q", tok, errs)
	}

	s, err := store.Open(context.Background(), filepath.Join(hubSide.data, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	srv := httptest.NewServer(hub.New(hub.Options{Store: s}))
	defer srv.Close()
	url := srv.URL + hub.BasePath

	// The URL first and flags after it, as the usage line has it; the token
	// on stdin, as decision 0020 recommends.
	code, out, errs := runnerSide.yad(tok+"\n", "connect", url, "--token", "-", "--name", "home")
	if code != 0 {
		t.Fatalf("connect: exit %d: %s", code, errs)
	}
	cred, err := config.Paths{Config: runnerSide.config}.Credential("home")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{tok, cred} {
		if strings.Contains(out+errs, secret) {
			t.Error("connect printed a secret")
		}
	}
	if !strings.Contains(out, `as "home"`) {
		t.Errorf("connect said %q", out)
	}
	_, doc, _ := runnerSide.yad("", "harnesses")
	if strings.Contains(doc, cred) {
		t.Error("yad harnesses shows the credential")
	}

	// Spent: the same token again is refused with the next action.
	code, _, errs = runnerSide.yad("", "connect", "--token", tok, url, "--name", "home")
	if code == 0 || !strings.Contains(errs, "yad hub token create") {
		t.Errorf("reused token: exit %d: %s", code, errs)
	}
	if strings.Contains(errs, tok) {
		t.Error("the refusal echoed the token")
	}
}

func TestConnectUsage(t *testing.T) {
	p := newProfile(t)
	for _, args := range [][]string{
		{"connect"},
		{"connect", "https://hub.example/v1", "extra", "--token", "t"},
	} {
		if code, _, errs := p.yad("", args...); code == 0 || !strings.Contains(errs, "usage") && !strings.Contains(errs, "unexpected") {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
	}
	if code, _, errs := p.yad("", "connect", "https://hub.example/v1"); code == 0 || !strings.Contains(errs, "no registration token") {
		t.Errorf("no token: exit %d %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(p.config, "config.toml")); err == nil {
		t.Error("a refused connect wrote config.toml")
	}
}

func TestTokenCreateRefusesABadTTL(t *testing.T) {
	p := newProfile(t)
	if code, out, errs := p.yad("", "hub", "token", "create", "--ttl", "0s"); code == 0 || out != "" || !strings.Contains(errs, "--ttl") {
		t.Errorf("exit %d out %q err %q", code, out, errs)
	}
}

// A hub URL with userinfo is refused before anything is spent or written: it
// can authenticate nothing, since the runner sends its token as a bearer, and
// accepting it would put the secret in config.toml. The refusal says what to
// do and names the URL without the secret — the output of a connect run from
// a script is kept.
func TestConnectRefusesAHubURLWithUserinfo(t *testing.T) {
	const password = "hunter2"
	hubSide, runnerSide := newProfile(t), newProfile(t)
	code, tok, errs := hubSide.yad("", "hub", "token", "create", "--ttl", "10m")
	if code != 0 {
		t.Fatalf("token create: exit %d: %s", code, errs)
	}
	s, err := store.Open(context.Background(), filepath.Join(hubSide.data, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	srv := httptest.NewServer(hub.New(hub.Options{Store: s}))
	defer srv.Close()

	for _, url := range []string{
		strings.Replace(srv.URL, "http://", "http://runner:"+password+"@", 1) + hub.BasePath,
		strings.Replace(srv.URL, "http://", "http://"+password+"@", 1) + hub.BasePath,
		"http://runner:" + password + "@hub.example/v1",
	} {
		code, out, errs := runnerSide.yad(tok, "connect", url, "--token", "-", "--name", "home")
		if code == 0 {
			t.Fatalf("connect to %s was accepted", url)
		}
		if strings.Contains(out+errs, password) {
			t.Errorf("the password is printed:\n%s%s", out, errs)
		}
		if !strings.Contains(errs, "redacted@") || !strings.Contains(errs, "drop the user:password@ part") {
			t.Errorf("the refusal does not name the URL and the way out:\n%s", errs)
		}
	}
	if _, err := os.Stat(filepath.Join(runnerSide.config, "config.toml")); err == nil {
		t.Error("a refused connect wrote config.toml")
	}
	// The token was never spent, so the same token connects the same hub
	// once the userinfo is gone.
	if code, _, errs := runnerSide.yad(tok, "connect", srv.URL+hub.BasePath, "--token", "-", "--name", "home"); code != 0 {
		t.Errorf("connect without the userinfo: exit %d: %s", code, errs)
	}
}
