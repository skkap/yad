package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/supervise"
)

// The values below stand in for an owner's; a failure names the variable and
// never prints one.
const (
	ownerDockerHost = "unix:///tmp/dev-188/colima/docker.sock"
	ownerBashMax    = "1800000"
)

// declareEnv writes [env] into the profile's config.toml and makes sure the
// layer a command sets in this process is gone when the test ends.
func declareEnv(t *testing.T, configDir string, env map[string]string) {
	t.Helper()
	t.Cleanup(func() { supervise.SetOwnerEnv(nil) })
	p := config.Paths{Config: configDir}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Env = env
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
}

// hasVar reports whether env, one NAME=value a line, sets name to value.
func hasVar(env, name, value string) bool {
	for _, line := range strings.Split(env, "\n") {
		if line == name+"="+value {
			return true
		}
	}
	return false
}

// A run's harness and its repository's setup hook both get what [env]
// declares — the DOCKER_HOST colima needs and a harness setting like
// BASH_MAX_TIMEOUT_MS — over whatever the runner's own environment says, with
// nothing set in the repository (DEV-188).
func TestE2EOwnerEnvReachesTheHarnessAndTheSetupHook(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		t.Setenv(k, v)
	}
	m := newMachine(t, claudeE2E)
	if err := os.Symlink(git, filepath.Join(os.Getenv("PATH"), "git")); err != nil {
		t.Fatal(err)
	}
	// What a service started without it would have instead: the runner's
	// own DOCKER_HOST, which [env] replaces.
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")

	root := t.TempDir()
	bare := filepath.Join(root, "acme.git")
	work := t.TempDir()
	hookEnv := filepath.Join(t.TempDir(), "hook.env")
	t.Setenv("E2E_HOOK_ENV", hookEnv)
	// To a file, never to the hook's output, which a run's events carry.
	hook := "#!/bin/sh\nprintf 'DOCKER_HOST=%s\\nBASH_MAX_TIMEOUT_MS=%s\\n' \"$DOCKER_HOST\" \"$BASH_MAX_TIMEOUT_MS\" > \"$E2E_HOOK_ENV\"\necho set up\n"
	if err := os.MkdirAll(filepath.Join(work, ".worktree"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".worktree", "setup"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", bare},
		{"-C", work, "init", "--quiet", "--initial-branch=main"},
		{"-C", work, "add", "-A"},
		{"-C", work, "commit", "--quiet", "-m", "initial"},
		{"-C", work, "push", "--quiet", bare, "main"},
	} {
		if out, err := exec.Command(git, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	paths := config.Paths{Config: m.p.config, Data: m.p.data}
	cfg, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workdirs.Roots = []string{root}
	if err := config.Save(paths, cfg); err != nil {
		t.Fatal(err)
	}
	declareEnv(t, m.p.config, map[string]string{"DOCKER_HOST": ownerDockerHost, "BASH_MAX_TIMEOUT_MS": ownerBashMax})

	harnessEnv := filepath.Join(t.TempDir(), "harness.env")
	t.Setenv(fakeClaudeEnv, harnessEnv)
	m.ok(m.submitArgs("--run-id", "env-1", "--git", "file://"+bare, "--branch", "e2e/env", m.h.instruction)...)
	d := m.daemon()
	code, watched, errs := m.watch("env-1")
	if code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, watched, d.out.String())
	}
	for who, file := range map[string]string{"the harness": harnessEnv, "the setup hook": hookEnv} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s wrote no environment: %v", who, err)
		}
		for name, want := range map[string]string{"DOCKER_HOST": ownerDockerHost, "BASH_MAX_TIMEOUT_MS": ownerBashMax} {
			if !hasVar(string(b), name, want) {
				t.Errorf("%s did not get [env]'s %s", who, name)
			}
		}
	}
	if strings.Contains(watched, ownerDockerHost) {
		t.Error("the run's events carry DOCKER_HOST's value")
	}
}

// A hub's grant under a name [env] declares fails the run as refused, naming
// the grant, before any harness starts; the hub learns nothing of [env] but
// the name it sent itself.
func TestE2EAGrantNamedForAnOwnerVariableIsRefused(t *testing.T) {
	m := newMachine(t, claudeE2E)
	declareEnv(t, m.p.config, map[string]string{"DOCKER_HOST": ownerDockerHost, "BASH_MAX_TIMEOUT_MS": ownerBashMax})
	started := filepath.Join(t.TempDir(), "harness.env")
	t.Setenv(fakeClaudeEnv, started)
	_, err := m.client().Submit(context.Background(), hubapi.SubmitRequest{
		RunID: "env-clash", Harness: m.h.name, Model: m.h.model,
		Brief:  v1.Brief{Instruction: m.h.instruction},
		Grants: []v1.Grant{{Name: "DOCKER_HOST", Value: "tcp://hub-chosen:2375", As: v1.GrantEnv}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := m.daemon()
	m.watch("env-clash")
	run, err := m.client().Run(context.Background(), "env-clash")
	if err != nil {
		t.Fatal(err)
	}
	if run.Result == nil || run.Result.State != v1.RunFailed || run.Result.Error == nil || run.Result.Error.Class != "refused" ||
		!strings.Contains(run.Result.Error.Message, "grant DOCKER_HOST") {
		t.Fatalf("result %+v, want a refusal naming the grant\ndaemon:\n%s", run.Result, d.out.String())
	}
	if msg := run.Result.Error.Message; strings.Contains(msg, ownerDockerHost) || strings.Contains(msg, "BASH_MAX_TIMEOUT_MS") {
		t.Error("the refusal tells the hub more of [env] than the name it sent")
	}
	if _, err := os.Stat(started); err == nil {
		t.Error("the harness started")
	}
}

// doctor names what [env] declares and never a value, and the capability
// document's docker probe runs with it, as the daemon's does.
func TestDoctorNamesTheOwnerEnvAndProbesWithIt(t *testing.T) {
	p := newProfile(t)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	declareEnv(t, p.config, map[string]string{"DOCKER_HOST": ownerDockerHost, "BASH_MAX_TIMEOUT_MS": ownerBashMax})
	seen := filepath.Join(t.TempDir(), "docker.saw")
	t.Setenv("E2E_DOCKER_SAW", seen)
	docker := "#!/bin/sh\nprintf '%s' \"$DOCKER_HOST\" > \"$E2E_DOCKER_SAW\"\ncase \"$1\" in\n--version) echo 'Docker version 28.0.0, build x' ;;\nversion) echo '28.0.0' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "docker"), []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}

	code, out, errs := p.yad("", "doctor")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "[env] gives every child yad starts BASH_MAX_TIMEOUT_MS, DOCKER_HOST (values not shown)") {
		t.Errorf("doctor does not name the declared variables:\n%s", out)
	}
	if strings.Contains(out+errs, ownerDockerHost) || strings.Contains(out+errs, ownerBashMax) {
		t.Error("doctor printed a declared value")
	}
	// The capability document's docker probe, which `yad harnesses` runs as
	// the daemon does.
	if code, _, errs := p.yad("", "harnesses"); code != 0 {
		t.Fatalf("harnesses: exit %d: %s", code, errs)
	}
	if b, err := os.ReadFile(seen); err != nil || string(b) != ownerDockerHost {
		t.Error("the docker probe did not run with [env]'s DOCKER_HOST")
	}
}

// A config.toml the daemon would refuse is said in doctor, which still
// answers the rest and exits as it would have.
func TestDoctorSaysWhyConfigTomlDoesNotLoad(t *testing.T) {
	p := newProfile(t)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	const value = "sk-ant-not-a-real-key-but-never-printed"
	if err := os.WriteFile(filepath.Join(p.config, "config.toml"), []byte("[env]\nANTHROPIC_API_KEY = \""+value+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := p.yad("", "doctor")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"error: config.toml could not be read, and the daemon will not start until it can", "env.ANTHROPIC_API_KEY is refused", "No drivable harness"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out+errs, value) {
		t.Error("doctor printed the refused value")
	}
}

// Docker on colima: its context names a socket no tool reading DOCKER_HOST or
// the default socket finds, and doctor says to declare DOCKER_HOST until
// [env] does.
func TestDoctorWarnsOfADockerContextOffTheDefaultSocket(t *testing.T) {
	p := newProfile(t)
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_CONTEXT", "")
	socket := filepath.Join(t.TempDir(), "colima.sock")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"colima"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// sha256("colima"), where docker keeps the context's endpoint.
	meta := filepath.Join(dir, "contexts", "meta", "f24fd3749c1368328e2b149bec149cb6795619f244c5b584e844961215dadd16")
	if err := os.MkdirAll(meta, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(`{"Name":"colima","Endpoints":{"docker":{"Host":"unix://`+socket+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, out, _ := p.yad("", "doctor")
	if !strings.Contains(out, `warning: docker's context "colima" points at `+socket) || !strings.Contains(out, `DOCKER_HOST = "unix://`+socket+`"`) {
		t.Fatalf("doctor does not warn about the colima socket:\n%s", out)
	}

	// A context [env] chooses is named by the variable, never by its value.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"default"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	declareEnv(t, p.config, map[string]string{"DOCKER_CONTEXT": "colima"})
	_, out, _ = p.yad("", "doctor")
	if !strings.Contains(out, "warning: the docker context [env]'s DOCKER_CONTEXT names points at "+socket) || strings.Contains(out, `"colima"`) {
		t.Errorf("doctor does not warn by the variable's name alone:\n%s", out)
	}

	declareEnv(t, p.config, map[string]string{"DOCKER_HOST": "unix://" + socket, "DOCKER_CONTEXT": "colima"})
	_, out, _ = p.yad("", "doctor")
	if strings.Contains(out, "docker context") || strings.Contains(out, "docker's context") {
		t.Errorf("doctor still warns once [env] declares DOCKER_HOST:\n%s", out)
	}
}
