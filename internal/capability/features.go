package capability

import (
	"slices"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/claude"
	"github.com/skkap/yad/internal/adapter/codex"
	"github.com/skkap/yad/internal/adapter/opencode"
)

// FeatureHarnessFeatures is a runner whose harness reports each carry
// features: the per-run features a run on that harness may use, the whole
// list, absent meaning none. A hub reading it gates a run's effort and fork,
// and its steer and interrupt controls, on the list of the harness the run
// targets rather than on the runner-wide strings — which a runner lists only
// while every harness it can drive supports them — so that one harness without
// a feature cannot switch it off for every other (decision 0069).
const FeatureHarnessFeatures = "harness_features"

// RunFeatures are the features whose answer depends on the harness a run
// targets, in the order a harness's list carries them. Everything else in
// Features is the runner's own and holds for every run.
func RunFeatures() []string {
	return []string{FeatureSteer, FeatureInterrupt, FeatureEffort, FeatureFork}
}

// Adapters are the adapters `yad daemon` registers, one per first-class
// harness. Held here so that what the document advertises for a harness and
// the adapter that drives its runs cannot come apart.
func Adapters() []adapter.Adapter {
	return []adapter.Adapter{claude.Adapter{}, codex.Adapter{}, opencode.Adapter{}}
}

func adapterFor(id string) (adapter.Adapter, bool) {
	for _, a := range Adapters() {
		if a.Harness() == id {
			return a, true
		}
	}
	return nil, false
}

// HarnessFeatures is the per-run features a run on harness id may use: what
// its adapter does, and nothing for a harness with no adapter, which takes no
// run at all. Interrupt is every adapter's — a harness is first-class only
// once its turn can be ended without ending its session (AGENTS.md).
func HarnessFeatures(id string) []string {
	a, ok := adapterFor(id)
	if !ok {
		return nil
	}
	out := []string{}
	if adapter.Steers(a) {
		out = append(out, FeatureSteer)
	}
	out = append(out, FeatureInterrupt)
	if adapter.AppliesEffort(a) {
		out = append(out, FeatureEffort)
	}
	if adapter.Forks(a) {
		out = append(out, FeatureFork)
	}
	return out
}

// everyDrivable is whether there is a harness in reports that can take a run
// on this machine, and every one lists feature. Drivable's rule decides which
// count, so a first-class harness that is not present, or whose probe
// failed, takes nothing from the rest. It reads each report's own list, so
// the runner-wide string and the lists in one document cannot disagree.
func everyDrivable(reports []v1.HarnessReport, feature string) bool {
	found := false
	for _, h := range reports {
		if !drivable(h) {
			continue
		}
		if !slices.Contains(h.Features, feature) {
			return false
		}
		found = true
	}
	return found
}

// RunMayUse is whether a run on harness id may use feature on the runner doc
// describes: the harness's own list when the runner advertises
// FeatureHarnessFeatures, and the runner-wide strings when it does not — a
// runner older than the lists, whose strings meant every harness. A hub
// checks it before offering a gated run or sending a gated control; a harness
// the document does not list may use nothing. A feature that is not a
// per-run one is the runner's, and answered by its own string alone.
func RunMayUse(doc v1.Capabilities, id, feature string) bool {
	if !slices.Contains(RunFeatures(), feature) || !slices.Contains(doc.ProtocolFeatures, FeatureHarnessFeatures) {
		return slices.Contains(doc.ProtocolFeatures, feature)
	}
	for _, h := range doc.Harnesses {
		if h.ID == id {
			return slices.Contains(h.Features, feature)
		}
	}
	return false
}
