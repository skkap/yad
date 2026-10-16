package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// probeWindow is how far back the repositories this hub asks runners about
// go: those of the runs submitted in the last week, which are the ones its
// next runs are likely to name again.
const probeWindow = 7 * 24 * time.Hour

// probeList is what this hub lists in probe_sources (decision 0087): the git
// URLs its recent runs named, the most recently used first, up to the most a
// runner probes. A URL with a credential in it is left out: the run carrying
// it brings its own way in, and a runner would refuse to probe it.
func probeList(ctx context.Context, q *db.Queries, now time.Time) ([]v1.ProbeSource, error) {
	rows, err := q.ProbeSourceCandidates(ctx, db.ProbeSourceCandidatesParams{
		Since: store.Ms(now.Add(-probeWindow)), Max: 2 * v1.ProbeSourcesMax,
	})
	if err != nil {
		return nil, err
	}
	var out []v1.ProbeSource
	for _, r := range rows {
		if r.Url == "" || carriesCredential(r.Url) {
			continue
		}
		out = append(out, v1.ProbeSource{URL: r.Url})
		if len(out) == v1.ProbeSourcesMax {
			break
		}
	}
	return out, nil
}

// syncSourceReach takes a sync's source_reach: the latest answer for each URL
// listed, kept per runner, and nothing for a URL this hub no longer lists,
// whose answers it forgets.
func syncSourceReach(ctx context.Context, q *db.Queries, runnerID string, listed []v1.ProbeSource, reach []v1.SourceReach, now time.Time) error {
	urls := make([]string, 0, len(listed))
	in := map[string]bool{}
	for _, p := range listed {
		urls = append(urls, p.URL)
		in[p.URL] = true
	}
	b, err := json.Marshal(urls)
	if err != nil {
		return err
	}
	if err := q.ForgetSourceReach(ctx, db.ForgetSourceReachParams{RunnerID: runnerID, UrlsJson: string(b)}); err != nil {
		return err
	}
	for _, r := range reach {
		if !in[r.URL] {
			continue
		}
		checked := r.CheckedAt
		if checked.IsZero() {
			checked = now
		}
		errClass := string(r.Error)
		if r.Reachable {
			errClass = ""
		}
		if err := q.RecordSourceReach(ctx, db.RecordSourceReachParams{
			RunnerID: runnerID, Url: r.URL, Reachable: boolInt(r.Reachable), Error: errClass,
			CheckedAt: store.Ms(checked), ReceivedAt: store.Ms(now),
		}); err != nil {
			return err
		}
	}
	return nil
}

// unreachable is the URLs a runner last said it cannot reach, for a reason
// that is the runner's (SourceReach.RoutesAway): a run opening a session on
// one goes to another runner.
func unreachable(ctx context.Context, q *db.Queries, runnerID string) (map[string]bool, error) {
	rows, err := q.SourceReachOf(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if (v1.SourceReach{Reachable: r.Reachable != 0, Error: v1.SourceReachError(r.Error)}).RoutesAway() {
			out[r.Url] = true
		}
	}
	return out, nil
}

// sourceReachOf is a runner's answers, for the service API.
func sourceReachOf(ctx context.Context, q *db.Queries, runnerID string) ([]v1.SourceReach, error) {
	rows, err := q.SourceReachOf(ctx, runnerID)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	var out []v1.SourceReach
	for _, r := range rows {
		out = append(out, v1.SourceReach{URL: r.Url, Reachable: r.Reachable != 0, Error: v1.SourceReachError(r.Error),
			CheckedAt: time.UnixMilli(r.CheckedAt).UTC()})
	}
	return out, nil
}

// carriesCredential is a git URL with a credential in its userinfo: any
// userinfo in an https URL, where a token is the user, and a password in an
// ssh one, whose user is only the login name. It reads the URL as the
// runner's parseRemote does, cut from the string rather than parsed.
func carriesCredential(raw string) bool {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return false
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	at := strings.LastIndex(rest[:end], "@")
	if at < 0 {
		return false
	}
	switch strings.ToLower(scheme) {
	case "ssh", "git+ssh", "ssh+git":
		return strings.Contains(rest[:at], ":")
	}
	return true
}
