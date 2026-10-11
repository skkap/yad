package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store/db"
	"github.com/skkap/yad/internal/workdir"
)

// archiveRefusal is a session archive this runner will not open, with the
// class and message the run's result carries.
type archiveRefusal struct{ class, msg string }

func (r archiveRefusal) Error() string { return r.msg }

func rejected(format string, args ...any) error {
	return archiveRefusal{ClassArchiveRejected, fmt.Sprintf(format, args...)}
}

// sourcesDiffer says why a run opening a session from an archive names
// sources other than the session's, or "". A continuing run names its
// session's sources or none, and the runner that opens a released session
// has only the archive's word for which those are: by URL, as the session
// keeps it, and every one a git source — a released session has no other.
func sourcesDiffer(run []v1.Source, carried []carriedSource) string {
	if len(run) == 0 {
		return ""
	}
	want := map[string]bool{}
	for _, s := range carried {
		want[s.URL] = true
	}
	got := map[string]bool{}
	for _, s := range workdir.StoredSources(run) {
		if s.Git == nil || !want[s.Git.URL] {
			return "the run names sources other than the ones its session was built from on the runner that released it — send the same sources, or none, to continue it"
		}
		got[s.Git.URL] = true
	}
	if len(got) != len(want) {
		return "the run names fewer sources than its session was built from on the runner that released it — send the same sources, or none, to continue it"
	}
	return ""
}

// importArchive opens a session from the session archive its hub said it
// opens from (decision 0081): fetched from the hub, checked against the
// run's sha256 and size, unpacked, and handed to the harness's adapter, which
// puts the conversation where the harness resumes it from this session's
// workdir. It returns the conversation's native id, or "" for a session with
// no archive to open. Until it succeeds the session keeps the archive, so a
// run that fails here leaves the next one to try again rather than start the
// conversation empty.
func (e *Exec) importArchive(ctx context.Context, c Claim, ad adapter.Adapter, dir string, lastSeq *int64) (string, error) {
	bg := context.WithoutCancel(ctx)
	sess, err := e.Store.GetSession(bg, db.GetSessionParams{Connection: c.Connection, ID: c.Run.Session.ID})
	if err != nil {
		return "", err
	}
	if !sess.Archive.Valid {
		return "", nil
	}
	var want v1.SessionArchive
	if err := json.Unmarshal([]byte(sess.Archive.String), &want); err != nil {
		return "", fmt.Errorf("the session's archive is recorded unreadably: %w", err)
	}
	rel, ok := ad.(adapter.Releaser)
	if !ok || !rel.Releases() {
		return "", archiveRefusal{ClassRefused, fmt.Sprintf("this runner cannot open a %s session from a session archive — offer it to a runner advertising release for %s", c.Run.Harness, c.Run.Harness)}
	}
	var hub ArchiveHub
	if e.Archives != nil {
		hub = e.Archives(c.Connection)
	}
	if hub == nil {
		return "", errors.New("no hub to fetch the session's archive from")
	}
	emit := func(status, text string) {
		ev := v1.Event{Kind: v1.EventStatus, Status: status, Text: text}
		if e.spool(bg, c, &ev, *lastSeq+1) {
			*lastSeq = ev.Seq
			e.report(c.Connection)
		}
	}
	in := filepath.Join(e.Data, "releases", "incoming", pathName(c.Connection), pathName(c.Run.Session.ID))
	if err := os.RemoveAll(in); err != nil {
		return "", err
	}
	if err := os.MkdirAll(in, 0o700); err != nil {
		return "", err
	}
	defer removeArchive(in, e.Log)
	file := filepath.Join(in, "archive.tar.gz")
	out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	emit("fetching the session's archive", fmt.Sprintf("%d bytes from the hub", want.Bytes))
	_, n, err := hub.GetArchive(ctx, c.Run.Session.ID, want.Bytes, out)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if final(err) || hubclient.Code(err) == v1.CodeNotFound {
			return "", rejected("the hub would not hand over the session's archive: %v — the session's next run here asks again; if the hub no longer has it, start a new session", err)
		}
		return "", fmt.Errorf("the session's archive could not be fetched from the hub (%w); the session's next run here tries again", err)
	}
	sum, _, err := fileSHA256(file)
	if err != nil {
		return "", err
	}
	if sum != want.SHA256 || n != want.Bytes {
		return "", rejected("the archive the hub sent (%d bytes, sha256 %s) is not the one the run named (%d bytes, sha256 %s); the session's next run here asks again", n, sum, want.Bytes, want.SHA256)
	}
	staged := filepath.Join(in, "conversation")
	m, err := unpackArchive(file, staged)
	var bad errArchive
	if errors.As(err, &bad) {
		return "", rejected("%s", string(bad))
	}
	if err != nil {
		return "", err
	}
	if m.Harness != c.Run.Harness {
		return "", rejected("the archive holds a %s session, and the run is a %s one", m.Harness, c.Run.Harness)
	}
	if why := sourcesDiffer(c.Run.Sources, m.Sources); why != "" {
		return "", rejected("%s", why)
	}
	conv := conversationOf(e.Data, e.Accounts.Lists(), c.Run.Harness, m.NativeID, dir, true)
	err = rel.Import(conv, staged, m.Dir)
	if errors.Is(err, adapter.ErrConversationExists) {
		// A conversation by this id that no open session here holds is this
		// session's own, left by an import whose end this runner never
		// recorded, or by an earlier stay of the session here: the archive
		// is newer. One an open session holds is somebody's, and stays.
		taken, qerr := e.Store.OtherOpenSessionWithNativeID(bg, db.OtherOpenSessionWithNativeIDParams{
			NativeID: sql.NullString{String: m.NativeID, Valid: true}, Connection: c.Connection, ID: c.Run.Session.ID,
		})
		switch {
		case qerr != nil:
			return "", qerr
		case taken:
			return "", rejected("another session on this runner holds a conversation with the archive's id %s, and it is never overwritten — offer the session's next run to another runner, or start a new session", m.NativeID)
		}
		e.Log.Info("replacing a conversation no open session holds with the session's archive", "connection", c.Connection, "session", c.Run.Session.ID)
		if err = rel.Forget(conversationOf(e.Data, e.Accounts.Lists(), c.Run.Harness, m.NativeID, dir, false)); err == nil {
			err = rel.Import(conv, staged, m.Dir)
		}
	}
	if err != nil {
		return "", err
	}
	branches := map[string]string{}
	for _, s := range m.Sources {
		if s.URL != "" && s.Branch != "" {
			branches[s.URL] = s.Branch
		}
	}
	var carried sql.NullString
	if len(branches) > 0 {
		b, err := json.Marshal(branches)
		if err != nil {
			return "", err
		}
		carried = sql.NullString{String: string(b), Valid: true}
	}
	if err := e.Store.SetSessionImported(bg, db.SetSessionImportedParams{
		NativeID: sql.NullString{String: m.NativeID, Valid: true}, Carried: carried, LastUsedAt: time.Now().UnixMilli(),
		Connection: c.Connection, ID: c.Run.Session.ID,
	}); err != nil {
		// The conversation is in place and the session does not say so: the
		// next run's import finds it there and is refused, so it goes back.
		if ferr := rel.Forget(conv); ferr != nil {
			e.Log.Warn("an imported conversation the session does not record could not be removed", "connection", c.Connection, "session", c.Run.Session.ID, "err", ferr)
		}
		return "", err
	}
	version := ""
	if m.HarnessVersion != "" {
		version = " " + m.HarnessVersion
	}
	emit("session archive opened", fmt.Sprintf("the conversation continues from another runner's %s%s", m.Harness, version))
	e.Log.Info("opened the session from its archive", "connection", c.Connection, "session", c.Run.Session.ID, "bytes", n, "harness_version", m.HarnessVersion)
	return m.NativeID, nil
}
