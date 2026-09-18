// Package capability builds the document a runner sends to a control plane to
// say what it is and what it can do.
//
// The document is the contract. A control plane that has never heard of YAD
// should be able to read one of these and decide whether to send it work.
package capability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"time"

	"github.com/skkap/yad/internal/agents"
	"github.com/skkap/yad/internal/buildinfo"
)

// Report is what a runner advertises. Everything in it is derived from the
// machine, so two runners with the same report are interchangeable for routing.
type Report struct {
	RunnerID   string            `json:"runner_id"`
	Name       string            `json:"name"`
	YadVersion string            `json:"yad_version"`
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	Labels     []string          `json:"labels,omitempty"`
	Agents     []agents.Detected `json:"agents"`
	MaxRuns    int               `json:"max_concurrent_runs"`
	ObservedAt time.Time         `json:"observed_at"`
}

// Build probes the machine and assembles the report.
func Build(ctx context.Context, runnerID, name string, maxRuns int, labels []string) Report {
	goos, goarch := agents.Platform()
	if name == "" {
		name, _ = os.Hostname()
	}
	return Report{
		RunnerID:   runnerID,
		Name:       name,
		YadVersion: buildinfo.Version,
		OS:         goos,
		Arch:       goarch,
		Labels:     labels,
		Agents:     agents.Detect(ctx),
		MaxRuns:    maxRuns,
		ObservedAt: time.Now().UTC(),
	}
}

// Fingerprint is a hash of everything in the report except the timestamp.
//
// Heartbeats carry the fingerprint rather than the whole document: the control
// plane asks for a full report only when the hash moves, which is what keeps a
// 15-second heartbeat from re-sending a kilobyte of unchanged agent list all
// day.
func (r Report) Fingerprint() string {
	c := r
	c.ObservedAt = time.Time{}
	b, err := json.Marshal(c)
	if err != nil {
		// Marshalling a struct of strings and slices cannot fail; if it ever
		// does, a fingerprint that never matches is the safe answer, because it
		// forces a full re-report rather than hiding a change.
		return "unfingerprintable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
