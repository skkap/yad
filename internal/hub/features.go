package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

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

// refuseUnadvertised is the answer to a control a runner never said it would
// act on. Nothing may use a feature the other side did not advertise
// (ARCHITECTURE.md §2), and a control is not acknowledged: one sent to a
// runner that ignores it would look to the caller exactly like one that
// landed, and the run would go on running.
func refuseUnadvertised(r db.Runner, kind v1.ControlKind, feature, alternative string) error {
	doc, err := storedDoc(r)
	if err != nil {
		return err
	}
	if slices.Contains(doc.ProtocolFeatures, feature) {
		return nil
	}
	return Fail(http.StatusConflict, v1.CodeConflict,
		fmt.Sprintf("runner %s does not advertise the %q feature; it would ignore the %s", r.ID, feature, kind),
		"upgrade yad on that runner"+alternative)
}

// controlFeature is the feature a control kind needs of a runner, or "" for
// the ones every v1 runner acts on: cancel, and report_capabilities. drain and
// close_session are gated where they are asked for, against the runner or the
// session named there.
func controlFeature(kind v1.ControlKind) (feature, alternative string) {
	switch kind {
	case v1.ControlSteer:
		return capability.FeatureSteer, ", or put the text in the brief of a new run"
	case v1.ControlInterrupt:
		return capability.FeatureInterrupt, ", or cancel the run instead: `yad hub cancel`"
	}
	return "", ""
}
