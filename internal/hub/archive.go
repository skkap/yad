package hub

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// Releasing a session (decision 0081), the hub's half: the service API asks
// for a release, the bound runner hears release_session in every sync answer
// until it reports how it went, uploads the session archive in between, and
// a release it reports leaves the session unbound with its archive here. The
// session's next run then goes out opening it, with session.archive, to any
// runner advertising release for its harness, which fetches the archive. The
// archive is opaque here: stored, handed back, and deleted once a run opened
// from it is running, the session closes, or it is older than the TTL.

type (
	putArchiveInput struct {
		ProtocolHeader
		Session string `path:"session" doc:"The session id."`
		SHA256  string `header:"Yad-Archive-Sha256" required:"true" pattern:"^[0-9a-f]{64}$" doc:"The SHA-256 of the body, as 64 lower-case hex digits. A body that does not hash to it is refused as invalid."`
		RawBody []byte `contentType:"application/gzip"`
	}
	archiveOutput   struct{ Body v1.SessionArchive }
	getArchiveInput struct {
		ProtocolHeader
		Session string `path:"session" doc:"The session id."`
	}
	getArchiveOutput struct {
		ContentType string `header:"Content-Type" doc:"application/gzip."`
		SHA256      string `header:"Yad-Archive-Sha256" doc:"The SHA-256 of the body, as 64 lower-case hex digits."`
		Body        []byte
	}
)

func (h *Hub) registerArchives(api huma.API) {
	protocolOp(api, huma.Operation{
		OperationID: "putSessionArchive", Method: http.MethodPut, Path: "/sessions/{session}/archive",
		Summary: "Upload the archive of a session this runner is releasing",
		Description: "Sent by the runner a session is bound to, while a release of it is asked for (the release_session control), " +
			"before it reports the release in released_sessions. The body is the session archive, opaque to the hub: store it as it " +
			"is. An upload of the same bytes again is answered as the first was; one of other bytes replaces it. 403 not_holder for " +
			"a runner the session is not bound to, 409 conflict for a session no release of which is asked for, 413 for an archive " +
			"past the hub's archive_max_bytes, which the runner reports as archive_too_large and keeps the session.",
		Security: security, Errors: []int{400, 401, 403, 404, 409, 413, 426},
		MaxBodyBytes: h.archiveMax,
		// huma gives a body five seconds; an archive is given what a
		// runner gives its upload — a minute, and a second for each 256 KiB
		// of the largest it may be — so a slow uplink is not cut off.
		BodyReadTimeout: time.Minute + time.Duration(h.archiveMax/(256<<10))*time.Second,
	}, h.putArchive)

	protocolOp(api, huma.Operation{
		OperationID: "getSessionArchive", Method: http.MethodGet, Path: "/sessions/{session}/archive",
		Summary: "Fetch the archive a released session opens from",
		Description: "Answered only to the runner a run of the session is offered to, or the runner the session is bound to, " +
			"while the hub holds the archive its last runner released it with — the run carrying session.archive. The runner " +
			"checks the bytes against that run's session.archive.sha256. 403 not_holder for any other runner; 404 not_found when " +
			"the hub holds no released archive for the session.",
		Security: security, Errors: []int{401, 403, 404, 426},
	}, h.getArchive)
	// huma would describe the bytes as JSON; they are the archive.
	get := api.OpenAPI().Paths["/sessions/{session}/archive"].Get
	get.Responses["200"].Content = map[string]*huma.MediaType{
		v1.ArchiveContentType: {Schema: &huma.Schema{Type: "string", Format: "binary"}},
	}
}

func (h *Hub) putArchive(ctx context.Context, in *putArchiveInput) (*archiveOutput, error) {
	runner, err := h.caller(ctx)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(in.RawBody)
	got := hex.EncodeToString(sum[:])
	if got != in.SHA256 {
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
			fmt.Sprintf("the archive's bytes hash to %s, not the %s the request named", got, in.SHA256),
			"upload the archive again, whole")
	}
	out := v1.SessionArchive{SHA256: got, Bytes: int64(len(in.RawBody))}
	err = h.store.Tx(ctx, func(q *db.Queries) error {
		sess, err := q.GetSession(ctx, in.Session)
		if errors.Is(err, sql.ErrNoRows) {
			return Fail(http.StatusNotFound, v1.CodeNotFound, fmt.Sprintf("this hub has no session %q", in.Session), "keep the session: nothing here asked for it")
		}
		if err != nil {
			return err
		}
		if !sess.RunnerID.Valid || sess.RunnerID.String != runner.ID {
			return Fail(http.StatusForbidden, v1.CodeNotHolder, fmt.Sprintf("session %s is not bound to runner %s", in.Session, runner.ID),
				"keep the session: only the runner a session is bound to releases it")
		}
		if !sess.ReleaseRequestedAt.Valid {
			return Fail(http.StatusConflict, v1.CodeConflict, fmt.Sprintf("no release of session %s is asked for", in.Session),
				"keep the session: the hub has not asked for it back")
		}
		if have, err := q.GetSessionArchive(ctx, in.Session); err == nil && have.Sha256 == got && have.RunnerID == runner.ID && have.Released == 0 {
			return nil
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return q.PutSessionArchive(ctx, db.PutSessionArchiveParams{
			SessionID: in.Session, Sha256: got, Bytes: int64(len(in.RawBody)), Data: in.RawBody, RunnerID: runner.ID, UploadedAt: store.Ms(h.now()),
		})
	})
	if err != nil {
		return nil, err
	}
	return &archiveOutput{Body: out}, nil
}

func (h *Hub) getArchive(ctx context.Context, in *getArchiveInput) (*getArchiveOutput, error) {
	runner, err := h.caller(ctx)
	if err != nil {
		return nil, err
	}
	out := &getArchiveOutput{ContentType: v1.ArchiveContentType}
	err = h.store.Tx(ctx, func(q *db.Queries) error {
		missing := Fail(http.StatusNotFound, v1.CodeNotFound, fmt.Sprintf("this hub holds no released archive for session %q", in.Session),
			"the session cannot be opened from an archive here: start a new session")
		sess, err := q.GetSession(ctx, in.Session)
		if errors.Is(err, sql.ErrNoRows) {
			return missing
		}
		if err != nil {
			return err
		}
		arch, err := q.GetSessionArchive(ctx, in.Session)
		if errors.Is(err, sql.ErrNoRows) || err == nil && arch.Released == 0 {
			return missing
		}
		if err != nil {
			return err
		}
		mine := sess.RunnerID.Valid && sess.RunnerID.String == runner.ID
		if !mine {
			offered, err := q.RunOfferedInSession(ctx, db.RunOfferedInSessionParams{SessionID: in.Session, RunnerID: sql.NullString{String: runner.ID, Valid: true}})
			if err != nil {
				return err
			}
			mine = offered
		}
		if !mine {
			return Fail(http.StatusForbidden, v1.CodeNotHolder, fmt.Sprintf("no run of session %s is offered to runner %s, and the session is not bound to it", in.Session, runner.ID),
				"fetch the archive only for a run carrying session.archive that this runner was offered")
		}
		data, err := q.SessionArchiveData(ctx, in.Session)
		if err != nil {
			return err
		}
		out.Body, out.SHA256 = data, arch.Sha256
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// registerRelease is the service API's release.
func (h *Hub) registerRelease(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "releaseSession", Method: http.MethodPost, Path: "/sessions/{session}/release",
		Summary: "Release a session, so its next run can go to another runner",
		Description: "The session's runner hands it back: it uploads the session's conversation as a session archive, closes the " +
			"session, and the session's next run goes to any runner advertising release for its harness, which opens it from " +
			"the archive and continues it under the same id. While a release is asked for the session is releasing and its runs " +
			"wait; once the runner has done it, it is released. A runner refuses — the session staying open on it, release_error " +
			"saying why — when the workdir holds work its remote does not, unless discard_local is set, when a source is a " +
			"directory on its machine, or when no run of it has started a conversation. A session on no runner, closed or " +
			"closing is 409, and so is one whose runner does not advertise release for its harness. Releasing a released session " +
			"answers it as it is.",
		Security: adminSecurity, Errors: []int{401, 404, 409},
	}, func(ctx context.Context, in *releaseInput) (*sessionOutput, error) {
		var view hubapi.Session
		err := h.store.Tx(ctx, func(q *db.Queries) error {
			sess, err := q.GetSession(ctx, in.Session)
			if errors.Is(err, sql.ErrNoRows) {
				_, err = sessionView(ctx, q, in.Session) // the 404
			}
			if err != nil {
				return err
			}
			switch {
			case sess.ClosedAt.Valid:
				return Fail(http.StatusConflict, v1.CodeConflict, fmt.Sprintf("session %s is closed", sess.ID), "start a new session")
			case sess.CloseRequestedAt.Valid:
				return Fail(http.StatusConflict, v1.CodeConflict, fmt.Sprintf("session %s is closing", sess.ID), "start a new session")
			case !sess.RunnerID.Valid && sess.ReleasedAt.Valid:
				// Released already: the same answer as the first time.
			case !sess.RunnerID.Valid:
				return Fail(http.StatusConflict, v1.CodeConflict, fmt.Sprintf("session %s is on no runner yet", sess.ID),
					"nothing to release: its next run may go to any runner already")
			default:
				r, err := q.GetRunner(ctx, sess.RunnerID.String)
				if err != nil {
					return err
				}
				doc, err := storedDoc(r)
				if err != nil {
					return err
				}
				if !capability.RunMayUse(doc, sess.Harness, capability.FeatureRelease) {
					return Fail(http.StatusConflict, v1.CodeConflict,
						fmt.Sprintf("session %s is on runner %s, which does not advertise the %q feature for harness %q to this hub: its owner has turned release off for this hub (release_sessions = false), its harness cannot be carried, or it runs an older yad", sess.ID, r.ID, capability.FeatureRelease, sess.Harness),
						"continue the session on that runner, or start a new session")
				}
				if _, err := q.RequestSessionRelease(ctx, db.RequestSessionReleaseParams{
					Now: sql.NullInt64{Int64: store.Ms(h.now()), Valid: true}, DiscardLocal: boolInt(in.Body.DiscardLocal), ID: sess.ID,
				}); err != nil {
					return err
				}
			}
			view, err = sessionView(ctx, q, sess.ID)
			return err
		})
		if err != nil {
			return nil, err
		}
		h.bell.ring()
		return &sessionOutput{Body: view}, nil
	})
}

type releaseInput struct {
	Session string `path:"session" doc:"The session id."`
	Body    hubapi.ReleaseRequest
}

// syncReleases takes the releases a runner reports, and returns the
// release_session controls it is still owed. Reports first, so a release
// this sync settles is not asked for again in its own answer.
func syncReleases(ctx context.Context, q *db.Queries, runnerID string, reported []v1.ReleasedSession, doc v1.Capabilities, described bool, now time.Time) ([]v1.Control, error) {
	me := sql.NullString{String: runnerID, Valid: true}
	for _, rs := range reported {
		sess, err := q.GetSession(ctx, rs.SessionID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Believed only from the runner the session is bound to, and only
		// while a release is asked for: a report from any other changes
		// nothing, and a repeat finds the release already settled.
		if !sess.RunnerID.Valid || sess.RunnerID.String != runnerID || !sess.ReleaseRequestedAt.Valid {
			continue
		}
		if rs.Error != nil {
			body, err := json.Marshal(rs.Error)
			if err != nil {
				return nil, err
			}
			if _, err := q.EndSessionRelease(ctx, db.EndSessionReleaseParams{Error: sql.NullString{String: string(body), Valid: true}, ID: sess.ID, RunnerID: me}); err != nil {
				return nil, err
			}
			// What a refused release uploaded is nobody's conversation; an
			// archive the session was opened from is kept until a run
			// opened from it is running.
			if a, err := q.GetSessionArchive(ctx, sess.ID); err == nil && a.Released == 0 {
				if err := q.DeleteSessionArchive(ctx, sess.ID); err != nil {
					return nil, err
				}
			}
			continue
		}
		a, err := q.GetSessionArchive(ctx, sess.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err != nil || rs.Archive == nil || a.Sha256 != rs.Archive.SHA256 || a.RunnerID != runnerID {
			// The runner has closed the session believing the hub holds its
			// archive, and it does not: the conversation is gone, and the
			// session with it, as a close the runner reported would be.
			if err := closeHere(ctx, q, sess.ID, v1.SessionClosed, unstartedEnd{v1.RunFailed, fmt.Sprintf(
				"its session was released by runner %s, and the archive it reported is not the one this hub holds; submit the work to a new session", runnerID)}, now); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := q.SetArchiveReleased(ctx, db.SetArchiveReleasedParams{SessionID: sess.ID, Sha256: a.Sha256, RunnerID: runnerID}); err != nil {
			return nil, err
		}
		if _, err := q.MarkSessionReleased(ctx, db.MarkSessionReleasedParams{Now: sql.NullInt64{Int64: store.Ms(now), Valid: true}, RunnerID: me, ID: sess.ID}); err != nil {
			return nil, err
		}
		// Every runner it leaves keeps its closed row, so none of them is
		// offered it again, however many moves later.
		if err := q.NoteSessionReleased(ctx, db.NoteSessionReleasedParams{SessionID: sess.ID, RunnerID: runnerID, ReleasedAt: store.Ms(now)}); err != nil {
			return nil, err
		}
		// A fork of it waiting to open can no longer: a fork opens only on
		// the runner holding the session it forks, and this one is on none.
		if err := endForksOf(ctx, q, sess.ID, sourceReleased(sess.ID), now); err != nil {
			return nil, err
		}
	}
	asked, err := q.SessionsToRelease(ctx, me)
	if err != nil || !described {
		return nil, err
	}
	var out []v1.Control
	for _, s := range asked {
		if capability.RunMayUse(doc, s.Harness, capability.FeatureRelease) {
			out = append(out, v1.Control{Kind: v1.ControlReleaseSession, SessionID: s.ID, DiscardLocal: s.ReleaseDiscardLocal != 0})
			continue
		}
		// The runner stopped advertising release since it was asked for:
		// its owner turned it off, or it went back to an older yad. A
		// control would be ignored, and the session's runs would wait for
		// ever behind it.
		body, err := json.Marshal(v1.ReleaseError{Code: v1.ReleaseNotReleasable,
			Message:    fmt.Sprintf("runner %s no longer advertises the %q feature for harness %q", runnerID, capability.FeatureRelease, s.Harness),
			NextAction: "continue the session on that runner"})
		if err != nil {
			return nil, err
		}
		if _, err := q.EndSessionRelease(ctx, db.EndSessionReleaseParams{Error: sql.NullString{String: string(body), Valid: true}, ID: s.ID, RunnerID: me}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// sourceReleased is what becomes of a run waiting to open a fork of a
// session that was released: no runner holds its conversation to copy.
func sourceReleased(source string) unstartedEnd {
	return unstartedEnd{v1.RunFailed, fmt.Sprintf(
		"the session it forks, %s, was released before this fork opened, and a fork opens only where the session it forks is; fork it once it is open on a runner again", source)}
}

// releasedArchive is the archive a released session's next run opens from,
// or nil: a session bound to no runner whose archive its runner released.
func releasedArchive(ctx context.Context, q *db.Queries, sess db.Session) (*v1.SessionArchive, error) {
	if sess.RunnerID.Valid || !sess.ReleasedAt.Valid {
		return nil, nil
	}
	a, err := q.GetSessionArchive(ctx, sess.ID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && a.Released == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v1.SessionArchive{SHA256: a.Sha256, Bytes: a.Bytes}, nil
}

// expireArchives deletes archives past the TTL. A released session whose
// archive goes can open nowhere — its conversation is in that archive alone
// — so it closes, its waiting runs failed with the reason; one bound to a
// runner that has not yet opened it is asked to close, as a hub's own close
// is. An upload whose release was never reported goes on its own.
func (h *Hub) expireArchives(ctx context.Context, q *db.Queries, now time.Time) error {
	old, err := q.ExpiredArchives(ctx, store.Ms(now.Add(-h.archiveTTL)))
	if err != nil {
		return err
	}
	for _, a := range old {
		if err := q.DeleteSessionArchive(ctx, a.SessionID); err != nil {
			return err
		}
		if a.Released == 0 {
			continue
		}
		sess, err := q.GetSession(ctx, a.SessionID)
		if err != nil {
			return err
		}
		end := unstartedEnd{v1.RunFailed, fmt.Sprintf("its session's archive was kept %s for a run to open it from, and no runner did; submit the work to a new session", h.archiveTTL)}
		if !sess.RunnerID.Valid {
			if err := closeHere(ctx, q, sess.ID, v1.SessionClosed, end, now); err != nil {
				return err
			}
			continue
		}
		if err := q.RequestSessionClose(ctx, db.RequestSessionCloseParams{Now: sql.NullInt64{Int64: store.Ms(now), Valid: true}, ID: sess.ID}); err != nil {
			return err
		}
	}
	return nil
}

// releaseView fills a session's release fields.
func releaseView(ctx context.Context, q *db.Queries, s db.Session, view *hubapi.Session) error {
	if s.ReleaseRequestedAt.Valid {
		t := time.UnixMilli(s.ReleaseRequestedAt.Int64).UTC()
		view.ReleaseRequestedAt = &t
		view.Release = hubapi.SessionReleasing
	}
	if s.ReleasedAt.Valid {
		t := time.UnixMilli(s.ReleasedAt.Int64).UTC()
		view.ReleasedAt, view.ReleasedFrom = &t, s.ReleasedFrom.String
		if !s.RunnerID.Valid {
			view.Release = hubapi.SessionReleased
		}
	}
	if s.ReleaseError.Valid {
		var e v1.ReleaseError
		if json.Unmarshal([]byte(s.ReleaseError.String), &e) == nil {
			view.ReleaseError = &e
		}
	}
	a, err := q.GetSessionArchive(ctx, s.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		view.Archive = &v1.SessionArchive{SHA256: a.Sha256, Bytes: a.Bytes}
	}
	return nil
}
