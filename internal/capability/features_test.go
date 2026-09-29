package capability

import (
	"slices"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
)

// A harness's list says what its adapter does, and the runner-wide string
// says what every first-class harness's does: a hub reading only the strings
// must never be told a run may use a feature its harness cannot, and one
// harness without a feature must not take it from the others' lists
// (decision 0069).
func TestEachHarnessListsWhatItsAdapterDoesAndTheRunnerWhatAllDo(t *testing.T) {
	does := map[string]func(adapter.Adapter) bool{
		FeatureSteer:     adapter.Steers,
		FeatureInterrupt: func(adapter.Adapter) bool { return true },
		FeatureEffort:    adapter.AppliesEffort,
		FeatureFork:      adapter.Forks,
	}
	if len(does) != len(RunFeatures()) {
		t.Fatalf("this test knows %d per-run features and RunFeatures lists %v: teach it the new one", len(does), RunFeatures())
	}
	every := map[string]bool{}
	for _, f := range RunFeatures() {
		every[f] = true
	}
	for _, h := range harness.Catalog() {
		if h.Kind != harness.FirstClass {
			if got := HarnessFeatures(h.ID); got != nil {
				t.Errorf("recognised harness %s lists %v, and takes no run to use them in", h.ID, got)
			}
			continue
		}
		a, ok := adapterFor(h.ID)
		if !ok {
			t.Fatalf("first-class harness %s has no adapter in Adapters(), which is what yad daemon registers", h.ID)
		}
		got := HarnessFeatures(h.ID)
		for _, f := range RunFeatures() {
			if slices.Contains(got, f) != does[f](a) {
				t.Errorf("harness %s lists %v, and its adapter's answer for %q is %v", h.ID, got, f, does[f](a))
			}
			every[f] = every[f] && does[f](a)
		}
	}
	for _, f := range RunFeatures() {
		if slices.Contains(Features(), f) != every[f] {
			t.Errorf("runner-wide %q advertised = %v, and every first-class adapter supports it = %v", f, slices.Contains(Features(), f), every[f])
		}
	}
	if !slices.Contains(Features(), FeatureHarnessFeatures) {
		t.Errorf("features %v do not say the harnesses carry their own lists", Features())
	}
}

// The harness list reaches the document for a first-class harness only: a
// recognised one takes no run.
func TestTheDocumentCarriesEachFirstClassHarnesssList(t *testing.T) {
	found := []harness.Detected{
		{Harness: harness.Harness{ID: "claude", Kind: harness.FirstClass}, Present: true},
		{Harness: harness.Harness{ID: "gemini", Kind: harness.Recognised}, Present: true},
	}
	reps := Harnesses(found, config.Default(), nil)
	if !slices.Equal(reps[0].Features, HarnessFeatures("claude")) || len(reps[0].Features) == 0 {
		t.Errorf("claude reports features %v, want %v", reps[0].Features, HarnessFeatures("claude"))
	}
	if reps[1].Features != nil {
		t.Errorf("recognised gemini reports features %v", reps[1].Features)
	}
}

// A hub gates a run's feature on its harness's list when the runner sends the
// lists, and on the runner-wide string when it does not; a feature that is
// not a per-run one is the runner's alone either way.
func TestRunMayUseReadsTheHarnesssListWhenThereIsOne(t *testing.T) {
	lists := v1.Capabilities{
		ProtocolFeatures: []string{FeatureStartAt, FeatureInterrupt, FeatureHarnessFeatures},
		Harnesses: []v1.HarnessReport{
			{ID: "claude", Features: []string{FeatureSteer, FeatureInterrupt, FeatureEffort, FeatureFork}},
			{ID: "opencode", Features: []string{FeatureInterrupt}},
			{ID: "codex"},
		},
	}
	older := v1.Capabilities{
		ProtocolFeatures: []string{FeatureStartAt, FeatureSteer, FeatureEffort},
		Harnesses:        []v1.HarnessReport{{ID: "claude"}, {ID: "codex"}},
	}
	for _, tc := range []struct {
		name             string
		doc              v1.Capabilities
		harness, feature string
		want             bool
	}{
		{"listed, not runner-wide", lists, "claude", FeatureEffort, true},
		{"runner-wide, not listed", lists, "opencode", FeatureInterrupt, true},
		{"missing from its list", lists, "opencode", FeatureFork, false},
		{"an empty list is none", lists, "codex", FeatureInterrupt, false},
		{"a harness not in the document", lists, "cursor", FeatureInterrupt, false},
		{"the runner's own feature", lists, "opencode", FeatureStartAt, true},
		{"the runner's own feature, absent", lists, "claude", FeatureDrain, false},
		{"no lists: runner-wide", older, "codex", FeatureEffort, true},
		{"no lists: not runner-wide", older, "claude", FeatureFork, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RunMayUse(tc.doc, tc.harness, tc.feature); got != tc.want {
				t.Errorf("RunMayUse(%s, %s) = %v, want %v", tc.harness, tc.feature, got, tc.want)
			}
		})
	}
}
