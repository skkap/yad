package v1

import "time"

// A source probe (decision 0088) is a hub asking a runner whether it can
// reach a repository, and the runner answering with its own credentials: the
// hub lists the repositories it may send the runner in probe_sources, the
// runner checks each with git ls-remote as a run's git would, and reports
// source_reach. host_tools says a gh is logged in to github.com; it cannot say
// that its account sees a repository in another organisation, or that its
// token still works. Everything here goes only to and from a runner
// advertising the "source_probe" feature.

// ProbeSourcesMax is the most repositories a runner probes for one hub. A hub
// lists the ones it most wants answered first; a runner probes the first this
// many distinct URLs and leaves the rest unanswered.
const ProbeSourcesMax = 64

// ProbeSource is one repository a hub asks a runner to check it can reach.
type ProbeSource struct {
	URL string `json:"url" doc:"The repository, as a git source's url names it: an https or ssh URL, or an absolute path or file:// URL on the runner's machine. Never with a credential in it — an https URL with userinfo, or an ssh URL with a password — nor with a query or a fragment, which the runner refuses unprobed and answers with error refused. Each URL at most once."`
	Ref string `json:"ref,omitempty" doc:"The ref to ask the remote for, as git ls-remote matches a pattern: a branch, a tag or a full ref name. Absent: HEAD, the repository's default branch."`
}

// SourceReach is a runner's answer about one repository a hub listed in
// probe_sources.
type SourceReach struct {
	URL       string           `json:"url" doc:"The repository, exactly as the hub listed it in probe_sources."`
	Reachable bool             `json:"reachable" doc:"Whether git ls-remote, run on this runner with the environment and credentials a run's git uses, found the ref at the URL."`
	CheckedAt time.Time        `json:"checked_at" doc:"When the runner checked. A runner checks a reachable repository again at least every 30 minutes and an unreachable one at least every 5, and at once when a hub first lists it."`
	Error     SourceReachError `json:"error,omitempty" enum:"auth,not_found,ref_not_found,network,timeout,refused,failed" doc:"Why reachable is false, as a class — never git's own words, which can quote a URL holding a credential. auth: the remote refused or asked for a credential this runner does not have. not_found: no repository at the URL; GitHub answers this way too for a private repository the credential cannot see. ref_not_found: the repository answered, and has no such ref — every runner will say the same, so it is no reason to route elsewhere. network: the host could not be resolved or reached. timeout: git did not finish in 20 seconds. refused: the runner would not probe the URL — it carries a credential, a query or a fragment, is not a URL a git source may name, or is on the machine where its owner does not allow sources. failed: git failed some other way, or could not be started. Absent when reachable is true. A closed set for all of v1: a value outside it goes only to a hub that advertised, in hub_features, the feature adding it."`
}

// SourceReachError is why a runner could not reach a repository, as a class.
type SourceReachError string

const (
	ReachAuth        SourceReachError = "auth"
	ReachNotFound    SourceReachError = "not_found"
	ReachRefNotFound SourceReachError = "ref_not_found"
	ReachNetwork     SourceReachError = "network"
	ReachTimeout     SourceReachError = "timeout"
	ReachRefused     SourceReachError = "refused"
	ReachFailed      SourceReachError = "failed"
)

// SourceReachErrors lists the closed set, for the parity test.
func SourceReachErrors() []SourceReachError {
	return []SourceReachError{ReachAuth, ReachNotFound, ReachRefNotFound, ReachNetwork, ReachTimeout, ReachRefused, ReachFailed}
}

// RoutesAway reports whether an answer is a reason to offer a run needing the
// repository to another runner: unreachable for any reason but a missing ref,
// which is the repository's and not the runner's.
func (r SourceReach) RoutesAway() bool {
	return !r.Reachable && r.Error != ReachRefNotFound
}
