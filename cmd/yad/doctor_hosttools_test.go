package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// doctor lists every host tool with the path it was found at, the browser
// with its kind, and says what is wrong with one in full (YAD-201).
func TestDoctorListsTheHostTools(t *testing.T) {
	t.Setenv("YAD_CONFIG_DIR", privateDir(t))
	t.Setenv("YAD_DATA_DIR", shortDir(t))
	noTools(t)
	bin := t.TempDir()
	for name, body := range map[string]string{
		"git":              "echo 'git version 2.51.0'",
		"chromium-browser": "echo 'Chromium 139.0.7258.5'",
		"docker":           "exit 1",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"doctor"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{
		`(?m)^HOST TOOL +STATUS +VERSION +PATH$`,
		`(?m)^git +ready +2\.51\.0 +` + regexp.QuoteMeta(filepath.Join(bin, "git")) + `$`,
		`(?m)^gh +— *$`,
		`(?m)^docker +broken +` + regexp.QuoteMeta(filepath.Join(bin, "docker")) + `$`,
		`(?m)^browser +ready +chromium 139\.0\.7258\.5 +` + regexp.QuoteMeta(filepath.Join(bin, "chromium-browser")) + `$`,
		`(?m)^error: docker — `,
	} {
		if !regexp.MustCompile(want).MatchString(out.String()) {
			t.Errorf("doctor does not match %s:\n%s", want, out.String())
		}
	}
}

// --json is the harness detection alone, so it probes no host tool: gh's
// login check would cost it a network round trip for an answer it drops.
func TestDoctorJSONProbesNoHostTool(t *testing.T) {
	t.Setenv("YAD_CONFIG_DIR", privateDir(t))
	t.Setenv("YAD_DATA_DIR", shortDir(t))
	noTools(t)
	bin, marker := t.TempDir(), filepath.Join(t.TempDir(), "asked")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\ntouch '"+marker+"'\necho 'git version 2.51.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"doctor", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("doctor --json asked git its version")
	}
}
