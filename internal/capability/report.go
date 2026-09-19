// Package capability builds the document a runner sends a hub to say what it is
// and what it can do.
//
// The document is the contract. A hub that has never heard of YAD should be
// able to read one of these and decide whether to send it work.
package capability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter/codex"
	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
)

// Features is what this build of the runner supports beyond the v1 baseline.
// A hub must not use a feature the runner did not advertise, so "live_sessions"
// is absent until it is built.
func Features() []string {
	return []string{"start_at", "steer", "interrupt", FeatureDrain}
}

// FeatureDrain is a runner that acts on the drain control and reports
// draining in its health (decision 0029).
const FeatureDrain = "drain"

// Build probes the machine and assembles the document from it and the owner's
// config. Accounts and host tools are filled in by their packages as they land.
func Build(ctx context.Context, runnerID string, cfg config.Config) v1.Capabilities {
	goos, goarch := harness.Platform()
	name := cfg.Name
	if name == "" {
		name, _ = os.Hostname()
	}
	caps := v1.Capacity{Total: cfg.Capacity}
	for id, h := range cfg.Harness {
		if h.Cap > 0 {
			if caps.ByHarness == nil {
				caps.ByHarness = map[string]int{}
			}
			caps.ByHarness[id] = h.Cap
		}
	}
	return v1.Capabilities{
		RunnerID:         runnerID,
		Name:             name,
		YadVersion:       buildinfo.Version,
		OS:               goos,
		Arch:             goarch,
		Labels:           sortedCopy(cfg.Labels),
		Harnesses:        Harnesses(Detect(ctx), cfg),
		Capacity:         caps,
		ProtocolFeatures: Features(),
		ObservedAt:       time.Now().UTC(),
	}
}

// Detect is harness.Detect with the checks an adapter adds for its harness:
// for Codex, whether the installed app-server protocol is one the adapter was
// built against (decision 0037). A check never fails detection; what it finds
// is a warning.
func Detect(ctx context.Context) []harness.Detected {
	found := harness.Detect(ctx)
	for i, d := range found {
		if d.ID == "codex" && d.Ready() {
			if w := codex.SchemaWarning(ctx, d.Path, d.Version); w != "" {
				found[i].Warnings = append(found[i].Warnings, w)
			}
		}
	}
	return found
}

// Harnesses turns detection into the public report, with the owner's accounts
// by label. Only the fields a hub may see survive the translation.
func Harnesses(found []harness.Detected, cfg config.Config) []v1.HarnessReport {
	out := make([]v1.HarnessReport, 0, len(found))
	for _, d := range found {
		r := v1.HarnessReport{
			ID: d.ID, Label: d.Label, Kind: string(d.Kind),
			Present: d.Present, Version: d.Version, Error: d.Error, Models: d.Models,
			Warnings: d.Warnings,
		}
		for _, a := range cfg.Harness[d.ID].Accounts {
			r.Accounts = append(r.Accounts, v1.AccountReport{Label: a})
		}
		out = append(out, r)
	}
	return out
}

// Fingerprint is a hash of everything in the document except the timestamp.
//
// Syncs carry the fingerprint rather than the document: the hub asks for a full
// one only when the hash moves, which keeps a 15-second sync from re-sending a
// kilobyte of unchanged harness list all day.
func Fingerprint(c v1.Capabilities) string {
	c.ObservedAt = time.Time{}
	b, err := json.Marshal(c)
	if err != nil {
		// Marshalling strings, slices and maps cannot fail; if it ever does, a
		// fingerprint that never matches is the safe answer — it forces a full
		// re-report rather than hiding a change.
		return "unfingerprintable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func sortedCopy(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

// Drivable is DOMAIN.md's rule for accepting a run, applied to a document: the
// harness is first-class, present, and passed its version probe. A hub checks
// it before offering and a runner again before claiming, because a hub's copy
// of the document may be stale — or the hub may not check at all.
func Drivable(doc v1.Capabilities, harnessID string) bool {
	for _, h := range doc.Harnesses {
		if h.ID == harnessID {
			return h.Kind == string(harness.FirstClass) && h.Present && h.Error == ""
		}
	}
	return false
}
