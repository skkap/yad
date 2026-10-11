package v1

import (
	"regexp"
	"time"
)

// A release (decision 0080) hands a session back to its hub so that its next
// run can go to another runner. The runner holding the session uploads its
// session archive — the harness's conversation and what a runner needs to
// carry it on — with PUT /sessions/{session}/archive, closes the session, and
// says so in released_sessions; the hub then offers the session's next run as
// one opening it, with session.archive, and the runner that claims it fetches
// the archive with GET and continues the conversation under the same session
// id. Everything here goes only to and from a runner advertising the
// "release" feature for the session's harness.

// HeaderArchiveSHA256 carries a session archive's SHA-256, as 64 lower-case hex
// digits, on the PUT that uploads it and the GET that fetches it. A hub
// refuses an upload whose body does not hash to it, and a runner an archive
// that does not.
const HeaderArchiveSHA256 = "Yad-Archive-Sha256"

// ArchiveContentType is a session archive's media type. The archive is opaque
// to a hub: it stores the bytes and hands them back.
const ArchiveContentType = "application/gzip"

// SessionArchive names one session archive by its content: the SHA-256 of its
// bytes, and how many there are.
type SessionArchive struct {
	SHA256 string `json:"sha256" doc:"The SHA-256 of the archive's bytes, as 64 lower-case hex digits. A runner fetching it refuses an archive that does not hash to this."`
	Bytes  int64  `json:"bytes" doc:"The archive's size in bytes."`
}

// ReleasedSession is one release_session a runner has settled, either way,
// repeated in every sync until one carrying it is answered with a 2xx — the
// closed_sessions pattern (decision 0035). Not a close reason: those are a
// closed set for all of v1, and a released session is not gone.
type ReleasedSession struct {
	SessionID string `json:"session_id" doc:"The session, by the id the hub gave it."`
	// ReleasedAt is when the runner settled the release: when the hub took
	// the archive, or when the runner refused.
	ReleasedAt time.Time `json:"released_at" doc:"When the runner settled the release: when the hub answered the archive's upload with a 2xx, or when the runner refused it."`
	// Archive is set when the session was released: the hub holds it, the
	// runner has closed the session, and its next run may go to any runner
	// advertising release for its harness.
	Archive *SessionArchive `json:"archive,omitempty" doc:"Set when the session was released: the archive this runner uploaded, which the hub must hold with this sha256. The runner has closed the session and keeps nothing of it; offer the session's next run as opening it (session.new true) with this archive in session.archive, to a runner advertising release for its harness. Absent when error is set."`
	// Error is set when the runner refused: the session stays open on it,
	// as it was, and takes runs again.
	Error *ReleaseError `json:"error,omitempty" doc:"Set when the runner did not release the session: it stays open on this runner exactly as it was, and its runs go on here. Absent when archive is set."`
}

// ReleaseError is why a runner did not release a session, with what to do.
type ReleaseError struct {
	// Code is not a closed set: a hub meeting one it does not know shows the
	// message and the next action.
	Code       string `json:"code" doc:"Why, for a program: local_changes (the workdir holds work not committed), unpushed_commits (its branch holds commits the remote does not), path_source (a source is a directory on the runner's machine, which no other runner has), no_conversation (no run of the session has started a conversation yet), not_releasable (this runner cannot release a session of its harness, or its owner has turned release off for this hub), archive_too_large (the archive is past the hub's archive_max_bytes), upload_refused (the hub refused the archive), session_unknown (the runner holds no such session for this hub), session_closed (the session was closed on the runner), or export_failed (the conversation could not be read on the runner). Not a closed set: show one you do not know."`
	Message    string `json:"message" doc:"What happened, for a person."`
	NextAction string `json:"next_action" doc:"What to do about it, for a person: commit and push the work with a run in the session, send the release again with discard_local, or start a new session."`
}

// Codes a runner puts in ReleaseError.Code. A hub acts on a code it knows and
// shows the message of one it does not; a new code is an addition.
const (
	ReleaseLocalChanges   = "local_changes"
	ReleaseUnpushed       = "unpushed_commits"
	ReleasePathSource     = "path_source"
	ReleaseNoConversation = "no_conversation"
	ReleaseNotReleasable  = "not_releasable"
	ReleaseTooLarge       = "archive_too_large"
	ReleaseUploadRefused  = "upload_refused"
	ReleaseUnknown        = "session_unknown"
	ReleaseClosed         = "session_closed"
	ReleaseExportFailed   = "export_failed"
)

// archiveSHA256 is what a SessionArchive's sha256 looks like: what
// crypto/sha256 gives, hex-encoded.
var archiveSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA256 reports whether s is a sha256 as the protocol writes one.
func ValidSHA256(s string) bool { return archiveSHA256.MatchString(s) }
