package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/upgrade"
)

// A check runs on the daemon's own context, which has no deadline, against
// the real release source. A release server — or a proxy in front of it —
// that takes the request and never answers, or stops partway through a
// download, must not stop the schedule: the check gives up at its bound,
// counts as a failure, and the next check comes as it would have (YAD-181).
// The source's own stall is set beyond the test so that the check's bound is
// the only thing that can end it.
func TestACheckAgainstAServerThatStopsAnsweringEnds(t *testing.T) {
	const (
		latest   = "/skkap/yad/releases/latest"
		download = "/skkap/yad/releases/download/v0.9.0/" + asset
	)
	for _, tc := range []struct {
		name string
		// stop is the path the server stops answering on; the others answer
		// as GitHub does.
		stop string
		// partway sends the headers and part of the body before stopping.
		partway bool
	}{
		{"the newest release is asked for", latest, false},
		{"the release is downloaded", download, false},
		{"the download stops partway", download, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.stop {
					if tc.partway {
						w.WriteHeader(http.StatusOK)
						w.Write([]byte("the first piece of a binary"))
						w.(http.Flusher).Flush()
					}
					select {
					case <-release:
					case <-r.Context().Done():
					}
					return
				}
				switch r.URL.Path {
				case latest:
					http.Redirect(w, r, "/skkap/yad/releases/tag/v0.9.0", http.StatusFound)
				default:
					w.WriteHeader(http.StatusOK)
				}
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(func() { close(release) })

			h := newHarness(t)
			o := h.options()
			o.Source = upgrade.GitHub{BaseURL: srv.URL, Stall: time.Hour}
			o.CheckTimeout = 200 * time.Millisecond
			u := New(o)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { u.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()

			h.clock.Advance(h.clock.next(t))
			// The next wait is asked for only once the check has ended; a
			// check nothing bounds never gets here, and next fails the test.
			if d := h.clock.next(t); d < Every-Every/10 || d > Every+Every/10 {
				t.Errorf("after the check that got no answer, the next comes in %s, want six hours and a tenth", d)
			}
			st := u.Status()
			if !strings.Contains(st.LastError, "did not end within 200ms") || st.LastCheck.IsZero() {
				t.Errorf("status %+v, want the check recorded as given up at its bound", st)
			}
			if st.Pending != nil || st.Refused != nil {
				t.Errorf("status %+v, want nothing pending or refused", st)
			}
			if !st.NextCheck.After(h.clock.Now()) {
				t.Errorf("next check at %s, want one after now (%s)", st.NextCheck, h.clock.Now())
			}
			if got := h.installed(t); got != "v0.8.0" {
				t.Errorf("installed binary is %q", got)
			}
			if !strings.Contains(h.log.String(), "level=WARN") {
				t.Errorf("no warning logged:\n%s", h.log)
			}
		})
	}
}
