package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
)

// doctor says how much every run is given and from where, judged as the
// next run will read it — and that runs are refused when it cannot be given.
func TestDoctorReportsTheStandingInstructions(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"set", "Never pkill by name.\n", "note: every run is given 51 B of standing instructions after its hub's context, from instructions.text in "},
		{"over the cap", strings.Repeat("x", config.MaxInstructions), "error: the standing instructions in [instructions] are 16.0 KiB, over the 16.0 KiB a run may carry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := privateDir(t)
			t.Setenv("YAD_CONFIG_DIR", dir)
			t.Setenv("YAD_DATA_DIR", shortDir(t))
			noTools(t)
			src := "[instructions]\ntext = \"Docker is the colima context.\"\nfile = \"INSTRUCTIONS.md\"\n"
			for name, body := range map[string]string{"config.toml": src, "INSTRUCTIONS.md": tc.body} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out, errb bytes.Buffer
			if code := run(context.Background(), []string{"doctor"}, &out, &errb); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("doctor does not say %q:\n%s", tc.want, out.String())
			}
			if strings.Contains(out.String(), "colima") {
				t.Errorf("doctor printed the instructions' text:\n%s", out.String())
			}
		})
	}
}

func TestStatusShowsTheStandingInstructions(t *testing.T) {
	for _, tc := range []struct {
		in   *control.Instructions
		want string
	}{
		{nil, ""},
		{&control.Instructions{Bytes: 812}, "standing instructions: 812 B given to every run after its hub's context (limit 16.0 KiB)\n"},
		{&control.Instructions{Error: "instructions.file could not be read"}, "standing instructions refuse every run: instructions.file could not be read\n"},
	} {
		var b bytes.Buffer
		printStatus(&b, config.Paths{}, control.Status{Started: time.Now(), Instructions: tc.in}, time.Now())
		got := b.String()
		if tc.want == "" && strings.Contains(got, "standing instructions") || !strings.Contains(got, tc.want) {
			t.Errorf("status with %+v:\n%s\nwant a line %q", tc.in, got, tc.want)
		}
	}
}
