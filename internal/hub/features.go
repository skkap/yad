package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/hub/store/db"
)

// storedDoc is a runner's capability document as the hub last received it —
// the hub's only record of what that runner will act on.
func storedDoc(r db.Runner) (v1.Capabilities, error) {
	var doc v1.Capabilities
	if err := json.Unmarshal([]byte(r.Capabilities), &doc); err != nil {
		return doc, fmt.Errorf("stored capability document for %s: %w", r.ID, err)
	}
	return doc, nil
}

// advertises reports whether a runner's current capability document says it
// acts on a feature. The document a sync carries is the only current answer:
// a control queued while the runner advertised more than it does now — a
// downgrade, a rebuild without a feature — would be ignored, and nothing
// acknowledges a control, so the hub holds it back rather than spending it.
func advertises(doc v1.Capabilities, feature string) bool {
	return slices.Contains(doc.ProtocolFeatures, feature)
}

// refuseUnadvertised is the answer to a control a runner never said it would
// act on. Nothing may use a feature the other side did not advertise
// (ARCHITECTURE.md §2), and a control is not acknowledged: one sent to a
// runner that ignores it would look to the caller exactly like one that
// landed, and the run would go on running.
//
// harness is the one the control is about. A per-run feature is asked of it
// (decision 0069), and one its runner advertises for other harnesses and not
// this one is not an upgrade away, so the answer offers only the alternative.
func refuseUnadvertised(r db.Runner, harness string, kind v1.ControlKind, feature, alternative string) error {
	doc, err := storedDoc(r)
	if err != nil {
		return err
	}
	if capability.RunMayUse(doc, harness, feature) {
		return nil
	}
	if slices.Contains(capability.RunFeatures(), feature) && advertises(doc, capability.FeatureHarnessFeatures) {
		return Fail(http.StatusConflict, v1.CodeConflict,
			fmt.Sprintf("runner %s does not advertise the %q feature for harness %q; it would ignore the %s", r.ID, feature, harness, kind),
			strings.TrimPrefix(alternative, ", or "))
	}
	return Fail(http.StatusConflict, v1.CodeConflict,
		fmt.Sprintf("runner %s does not advertise the %q feature; it would ignore the %s", r.ID, feature, kind),
		"upgrade yad on that runner"+alternative)
}

// controlFeature is the feature a control kind needs of a runner, or "" for
// the ones every v1 runner acts on: cancel, and report_capabilities. drain and
// close_session are gated where they are asked for, against the runner or the
// session named there.
func controlFeature(kind v1.ControlKind, runID string) (feature, alternative string) {
	switch kind {
	case v1.ControlSteer:
		return capability.FeatureSteer, ", or put the text in the brief of a new run"
	case v1.ControlInterrupt:
		return capability.FeatureInterrupt, ", or cancel the run instead: `" + serviceCommand("cancel", runID) + "`"
	}
	return "", ""
}
