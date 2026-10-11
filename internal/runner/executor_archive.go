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
)

// archiveRefusal is a session archive this runner will not open, with the
// class and message the run's result carries.
type archiveRefusal struct{ class, msg string }

func (r archiveRefusal) Error() string { return r.msg }

func rejected(format string, args ...any) error {
	return archiveRefusal{ClassArchiveRejected, fmt.Sprintf(format, args...)}
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
	defer os.RemoveAll(in)
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
	conv := conversationOf(e.Data, e.Accounts.Lists(), c.Run.Harness, m.NativeID, dir, true)
	switch err := rel.Import(conv, staged, m.Dir); {
	case errors.Is(err, adapter.ErrConversationExists):
		return "", rejected("the harness on this runner already has a conversation with the archive's id %s, and it is never overwritten — offer the session's next run to another runner, or start a new session", m.NativeID)
	case err != nil:
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
