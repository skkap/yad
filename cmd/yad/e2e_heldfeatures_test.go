package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
)

// The case DEV-169 closes, end to end: OpenCode's binary goes while a run on
// it is held, so OpenCode is no longer drivable and Claude is the only harness
// that is. Counted over the drivable harnesses alone, the runner-wide strings
// would say steer again, and a hub reading only them would steer the OpenCode
// run, which takes none. While the run is held they do not; once it has
// ended, the next document follows the drivable harnesses alone and lists
// steer. OpenCode's own list says the same throughout.
func TestE2EAHarnessGoneMidRunKeepsItsFeaturesRunnerWideUntilTheRunEnds(t *testing.T) {
	m := newMachine(t, opencodeE2E)
	claudeE2E.install(t)
	opencode := os.Getenv("YAD_OPENCODE_PATH")
	opencodePlays(t, "plain")
	m.gated()
	m.ok(m.submitArgs("--run-id", "oc-held", "--new-session", "oc-held", "Reply with exactly: pong")...)
	// Short, so that the binary going is in a document soon after.
	d := m.daemon("--interval", "200ms")
	m.waitAtGate()

	hubHas := func(what string, cond func(v1.Capabilities) bool) v1.Capabilities {
		t.Helper()
		var doc v1.Capabilities
		eventually(t, what, func() bool {
			r, err := m.hubDB.GetRunner(context.Background(), m.runnerID())
			if err != nil || json.Unmarshal([]byte(r.Capabilities), &doc) != nil {
				return false
			}
			return cond(doc)
		})
		return doc
	}
	steer := func(doc v1.Capabilities) bool { return slices.Contains(doc.ProtocolFeatures, capability.FeatureSteer) }
	both := hubHas("the hub holds a document with claude and opencode drivable", func(doc v1.Capabilities) bool {
		return capability.Drivable(doc, "claude") && capability.Drivable(doc, "opencode")
	})
	if steer(both) {
		t.Fatalf("steer runner-wide with opencode drivable: %v", both.ProtocolFeatures)
	}

	if err := os.Remove(opencode); err != nil {
		t.Fatal(err)
	}
	gone := hubHas("the hub holds a document in which opencode is not drivable", func(doc v1.Capabilities) bool {
		return capability.Drivable(doc, "claude") && !capability.Drivable(doc, "opencode")
	})
	if steer(gone) {
		t.Errorf("steer runner-wide while a run is held on opencode, which is no longer drivable: %v\ndaemon:\n%s", gone.ProtocolFeatures, d.out.String())
	}
	for _, f := range capability.HarnessFeatures("opencode") {
		if !slices.Contains(gone.ProtocolFeatures, f) {
			t.Errorf("runner-wide %v lacks %q, which claude and opencode both take", gone.ProtocolFeatures, f)
		}
	}
	for _, h := range gone.Harnesses {
		if h.ID == "opencode" && !slices.Equal(h.Features, capability.HarnessFeatures("opencode")) {
			t.Errorf("opencode's own list %v moved with its run, want %v", h.Features, capability.HarnessFeatures("opencode"))
		}
	}

	m.open()
	if code, out, errs := m.watch("oc-held"); code != 0 || !strings.Contains(out, "── succeeded in") {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	hubHas("the hub holds a document listing steer runner-wide, now that no run is held on opencode", steer)
}
