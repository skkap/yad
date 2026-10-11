package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
	"github.com/skkap/yad/internal/workdir"
)

// Releasing a session (decision 0081): the hub's release_session hands one
// back, so its next run can go to another runner. The session's conversation
// and what a runner needs to carry it on go in a session archive, uploaded to
// the hub; once the hub has it the session closes here as released, its
// workdir and conversation are reclaimed, and the hub hears of it in
// released_sessions. Nothing is released that the next runner could not
// build again: work the remote does not have is refused unless the hub says
// discard_local, and a source on this machine always is.

// Timings of the releases worker.
const (
	// releaseTick is how often the worker looks for a release that is due
	// when nothing wakes it sooner: one waiting on a run held in its
	// session, or on its upload's next attempt.
	releaseTick = 5 * time.Second
	// releasedPerSync bounds the releases one sync reports, as
	// closedPerSync bounds the closes.
	releasedPerSync = 64
)

// ArchiveHub is the part of hubclient.Client that carries session archives.
type ArchiveHub interface {
	PutArchive(ctx context.Context, sessionID, sha string, size int64, body io.Reader) (v1.SessionArchive, error)
	GetArchive(ctx context.Context, sessionID string, limit int64, w io.Writer) (string, int64, error)
}

var _ ArchiveHub = (*hubclient.Client)(nil)

// Releases does every connection's releases: it builds each archive once no
// run is held in the session, uploads it, and settles the release — the
// result outbox's pattern, from the store, so a restart picks up where it
// was. Its sync loop asks for a release and reports how it ended.
type Releases struct {
	Store *store.Store
	// Data is the profile's data directory: archives are built under
	// <data>/releases.
	Data     string
	Adapters *Registry
	// Workdirs reads what a session's workdir holds that its remote does
	// not (workdir.Manager.Local).
	Workdirs *workdir.Manager
	// Accounts says whether a harness has accounts here, and so where its
	// conversations are kept. Nil is none.
	Accounts *Accounts
	// Capabilities is the runner's document, for the harness version a
	// manifest records. Nil records none.
	Capabilities func() v1.Capabilities
	// Sessions is woken once a released session's workdir can go.
	Sessions *Collector
	Log      *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	once sync.Once
	wake chan struct{}
	// sweep makes sweeps one at a time: a release is built and uploaded
	// once.
	sweep sync.Mutex

	mu    sync.Mutex
	hubs  map[string]ArchiveHub
	caps  map[string]int64
	loops map[string]func()
}

func (r *Releases) init() {
	r.once.Do(func() {
		r.wake = make(chan struct{}, 1)
		if r.Log == nil {
			r.Log = slog.New(slog.DiscardHandler)
		}
		if r.Now == nil {
			r.Now = time.Now
		}
	})
}

// Join is one connection's hub, and how to bring its loop's next sync
// forward once a release has an outcome to report.
func (r *Releases) Join(connection string, hub ArchiveHub, wake func()) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hubs == nil {
		r.hubs, r.caps, r.loops = map[string]ArchiveHub{}, map[string]int64{}, map[string]func(){}
	}
	r.hubs[connection], r.loops[connection] = hub, wake
}

// Hub is a connection's archive hub, or nil.
func (r *Releases) Hub(connection string) ArchiveHub {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hubs[connection]
}

// SetCap records the largest archive a connection's hub takes, as its last
// answer said; 0 is none named.
func (r *Releases) SetCap(connection string, n int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.caps == nil {
		r.caps = map[string]int64{}
	}
	r.caps[connection] = n
}

func (r *Releases) capOf(connection string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.caps[connection]
}

func (r *Releases) wakeLoop(connection string) {
	r.mu.Lock()
	wake := r.loops[connection]
	r.mu.Unlock()
	if wake != nil {
		wake()
	}
}

// Wake asks for a sweep now: a release was asked for, or a run ended and
// one may have been waiting on it. Safe on nil.
func (r *Releases) Wake() {
	if r == nil {
		return
	}
	r.init()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run sweeps until ctx ends: at once, whenever woken, and every releaseTick.
func (r *Releases) Run(ctx context.Context) {
	if r == nil {
		return
	}
	r.init()
	for {
		r.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(releaseTick):
		}
	}
}

// Request is a hub's release_session for a session of connection: asked
// for now, or — when the hub asks about one it already heard the end of —
// told again, or begun again after a refusal. One in progress is left to
// go on.
func (r *Releases) Request(ctx context.Context, connection, session string, discard bool) {
	r.init()
	log := r.Log.With("connection", connection, "session", session)
	row, err := r.Store.GetRelease(ctx, db.GetReleaseParams{Connection: connection, SessionID: session})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		sess, err := r.Store.GetSession(ctx, db.GetSessionParams{Connection: connection, ID: session})
		switch {
		case errors.Is(err, sql.ErrNoRows):
			r.Refuse(ctx, connection, session, v1.ReleaseError{Code: v1.ReleaseUnknown,
				Message:    "this runner holds no session " + session + " for this hub",
				NextAction: "nothing to release: send the session's next run as one opening it"})
			return
		case err != nil:
			log.Error("a release was asked for and could not be recorded; the hub asks again at the next sync", "err", err)
			return
		case sess.State != "open":
			r.Refuse(ctx, connection, session, v1.ReleaseError{Code: v1.ReleaseClosed,
				Message:    fmt.Sprintf("session %s was closed on this runner (%s)", session, sess.CloseReason.String),
				NextAction: "nothing to release: start a new session"})
			return
		}
	case err != nil:
		log.Error("a release was asked for and could not be read; the hub asks again at the next sync", "err", err)
		return
	case row.SettledAt.Valid && row.ReportedAt.Valid && !row.Error.Valid:
		// Released, and the hub heard so: it asks again only because the
		// answer that carried the report was lost on its way back.
		if err := r.Store.ReportReleaseAgain(ctx, db.ReportReleaseAgainParams{Connection: connection, SessionID: session}); err != nil {
			log.Error("a release the hub asks about again is not reported again; it asks again at the next sync", "err", err)
		}
		return
	case !(row.SettledAt.Valid && row.ReportedAt.Valid):
		// In progress, or settled and not yet heard of: the hub repeats the
		// control until it hears.
		return
	}
	if err := r.Store.RequestRelease(ctx, db.RequestReleaseParams{
		Connection: connection, SessionID: session, DiscardLocal: boolInt(discard), Now: r.Now().UnixMilli(),
	}); err != nil {
		log.Error("a release was asked for and could not be recorded; the hub asks again at the next sync", "err", err)
		return
	}
	log.Info("the hub asked to release the session; it goes once no run is held in it", "discard_local", discard)
	r.Wake()
}

// Refuse settles a release as refused before anything was done, and has
// its loop report it.
func (r *Releases) Refuse(ctx context.Context, connection, session string, why v1.ReleaseError) {
	r.init()
	body, _ := json.Marshal(why)
	now := r.Now().UnixMilli()
	if err := r.Store.RefuseRelease(ctx, db.RefuseReleaseParams{
		Connection: connection, SessionID: session, Now: now, Error: sql.NullString{String: string(body), Valid: true},
	}); err != nil {
		r.Log.Error("a release refused is not recorded; the hub asks again at the next sync", "connection", connection, "session", session, "err", err)
		return
	}
	r.Log.Warn("refused to release a session", "connection", connection, "session", session, "code", why.Code, "reason", why.Message)
	r.wakeLoop(connection)
}

// Reports is what a connection's next sync reports in released_sessions.
func (r *Releases) Reports(ctx context.Context, connection string) ([]v1.ReleasedSession, error) {
	if r == nil {
		return nil, nil
	}
	r.init()
	rows, err := r.Store.UnreportedReleases(ctx, db.UnreportedReleasesParams{Connection: connection, Max: releasedPerSync})
	if err != nil {
		return nil, err
	}
	var out []v1.ReleasedSession
	for _, row := range rows {
		rs := v1.ReleasedSession{SessionID: row.SessionID, ReleasedAt: msTime(row.SettledAt.Int64)}
		if row.Error.Valid {
			var e v1.ReleaseError
			if err := json.Unmarshal([]byte(row.Error.String), &e); err != nil || e.Code == "" {
				e = v1.ReleaseError{Code: v1.ReleaseExportFailed, Message: "the release was refused, and why could not be read back", NextAction: "release it again"}
			}
			rs.Error = &e
		} else {
			rs.Archive = &v1.SessionArchive{SHA256: row.Sha256.String, Bytes: row.Bytes.Int64}
		}
		out = append(out, rs)
	}
	return out, nil
}

// Reported records that the hub answered a sync carrying these.
func (r *Releases) Reported(ctx context.Context, connection string, rs []v1.ReleasedSession) {
	if r == nil {
		return
	}
	r.init()
	now := sql.NullInt64{Int64: r.Now().UnixMilli(), Valid: true}
	for _, s := range rs {
		if err := r.Store.SetReleaseReported(ctx, db.SetReleaseReportedParams{ReportedAt: now, Connection: connection, SessionID: s.SessionID}); err != nil {
			r.Log.Error("a release the hub heard is not recorded as heard; it is reported again", "connection", connection, "session", s.SessionID, "err", err)
		}
	}
}

// Sweep does every release that is due.
func (r *Releases) Sweep(ctx context.Context) {
	if r == nil {
		return
	}
	r.init()
	r.sweep.Lock()
	defer r.sweep.Unlock()
	rows, err := r.Store.PendingReleases(ctx, r.Now().UnixMilli())
	if err != nil {
		r.Log.Error("releases not read; trying again shortly", "err", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		r.process(ctx, row)
	}
}

// process takes one release as far as it can go now.
func (r *Releases) process(ctx context.Context, row db.Release) {
	log := r.Log.With("connection", row.Connection, "session", row.SessionID)
	sess, err := r.Store.GetSession(ctx, db.GetSessionParams{Connection: row.Connection, ID: row.SessionID})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r.settle(ctx, row, &v1.ReleaseError{Code: v1.ReleaseUnknown, Message: "this runner holds no session " + row.SessionID + " for this hub",
			NextAction: "nothing to release: send the session's next run as one opening it"})
		return
	case err != nil:
		log.Error("a release's session could not be read; trying again shortly", "err", err)
		return
	case sess.State != "open":
		r.settle(ctx, row, &v1.ReleaseError{Code: v1.ReleaseClosed, Message: fmt.Sprintf("session %s was closed on this runner (%s) before it could be released", row.SessionID, sess.CloseReason.String),
			NextAction: "nothing to release: start a new session"})
		return
	}
	held, err := r.Store.HeldRunInSession(ctx, db.HeldRunInSessionParams{Connection: row.Connection, ID: row.SessionID})
	if err != nil {
		log.Error("a release could not tell whether a run is held in its session; trying again shortly", "err", err)
		return
	}
	if held != "" {
		// As a close does: the session goes once its run has ended, and the
		// run's end wakes this worker.
		return
	}
	if !row.Archive.Valid || !regularFile(row.Archive.String) {
		why, err := r.build(ctx, row, sess)
		switch {
		case why != nil:
			r.settle(ctx, row, why)
			return
		case err != nil:
			log.Error("the session's archive could not be built; trying again shortly", "err", err)
			r.retry(ctx, row, err)
			return
		}
		if row, err = r.Store.GetRelease(ctx, db.GetReleaseParams{Connection: row.Connection, SessionID: row.SessionID}); err != nil {
			log.Error("a release could not be read back; trying again shortly", "err", err)
			return
		}
	}
	r.upload(ctx, row, sess)
}

func regularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// build checks the session can be carried and writes its archive. A refusal
// is why it cannot; an error is a fault here, tried again.
func (r *Releases) build(ctx context.Context, row db.Release, sess db.Session) (*v1.ReleaseError, error) {
	ad, ok := r.Adapters.Lookup(sess.Harness)
	rel, isRel := ad.(adapter.Releaser)
	if !ok || !isRel || !rel.Releases() {
		return &v1.ReleaseError{Code: v1.ReleaseNotReleasable, Message: fmt.Sprintf("this runner cannot release a %s session", sess.Harness),
			NextAction: "continue the session on this runner, or start a new session elsewhere"}, nil
	}
	if sess.NativeID.String == "" || sess.Workdir == "" {
		return &v1.ReleaseError{Code: v1.ReleaseNoConversation, Message: fmt.Sprintf("session %s has no conversation on this runner yet: none of its runs has started its harness", sess.ID),
			NextAction: "send the session's next run as one opening it, to any runner — there is nothing to carry"}, nil
	}
	var sources []v1.Source
	if sess.Sources.Valid {
		if err := json.Unmarshal([]byte(sess.Sources.String), &sources); err != nil {
			return &v1.ReleaseError{Code: v1.ReleaseExportFailed, Message: "the session's recorded sources are unreadable on this runner",
				NextAction: "continue the session on this runner, or start a new session"}, nil
		}
	}
	for i, src := range sources {
		if src.Git == nil {
			return &v1.ReleaseError{Code: v1.ReleasePathSource, Message: fmt.Sprintf("sources[%d] is a directory on this runner's machine (%s), which no other runner has", i, src.Path),
				NextAction: "continue the session on this runner; a session on a directory of the machine is never released"}, nil
		}
	}
	carried, err := carriedOf(sess)
	if err != nil {
		return nil, err
	}
	works, err := r.Workdirs.Local(ctx, workdir.Request{
		Dir: sess.Workdir, Connection: row.Connection, Session: sess.ID, Retake: retakeOf(sess.Workdir, sess.ID), Branches: carried,
		Sources: workdir.StoredSources(sources),
	})
	if err != nil {
		// git could not read the worktree: nothing can be said of what is
		// there, so nothing is left behind on a guess.
		r.Log.Warn("the session's workdir could not be read to release it", "connection", row.Connection, "session", sess.ID, "err", err)
		return &v1.ReleaseError{Code: v1.ReleaseExportFailed, Message: "the session's workdir could not be read on this runner to see what it holds",
			NextAction: "its owner sees why in the runner's log; continue the session there, or release it again"}, nil
	}
	if row.DiscardLocal == 0 {
		if why := localWork(works); why != nil {
			return why, nil
		}
	}
	conv := conversationOf(r.Data, r.Accounts.Lists(), sess.Harness, sess.NativeID.String, sess.Workdir, false)
	exp, err := rel.Export(conv)
	if errors.Is(err, adapter.ErrNoConversation) {
		return &v1.ReleaseError{Code: v1.ReleaseNoConversation, Message: fmt.Sprintf("the harness has no conversation for session %s on this runner: its transcript is gone", sess.ID),
			NextAction: "start a new session: there is no conversation to carry"}, nil
	}
	if err != nil {
		r.Log.Warn("the session's conversation could not be read to release it", "connection", row.Connection, "session", sess.ID, "err", err)
		return &v1.ReleaseError{Code: v1.ReleaseExportFailed, Message: "the session's conversation could not be read on this runner",
			NextAction: "its owner sees why in the runner's log; continue the session there, or release it again"}, nil
	}
	m := manifest{Format: archiveFormat, Harness: sess.Harness, NativeID: sess.NativeID.String, Dir: exp.Dir, CreatedAt: r.Now().UTC()}
	if r.Capabilities != nil {
		for _, h := range r.Capabilities().Harnesses {
			if h.ID == sess.Harness {
				m.HarnessVersion = h.Version
			}
		}
	}
	for _, w := range works {
		m.Sources = append(m.Sources, carriedSource{URL: w.URL, Branch: w.Branch})
	}
	file := filepath.Join(r.Data, "releases", pathName(row.Connection), pathName(sess.ID)+".tar.gz")
	sha, size, err := packArchive(file, m, exp.Files)
	if err != nil {
		return nil, err
	}
	if limit := r.capOf(row.Connection); limit > 0 && size > limit {
		removeArchive(file, r.Log)
		return &v1.ReleaseError{Code: v1.ReleaseTooLarge, Message: fmt.Sprintf("the session's archive is %d bytes, past the %d bytes this hub takes", size, limit),
			NextAction: "continue the session on this runner, or raise the hub's archive limit and release it again"}, nil
	}
	if err := r.Store.SetReleaseArchive(ctx, db.SetReleaseArchiveParams{
		Archive: sql.NullString{String: file, Valid: true}, Sha256: sql.NullString{String: sha, Valid: true}, Bytes: sql.NullInt64{Int64: size, Valid: true},
		Connection: row.Connection, SessionID: row.SessionID,
	}); err != nil {
		removeArchive(file, r.Log)
		return nil, err
	}
	r.Log.Info("the session's archive is built", "connection", row.Connection, "session", sess.ID, "bytes", size, "files", len(exp.Files))
	return nil, nil
}

// localWork is the refusal for work the remote does not have, or nil.
func localWork(works []workdir.LocalWork) *v1.ReleaseError {
	for _, w := range works {
		if w.Changed > 0 {
			shown := strings.Join(w.Changes, ", ")
			if w.Changed > len(w.Changes) {
				shown += fmt.Sprintf(" and %d more", w.Changed-len(w.Changes))
			}
			return &v1.ReleaseError{Code: v1.ReleaseLocalChanges,
				Message:    fmt.Sprintf("the session's worktree of %s holds changes not committed (%s), which the next runner would not have", w.Shown, shown),
				NextAction: "send a run in the session that commits and pushes its work, then release it again — or release it with discard_local to leave those changes behind"}
		}
	}
	for _, w := range works {
		if w.Unpushed > 0 {
			return &v1.ReleaseError{Code: v1.ReleaseUnpushed,
				Message:    fmt.Sprintf("branch %s of %s holds %d commit(s) its remote does not, which the next runner would not have", w.Branch, w.Shown, w.Unpushed),
				NextAction: "send a run in the session that pushes the branch, then release it again — or release it with discard_local to leave those commits behind"}
		}
	}
	return nil
}

// upload sends the archive and settles the release on the hub's answer.
func (r *Releases) upload(ctx context.Context, row db.Release, sess db.Session) {
	log := r.Log.With("connection", row.Connection, "session", row.SessionID)
	hub := r.Hub(row.Connection)
	if hub == nil {
		// No loop is syncing this connection, so no hub to send it to: it
		// waits, as a result owed does, for the next start.
		return
	}
	f, err := os.Open(row.Archive.String)
	if err != nil {
		log.Warn("the session's archive is gone before its upload; building it again", "err", err)
		r.forgetArchive(ctx, row)
		return
	}
	_, err = hub.PutArchive(ctx, row.SessionID, row.Sha256.String, row.Bytes.Int64, f)
	f.Close()
	var se *hubclient.StatusError
	switch {
	case err == nil:
		r.released(ctx, row, sess)
	case errors.As(err, &se) && se.Status == http.StatusRequestEntityTooLarge:
		r.settle(ctx, row, &v1.ReleaseError{Code: v1.ReleaseTooLarge, Message: fmt.Sprintf("the hub refused the session's archive of %d bytes as too large", row.Bytes.Int64),
			NextAction: "continue the session on this runner, or raise the hub's archive limit and release it again"})
	case final(err) || hubclient.Code(err) == v1.CodeConflict:
		msg := "the hub refused the session's archive"
		if se != nil && se.Protocol != nil {
			msg += ": " + se.Protocol.Message
		}
		r.settle(ctx, row, &v1.ReleaseError{Code: v1.ReleaseUploadRefused, Message: msg, NextAction: "release it again, or continue the session on this runner"})
	case ctx.Err() != nil:
	default:
		log.Warn("the session's archive was not uploaded; retrying", "attempt", row.Attempts+1, "err", err)
		r.retry(ctx, row, err)
	}
}

func (r *Releases) retry(ctx context.Context, row db.Release, cause error) {
	next := r.Now().Add(resultBackoff(int(row.Attempts) + 1))
	if err := r.Store.RetryRelease(ctx, db.RetryReleaseParams{
		NextAttemptAt: next.UnixMilli(), LastError: sql.NullString{String: cause.Error(), Valid: true},
		Connection: row.Connection, SessionID: row.SessionID,
	}); err != nil {
		r.Log.Error("a release's retry is not recorded", "connection", row.Connection, "session", row.SessionID, "err", err)
	}
}

func (r *Releases) forgetArchive(ctx context.Context, row db.Release) {
	if err := r.Store.SetReleaseArchive(ctx, db.SetReleaseArchiveParams{Connection: row.Connection, SessionID: row.SessionID}); err != nil {
		r.Log.Error("a release's lost archive is not forgotten", "connection", row.Connection, "session", row.SessionID, "err", err)
	}
}

// released settles a release the hub took: the release and the session's
// close in one transaction, then the conversation and the archive go, and
// the collector reclaims the workdir.
func (r *Releases) released(ctx context.Context, row db.Release, sess db.Session) {
	log := r.Log.With("connection", row.Connection, "session", row.SessionID)
	now := r.Now().UnixMilli()
	err := r.Store.Tx(ctx, func(q *db.Queries) error {
		if _, err := q.SettleRelease(ctx, db.SettleReleaseParams{SettledAt: sql.NullInt64{Int64: now, Valid: true}, Connection: row.Connection, SessionID: row.SessionID}); err != nil {
			return err
		}
		_, err := q.CloseReleasedSession(ctx, db.CloseReleasedSessionParams{Now: sql.NullInt64{Int64: now, Valid: true}, Connection: row.Connection, ID: row.SessionID})
		return err
	})
	if err != nil {
		// The hub has the archive; the next sweep uploads it again, which
		// the hub answers as it did, and settles it then.
		log.Error("the hub took the session's archive and the release is not recorded; trying again shortly", "err", err)
		return
	}
	log.Info("session released: the hub has its archive, and its next run may go to another runner", "bytes", row.Bytes.Int64)
	if ad, ok := r.Adapters.Lookup(sess.Harness); ok {
		if rel, ok := ad.(adapter.Releaser); ok {
			conv := conversationOf(r.Data, r.Accounts.Lists(), sess.Harness, sess.NativeID.String, sess.Workdir, false)
			if err := rel.Forget(conv); err != nil {
				log.Warn("the released session's conversation could not be deleted on this runner; remove it by hand", "err", err)
			}
		}
	}
	removeArchive(row.Archive.String, r.Log)
	r.Sessions.Wake()
	r.wakeLoop(row.Connection)
}

// settle ends a release refused, the session left as it was.
func (r *Releases) settle(ctx context.Context, row db.Release, why *v1.ReleaseError) {
	body, _ := json.Marshal(why)
	if _, err := r.Store.SettleRelease(ctx, db.SettleReleaseParams{
		SettledAt: sql.NullInt64{Int64: r.Now().UnixMilli(), Valid: true}, Error: sql.NullString{String: string(body), Valid: true},
		Connection: row.Connection, SessionID: row.SessionID,
	}); err != nil {
		r.Log.Error("a release refused is not recorded; trying again shortly", "connection", row.Connection, "session", row.SessionID, "err", err)
		return
	}
	if row.Archive.Valid {
		removeArchive(row.Archive.String, r.Log)
	}
	r.Log.Warn("refused to release a session; it stays open here", "connection", row.Connection, "session", row.SessionID, "code", why.Code, "reason", why.Message)
	r.wakeLoop(row.Connection)
}

// conversationOf names a session's conversation for its adapter: where yad
// keeps the harness's transcripts for its accounts, and its default home. An
// import goes to the shared directory only when the harness has accounts
// here, which is where its next run reads; an export and a forget look in
// both, since accounts may have come or gone since the session's runs.
func conversationOf(data string, lists account.Lists, harness, native, workdir string, forImport bool) adapter.Conversation {
	c := adapter.Conversation{Native: native, Workdir: workdir, DefaultHome: account.DefaultHome(harness)}
	if account.Supported(harness) && (!forImport || len(lists[harness]) > 0) {
		c.Transcripts = account.TranscriptDir(data, harness)
	}
	return c
}

// removeArchive deletes a session archive this runner has no more use for. It
// holds a whole conversation, so one that will not go is said, by its path:
// nothing comes back to it once its release is settled.
func removeArchive(path string, log *slog.Logger) {
	if err := os.RemoveAll(path); err != nil {
		log.Error("a session archive could not be deleted, and holds a whole conversation — remove it by hand", "archive", path, "err", err)
	}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// carriedOf is the branches a session opened from an archive carries.
func carriedOf(sess db.Session) (map[string]string, error) {
	if !sess.Carried.Valid || sess.Carried.String == "" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(sess.Carried.String), &out); err != nil {
		return nil, fmt.Errorf("the branches session %s carried from its archive are unreadable: %w", sess.ID, err)
	}
	return out, nil
}
