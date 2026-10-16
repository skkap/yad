package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInstructionsRead(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(p.Config, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	abs := write("ABS.md", "  from an absolute path\n")
	write("REL.md", "\nfrom the config dir\n\n")
	write("BLANK.md", "\n  \n")
	write("BIG.md", strings.Repeat("y", MaxInstructions-10))
	write("LATIN1.md", "caf\xe9")
	if err := os.Mkdir(filepath.Join(p.Config, "DIR.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A FIFO nothing writes to: opened, it would block the run for ever.
	if err := syscall.Mkfifo(filepath.Join(p.Config, "FIFO.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		c       InstructionsConfig
		want    string
		wantErr string
	}{
		{name: "none"},
		{name: "text", c: InstructionsConfig{Text: "\nleave colima-ci alone\n"}, want: "leave colima-ci alone"},
		{name: "relative file", c: InstructionsConfig{File: "REL.md"}, want: "from the config dir"},
		{name: "absolute file", c: InstructionsConfig{File: abs}, want: "from an absolute path"},
		{name: "text then file", c: InstructionsConfig{Text: "first", File: "REL.md"}, want: "first\n\nfrom the config dir"},
		{name: "blank file adds nothing", c: InstructionsConfig{Text: "first", File: "BLANK.md"}, want: "first"},
		{name: "missing file", c: InstructionsConfig{File: "GONE.md"}, wantErr: "instructions.file could not be read"},
		{name: "a directory", c: InstructionsConfig{File: "DIR.md"}, wantErr: "it is a directory"},
		{name: "a FIFO", c: InstructionsConfig{File: "FIFO.md"}, wantErr: "not a regular file"},
		{name: "not UTF-8", c: InstructionsConfig{File: "LATIN1.md"}, wantErr: "not UTF-8"},
		// Each under the cap, together over it: the cap is on what the run
		// carries.
		{name: "text and file over the cap together", c: InstructionsConfig{Text: strings.Repeat("x", 20), File: "BIG.md"}, wantErr: "over the 16.0 KiB a run may carry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.c.Read(p)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want one saying %q", err, tc.wantErr)
				}
				if got != "" {
					t.Errorf("an error came with text %q: nothing is given in part", got)
				}
			case err != nil:
				t.Errorf("err = %v", err)
			case got != tc.want:
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A file far over the cap is refused without being read whole.
func TestInstructionsFileIsReadBounded(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Config, "HUGE.md"), make([]byte, 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := readBounded(filepath.Join(p.Config, "HUGE.md"), MaxInstructions)
	if err != nil || len(b) != MaxInstructions+1 {
		t.Errorf("read %d bytes, %v; want the cap and one more", len(b), err)
	}
	if _, err := (InstructionsConfig{File: "HUGE.md"}).Read(p); err == nil || !strings.Contains(err.Error(), "refused rather than cut") {
		t.Errorf("err = %v, want the cap's refusal", err)
	}
}

func TestWithInstructions(t *testing.T) {
	for _, tc := range []struct{ context, instructions, want string }{
		{"hub", "", "hub"},
		{"", "", ""},
		{"", "rules", "## This machine\n\nrules"},
		{"  \n", "rules", "## This machine\n\nrules"},
		{"hub\n\n", "rules", "hub\n\n## This machine\n\nrules"},
	} {
		if got := WithInstructions(tc.context, tc.instructions); got != tc.want {
			t.Errorf("WithInstructions(%q, %q) = %q, want %q", tc.context, tc.instructions, got, tc.want)
		}
	}
}

func TestLoadInstructions(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "[instructions]\ntext = '''\nDocker is the colima context.\n'''\nfile = \"INSTRUCTIONS.md\"\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Instructions.Text != "Docker is the colima context.\n" || c.Instructions.FilePath(p) != filepath.Join(p.Config, "INSTRUCTIONS.md") || !c.Instructions.Set() {
		t.Errorf("instructions = %+v", c.Instructions)
	}
	for src, want := range map[string]string{
		"[instructions]\nfile = \"~/.config/yad/INSTRUCTIONS.md\"\n":                  "starts with ~",
		"[instructions]\ntext = \"" + strings.Repeat("x", MaxInstructions+1) + "\"\n": "instructions.text is 16.0 KiB, over",
		"[instructions]\nwords = \"x\"\n":                                             "unknown setting instructions.words",
	} {
		writeConfig(t, p, src)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.40q: err = %v, want one saying %q", src, err, want)
		}
	}
}

// A config with no instructions is written without the section, so every
// config.toml yad saved before them saves the same after.
func TestSaveLeavesOutUnsetInstructions(t *testing.T) {
	p := testPaths(t)
	if err := Save(p, Default()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "instructions") {
		t.Errorf("config.toml names instructions none were set:\n%s", b)
	}
	c := Default()
	c.Instructions = InstructionsConfig{Text: "a\nb", File: "I.md"}
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	again, err := Load(p)
	if err != nil || again.Instructions != c.Instructions {
		t.Errorf("round trip = %+v, %v; want %+v", again.Instructions, err, c.Instructions)
	}
}

// A spec's instructions are applied like any setting and reported by name:
// pages of prose would bury the rest of the report.
func TestApplyCarriesInstructionsAndHidesTheText(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "capacity = 2\n")
	got, err := Apply(context.Background(), p, specFile(t, "capacity = 2\n[instructions]\ntext = \"leave colima-ci alone\"\nfile = \"I.md\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range got.Changes {
		lines = append(lines, c.String())
	}
	if want := `instructions.file = "I.md" (was unset)|instructions.text set (was unset)`; strings.Join(lines, "|") != want {
		t.Errorf("changes = %q, want %q", lines, want)
	}
	if c, err := Load(p); err != nil || c.Instructions.Text != "leave colima-ci alone" {
		t.Errorf("after apply: %+v, %v", c.Instructions, err)
	}
}
