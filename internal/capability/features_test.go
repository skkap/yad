package capability

import (
	"context"
	"slices"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
)

// A harness's list says what its adapter does, whatever else the machine has
// installed: one harness without a feature must not take it from the others'
// lists (decision 0069).
func TestEachHarnessListsWhatItsAdapterDoes(t *testing.T) {
	does := map[string]func(adapter.Adapter) bool{
		FeatureSteer:     adapter.Steers,
		FeatureInterrupt: func(adapter.Adapter) bool { return true },
		FeatureEffort:    adapter.AppliesEffort,
		FeatureFork:      adapter.Forks,
	}
	if len(does) != len(RunFeatures()) {
		t.Fatalf("this test knows %d per-run features and RunFeatures lists %v: teach it the new one", len(does), RunFeatures())
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
		}
	}
}

// machine is the harness reports of a machine where the harnesses named are
// installed and pass their probes, and the rest of the catalog is absent —
// what Build puts in the document, from detection onwards.
func machine(installed ...string) []v1.HarnessReport {
	var found []harness.Detected
	for _, h := range harness.Catalog() {
		found = append(found, harness.Detected{Harness: h, Present: slices.Contains(installed, h.ID), Version: "1.0.0"})
	}
	return Harnesses(found, config.Default(), nil)
}

func perRun(features []string) []string {
	return slices.DeleteFunc(slices.Clone(features), func(f string) bool { return !slices.Contains(RunFeatures(), f) })
}

// The runner-wide strings say what every harness this machine can drive may
// use, not what every harness the build knows may: a runner without OpenCode
// advertises steer, so a hub reading only the strings steers its Claude and
// Codex runs, and one with OpenCode does not, since a run there may target a
// harness with no steer. A harness that is present but cannot take a run,
// or is only recognised, takes nothing from the rest; a machine that can
// drive nothing advertises no per-run feature (decision 0069).
func TestTheRunnerWideStringsAreWhatEveryDrivableHarnessMayUse(t *testing.T) {
	all := RunFeatures()
	brokenOpenCode := machine("claude", "opencode")
	for i := range brokenOpenCode {
		if brokenOpenCode[i].ID == "opencode" {
			brokenOpenCode[i].Error = "not logged in on this machine"
		}
	}
	for _, tc := range []struct {
		name    string
		reports []v1.HarnessReport
		want    []string
	}{
		{"claude and codex, no opencode", machine("claude", "codex"), all},
		{"claude alone", machine("claude"), all},
		{"with opencode", machine("claude", "codex", "opencode"), HarnessFeatures("opencode")},
		{"opencode alone", machine("opencode"), HarnessFeatures("opencode")},
		{"opencode present and unable to take a run", brokenOpenCode, all},
		{"a recognised harness beside claude", machine("claude", "gemini", "cursor"), all},
		{"nothing to drive", machine(), nil},
		{"only recognised harnesses", machine("gemini"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Features(tc.reports)
			if !slices.Equal(perRun(got), tc.want) {
				t.Errorf("runner-wide per-run features %v, want %v", perRun(got), tc.want)
			}
			if !slices.Contains(got, FeatureHarnessFeatures) {
				t.Errorf("features %v do not say the harnesses carry their own lists", got)
			}
			// A hub reading only the strings is never told a run may use
			// what its harness cannot.
			for _, h := range tc.reports {
				for _, f := range perRun(got) {
					if drivable(h) && !slices.Contains(h.Features, f) {
						t.Errorf("runner-wide %q, and drivable %s lists %v", f, h.ID, h.Features)
					}
				}
			}
		})
	}
	// The case the ticket is about, said plainly: OpenCode has no steer in
	// ACP v1, and without it installed the runner-wide steer is back.
	if slices.Contains(HarnessFeatures("opencode"), FeatureSteer) {
		t.Fatal("opencode now steers: this test's with-opencode cases no longer show a harness taking a feature away")
	}
	if !slices.Contains(Features(machine("claude", "codex")), FeatureSteer) || slices.Contains(Features(machine("claude", "codex", "opencode")), FeatureSteer) {
		t.Error("steer runner-wide should follow whether opencode is installed")
	}
}

// Installing or removing a first-class harness changes the document's
// runner-wide strings with its harness reports, so the fingerprint moves and a
// hub re-reads both together; each harness's own list is the same either way.
func TestInstallingAHarnessMovesTheDocumentAndLeavesTheOtherListsAlone(t *testing.T) {
	doc := func(reports []v1.HarnessReport) v1.Capabilities {
		return v1.Capabilities{RunnerID: "r1", Harnesses: reports, ProtocolFeatures: Features(reports)}
	}
	without, with := doc(machine("claude", "codex")), doc(machine("claude", "codex", "opencode"))
	if Fingerprint(without) == Fingerprint(with) {
		t.Error("installing opencode left the fingerprint where it was")
	}
	if slices.Equal(without.ProtocolFeatures, with.ProtocolFeatures) {
		t.Errorf("installing opencode left the runner-wide strings at %v", with.ProtocolFeatures)
	}
	for _, id := range []string{"claude", "codex"} {
		if !slices.Equal(harnessReport(without.Harnesses, id).Features, harnessReport(with.Harnesses, id).Features) {
			t.Errorf("%s lists %v without opencode and %v with it", id, harnessReport(without.Harnesses, id).Features, harnessReport(with.Harnesses, id).Features)
		}
	}
}

// Build hands Features the reports it sends: on a machine with no harness at
// all the document advertises no per-run feature, and still every one of the
// runner's own.
func TestBuildAdvertisesThePerRunFeaturesOfWhatIsInstalled(t *testing.T) {
	noTools(t)
	got := Build(context.Background(), "r1", config.Default(), nil).ProtocolFeatures
	if len(perRun(got)) != 0 {
		t.Errorf("a machine with no harness advertises %v runner-wide", perRun(got))
	}
	for _, f := range []string{FeatureStartAt, FeatureDrain, FeatureCloseSession, FeatureLogin, FeatureHarnessFeatures} {
		if !slices.Contains(got, f) {
			t.Errorf("features %v leave out the runner's own %q", got, f)
		}
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
