package capability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
)

// The document says that standing instructions exist and nothing more: not
// their text, not their file, and not their size, so editing them moves no
// fingerprint (decision 0089).
func TestTheDocumentAdvertisesOnlyThatInstructionsExist(t *testing.T) {
	noTools(t)
	cfg := config.Default()
	none := Build(context.Background(), "r1", cfg, State{})
	if none.Instructions != nil {
		t.Errorf("instructions = %v with none configured, want absent", *none.Instructions)
	}
	b, _ := json.Marshal(none)
	if strings.Contains(string(b), "instructions") {
		t.Errorf("a runner with none sends the key: %s", b)
	}

	cfg.Instructions = config.InstructionsConfig{Text: "leave colima-ci alone", File: "/Users/alice/.config/yad/SECRET-PLACE.md"}
	set := Build(context.Background(), "r1", cfg, State{})
	if set.Instructions == nil || !*set.Instructions {
		t.Fatalf("instructions = %v, want true", set.Instructions)
	}
	b, _ = json.Marshal(set)
	for _, leak := range []string{"colima", "SECRET-PLACE", "/Users/alice"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("the document carries %q: %s", leak, b)
		}
	}
	cfg.Instructions.Text = "something else entirely"
	if edited := Build(context.Background(), "r1", cfg, State{}); Fingerprint(edited) != Fingerprint(set) {
		t.Error("editing the instructions moved the fingerprint")
	}
	if Fingerprint(none) == Fingerprint(set) {
		t.Error("setting instructions did not move the fingerprint")
	}
}
