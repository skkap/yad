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
	"slices"
	"sort"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter/claude"
	"github.com/skkap/yad/internal/adapter/codex"
	"github.com/skkap/yad/internal/adapter/opencode"
	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/hostool"
	"github.com/skkap/yad/internal/probe"
)

// Features is what this runner supports beyond the v1 baseline, given the
// harness reports of the document it goes in. A hub must not use a feature the
// runner did not advertise, so "live_sessions" is absent until it is built.
//
// A per-run feature — one a run's harness decides, RunFeatures — is listed
// here only while every harness this machine can drive (Drivable: first-class,
// installed, its probe passed) lists it: a hub that reads only these strings
// must never be told a run may use one its harness cannot. It is over what is
// installed, not the build's catalog, so that a harness this machine does not
// have — OpenCode, which takes no steer — takes nothing from those it does.
// Installing or removing one changes the reports, so the document and its
// fingerprint move together and a hub re-reads both. With no harness to drive
// none is listed: there is no run for one to apply to, and a hub reading only
// these strings then sees a feature appear when the first harness is installed
// rather than vanish when it is one that lacks it. Each harness's own list,
// beside FeatureHarnessFeatures, is the whole answer for a hub that reads it
// (decision 0069).
func Features(reports []v1.HarnessReport) []string {
	out := []string{FeatureStartAt}
	for _, f := range RunFeatures() {
		if everyDrivable(reports, f) {
			out = append(out, f)
		}
	}
	return append(out, FeatureDrain, FeatureCloseSession, FeatureLogin, FeatureHarnessFeatures)
}

// FeatureStartAt is a runner that holds a run until its start_at rather than
// beginning it at once. A hub offers a run carrying one only to such a runner.
const FeatureStartAt = "start_at"

// FeatureSteer is a runner that hands a steer control's text to the running
// harness; FeatureInterrupt, one that ends a turn without ending the session.
// A hub sends neither control to a runner that does not advertise it: nothing
// acknowledges a control, so one that is ignored looks exactly like one that
// landed. Both are per-run features, like effort (decision 0069).
const (
	FeatureSteer     = "steer"
	FeatureInterrupt = "interrupt"
)

// FeatureDrain is a runner that acts on the drain control and reports
// draining in its health (decision 0029).
const FeatureDrain = "drain"

// FeatureCloseSession is a runner that acts on the close_session control and
// reports every session it closes — by the hub's word, its owner's, the idle
// TTL or disk pressure — in its syncs' closed_sessions (decision 0035).
const FeatureCloseSession = "close_session"

// FeatureEffort is a runner that hands a run's effort to its harness. A hub
// offers a run carrying one only to such a runner: any other would drop the
// field it does not know and run the harness at its default, and the run
// would succeed saying nothing of it (decision 0049). A per-run feature:
// listed for each harness whose adapter applies it, and runner-wide only
// while every harness this machine can drive does (decision 0069).
const FeatureEffort = "effort"

// FeatureFork is a runner that opens a session as a fork of another it holds
// (session.fork_from): the fork's conversation starts from a copy of the
// other's, which goes on untouched — Claude's --fork-session, Codex's
// thread/fork (decision 0065). A hub offers a run carrying fork_from only to
// such a runner, and it is always the one holding the session forked, so a
// fork waits on that runner rather than going elsewhere. A per-run feature,
// like effort (decision 0069). Claude's flags probe asks for --fork-session,
// so a Claude without it is driven not at all, and the pinned Codex protocol
// has thread/fork. A Codex whose protocol drifted from the pinned one is still driven, with a
// warning, for forks as for every other method it may have changed (decision
// 0037); a fork it cannot do fails with Codex's own refusal.
const FeatureFork = "fork"

// FeatureLogin is a runner that acts on the hub-login controls —
// start_login, login_code, login_token and cancel_login — and reports each
// login in its syncs' logins (decision 0055). Advertised whatever harnesses
// are installed: a login this runner cannot do ends failed with the reason,
// which a hub can show, where a control it ignored would say nothing.
const FeatureLogin = "login"

// FeatureAccounts is a runner that lets this hub add accounts — start_login
// and login_token carrying add — and remove them with remove_account
// (decision 0057). Unlike the rest it is not in Features: whether a hub may is
// the owner's per connection (manage_accounts), so ForConnection adds it to
// the document that connection's hub is sent.
const FeatureAccounts = "accounts"

// ForConnection is the document as one connection's hub is sent it: doc, with
// FeatureAccounts for a hub its owner lets add and remove accounts, and only
// beside FeatureLogin, since an add is a login. doc itself, which every other
// hub is sent too, is left as it was.
func ForConnection(doc v1.Capabilities, manageAccounts bool) v1.Capabilities {
	if manageAccounts && slices.Contains(doc.ProtocolFeatures, FeatureLogin) {
		doc.ProtocolFeatures = append(slices.Clone(doc.ProtocolFeatures), FeatureAccounts)
	}
	return doc
}

// Build probes the machine and assembles the document from it and the owner's
// config.
//
// accounts is the runner's accounts with their states, or nil when there are
// none to report — a harness with no accounts runs on its own default home and
// says so by reporting none, which is a state and not a failure.
func Build(ctx context.Context, runnerID string, cfg config.Config, accounts []account.Account) v1.Capabilities {
	// The two probes run side by side because the daemon re-probes on a fixed
	// interval: one machine where every harness and every host tool hangs must
	// still finish within it, and in series it would not.
	var tools []hostool.Detected
	done := make(chan struct{})
	go func() {
		defer close(done)
		tools = hostool.Detect(ctx)
	}()
	found := Detect(ctx)
	DefaultLogins(ctx, found, cfg)
	<-done
	addModels(ctx, found, cfg, accounts)

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
	reports := Harnesses(found, cfg, accounts)
	return v1.Capabilities{
		RunnerID:         runnerID,
		Name:             name,
		YadVersion:       buildinfo.Version,
		OS:               goos,
		Arch:             goarch,
		Labels:           sortedCopy(cfg.Labels),
		Harnesses:        reports,
		HostTools:        HostTools(tools),
		Capacity:         caps,
		ProtocolFeatures: Features(reports),
		PathSources:      PathSources(cfg.Workdirs),
		ObservedAt:       time.Now().UTC(),
	}
}

// PathSources is the document's path_sources: false when the owner has
// switched sources on the machine off, so a hub offers this runner no run it
// would refuse (decision 0062), and absent otherwise. Never true: absent
// already means it, and a runner older than the field says the same.
func PathSources(w config.WorkdirsConfig) *bool {
	if w.AllowsPathSources() {
		return nil
	}
	return new(false)
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
		// OpenCode cannot print the ACP surface it speaks, so the release
		// is the pin (decision 0073).
		if d.ID == "opencode" && d.Ready() {
			if w := opencode.VersionWarning(d.Version); w != "" {
				found[i].Warnings = append(found[i].Warnings, w)
			}
		}
		// A Claude without a flag every run passes would fail each run at
		// its arguments, so it is reported unable to take any rather than
		// claim them (AGENTS.md, decision 0050).
		if d.ID == "claude" && d.Ready() {
			e, w := claude.FlagsCheck(ctx, probe.Find(d.EnvPath, d.Binary, d.VersionArgs), d.Version)
			if e != "" {
				found[i].Error = e
			}
			if w != "" {
				found[i].Warnings = append(found[i].Warnings, w)
			}
		}
	}
	return found
}

// Harnesses turns detection into the public report, with the owner's accounts
// by label and state. Only the fields a hub may see survive the translation:
// an account's home, and everything the harness wrote inside it, do not.
//
// An account the store has never seen is reported free when its home is on
// disk and needs_login when it is not (account.Load owns that rule), so no
// store write is needed to report an account correctly. The nil-accounts
// fallback below cannot check either and reports the configured labels free.
func Harnesses(found []harness.Detected, cfg config.Config, accounts []account.Account) []v1.HarnessReport {
	out := make([]v1.HarnessReport, 0, len(found))
	for _, d := range found {
		r := v1.HarnessReport{
			ID: d.ID, Label: d.Label, Kind: string(d.Kind),
			Present: d.Present, Version: d.Version, Error: d.Error, Models: d.Models,
			ModelsSource: d.ModelsSource, Warnings: d.Warnings,
		}
		if d.Kind == harness.FirstClass {
			r.Features = HarnessFeatures(d.ID)
		}
		if len(r.Models) > 0 && r.ModelsSource == "" {
			// Detection alone, with no model probe: the catalog's list.
			r.ModelsSource = v1.ModelsFromCatalog
		}
		if reps := account.Reports(accounts, d.ID); reps != nil {
			for i := range reps {
				reps[i].Models = d.AccountModels[reps[i].Label]
			}
			r.Accounts = reps
			// A setup-token lasts a year and nothing else says when it runs
			// out, so the month before is said here (decision 0054).
			for _, a := range account.For(accounts, d.ID) {
				if w := account.TokenWarning(a.Label, a.Home, time.Now()); w != "" {
					r.Warnings = append(r.Warnings, w)
				}
			}
		} else {
			for _, a := range cfg.Harness[d.ID].Accounts {
				r.Accounts = append(r.Accounts, v1.AccountReport{Label: a, State: v1.AccountFree})
			}
		}
		out = append(out, r)
	}
	return out
}

// HostTools turns host-tool detection into the public report. Only what a hub
// may route on survives: never a path, and never who a tool is logged in as.
func HostTools(found []hostool.Detected) []v1.HostTool {
	out := make([]v1.HostTool, 0, len(found))
	for _, d := range found {
		out = append(out, v1.HostTool{
			ID: d.ID, Present: d.Present, Version: d.Version,
			LoggedIn: d.LoggedIn, LoginHosts: d.LoginHosts, Error: d.Error,
			Warnings: d.Warnings,
		})
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
			return drivable(h)
		}
	}
	return false
}

func drivable(h v1.HarnessReport) bool {
	return h.Kind == string(harness.FirstClass) && h.Present && h.Error == ""
}
