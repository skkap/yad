package conformance

import (
	"slices"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
)

// misleads is the per-run features doc advertises runner-wide, beside
// harness_features, that a harness it can drive does not list: what a hub
// reading only the runner-wide strings would be wrong about.
func misleads(doc v1.Capabilities) []string {
	var out []string
	for _, f := range capability.RunFeatures() {
		if !slices.Contains(doc.ProtocolFeatures, f) {
			continue
		}
		for _, h := range doc.Harnesses {
			if capability.Drivable(doc, h.ID) && !slices.Contains(h.Features, f) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// The suite's document is a probe because no yad runner sends it: its
// runner-wide strings name per-run features its one harness's list does not,
// and a yad runner's name only what every harness it can drive lists — on a
// machine without OpenCode, where steer is runner-wide, and on one with it,
// where it is not (decision 0069). If a yad runner could send the probe, a
// hub failing run/gated-features would be failed for gating correctly on a
// document real runners send.
func TestNoYadRunnerSendsTheSuitesProbe(t *testing.T) {
	probe := (&session{opts: Options{Harness: DefaultHarness}}).doc()
	if got := misleads(probe); len(got) == 0 {
		t.Fatalf("the suite's document names no runner-wide feature its harness lacks, so run/gated-features probes nothing: %v", probe.ProtocolFeatures)
	}
	for _, installed := range [][]string{
		{"claude", "codex"},
		{"claude", "codex", "opencode"},
		{"opencode"},
		{},
	} {
		var found []harness.Detected
		for _, h := range harness.Catalog() {
			found = append(found, harness.Detected{Harness: h, Present: slices.Contains(installed, h.ID), Version: "1.0.0"})
		}
		reports := capability.Harnesses(found, config.Default(), nil)
		doc := v1.Capabilities{Harnesses: reports, ProtocolFeatures: capability.Features(reports)}
		if got := misleads(doc); len(got) != 0 {
			t.Errorf("a yad runner with %v installed advertises %v runner-wide, which a harness it drives does not list", installed, got)
		}
		// With nothing to drive there is no run to steer.
		wantSteer := len(installed) > 0 && !slices.Contains(installed, "opencode")
		if slices.Contains(doc.ProtocolFeatures, capability.FeatureSteer) != wantSteer {
			t.Errorf("a yad runner with %v installed advertises %v, and steer runner-wide = %v is wanted", installed, doc.ProtocolFeatures, wantSteer)
		}
	}
}
