package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// InstructionsConfig is config.toml's [instructions]: what the machine's owner
// tells every run on this runner, whichever hub sent it — "Docker is the
// colima context, leave colima-ci alone", "never pkill by name, CI shares
// this box" (decision 0088). yad's own and hub-neutral: no protocol field
// reaches it, and a hub learns only that it is set.
type InstructionsConfig struct {
	// Text is written in config.toml itself, read when the daemon starts
	// like every other setting there.
	Text string `toml:"text,omitempty"`
	// File is a file of instructions, relative to the profile's config
	// directory or absolute, read afresh at every run's start, so an edit
	// reaches the next run without a restart. No ~ is expanded, as in [env].
	File string `toml:"file,omitempty"`
}

// MaxInstructions bounds the text and the file together. Every run carries
// them in its harness's system prompt or developer instructions, which every
// request of every turn pays for, and 16 KiB — some four thousand tokens —
// is several pages of rules: past it is a document, which belongs in a file
// the instructions name, not in every request. Over it a run is refused
// rather than handed a cut text, because a rule cut in half can say the
// opposite of the whole one.
const MaxInstructions = 16 << 10

// InstructionsHeading is what the runner writes above the instructions in a
// run's context, after the hub's: whose words they are, so a harness reading
// a hub's brief and these together can tell the two apart.
const InstructionsHeading = "## This machine"

// Set is whether the owner configured any instructions at all — what the
// capability document advertises, never their text.
func (c InstructionsConfig) Set() bool { return c.Text != "" || c.File != "" }

// FilePath is File resolved against the profile's config directory; "" when
// none is configured.
func (c InstructionsConfig) FilePath(p Paths) string {
	if c.File == "" || filepath.IsAbs(c.File) {
		return c.File
	}
	return filepath.Join(p.Config, c.File)
}

// Read is the instructions as a run receives them: the text, then the file,
// a blank line between, trimmed. "" with no error when none are configured.
//
// An error is for the owner and refuses the run: a file that names nothing
// or cannot be read, and instructions over MaxInstructions. A run that went
// ahead without them would do what they forbid, and nothing would say why.
func (c InstructionsConfig) Read(p Paths) (string, error) {
	parts := []string{}
	if t := strings.TrimSpace(c.Text); t != "" {
		parts = append(parts, t)
	}
	if path := c.FilePath(p); path != "" {
		b, err := readBounded(path, MaxInstructions)
		if err != nil {
			return "", fmt.Errorf("instructions.file could not be read (%w) — fix the file, or the path in [instructions] in config.toml", err)
		}
		if !utf8.Valid(b) {
			return "", errors.New("instructions.file is not UTF-8 text — save it as UTF-8")
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			parts = append(parts, t)
		}
	}
	out := strings.Join(parts, "\n\n")
	if len(out) > MaxInstructions {
		return "", fmt.Errorf("the standing instructions in [instructions] are %s, over the %s a run may carry — shorten them; they are refused rather than cut", Size(len(out)), Size(MaxInstructions))
	}
	return out, nil
}

// readBounded reads path, stopping a byte past limit: a file far over the
// cap is refused by the size check without being read whole into memory.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, errors.New("it is a directory")
	}
	return io.ReadAll(io.LimitReader(f, limit+1))
}

// WithInstructions is a run's context with the owner's instructions after the
// hub's, under InstructionsHeading. Either may be empty.
func WithInstructions(context, instructions string) string {
	if instructions == "" {
		return context
	}
	section := InstructionsHeading + "\n\n" + instructions
	if strings.TrimSpace(context) == "" {
		return section
	}
	return strings.TrimRight(context, "\n") + "\n\n" + section
}

// Size is n bytes as a person reads it: 812 B, 3.2 KiB.
func Size(n int) string {
	if n < 1<<10 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
}

func (c InstructionsConfig) validate() []error {
	var errs []error
	if strings.HasPrefix(c.File, "~") {
		errs = append(errs, fmt.Errorf("instructions.file %q starts with ~, which is not expanded — write it relative to the config directory, like \"INSTRUCTIONS.md\", or in full", c.File))
	}
	if strings.ContainsRune(c.Text, 0) || strings.ContainsRune(c.File, 0) {
		errs = append(errs, errors.New("[instructions] holds a NUL byte"))
	}
	if n := len(strings.TrimSpace(c.Text)); n > MaxInstructions {
		errs = append(errs, fmt.Errorf("instructions.text is %s, over the %s a run may carry — shorten it", Size(n), Size(MaxInstructions)))
	}
	return errs
}
