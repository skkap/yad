package adapter

import (
	"errors"
	"io/fs"
)

// Releaser is an adapter whose sessions can be carried to another runner
// (decision 0084): it says which files on this machine are one session's
// conversation, puts a conversation exported elsewhere where its harness will
// resume it, and deletes one that has gone. The runner packs and unpacks the
// session archive and talks to the hub; the adapter knows only where its
// harness keeps a conversation. The runner advertises the release feature for
// each harness whose adapter is one, and refuses to release or import a
// session of any other.
type Releaser interface {
	Releases() bool
	// Export lists the files of the conversation, reading none of them.
	Export(c Conversation) (Exported, error)
	// Import puts the conversation whose files are under staged, by their
	// names in Exported.Files, where the harness resumes c.Native from
	// c.Workdir. from is Exported.Dir on the runner that exported it: the
	// conversation names paths under it, which Import points at where the
	// files are now. It refuses a conversation the harness already has here
	// (ErrConversationExists), leaving it as it was.
	Import(c Conversation, staged, from string) error
	// Forget deletes the conversation's files on this machine: the session
	// was released, and its conversation lives on in the archive.
	Forget(c Conversation) error
}

// Releases reports whether an adapter can carry its sessions to another
// runner.
func Releases(a Adapter) bool {
	r, ok := a.(Releaser)
	return ok && r.Releases()
}

// Conversation names one session's conversation on this machine.
type Conversation struct {
	// Native is the harness's own session id.
	Native string
	// Workdir is the session's workdir, where its runs execute.
	Workdir string
	// Transcripts is the directory yad keeps the harness's transcripts in for
	// its accounts, linked into every account home (account.TranscriptDir),
	// when the harness has accounts here; "" when its runs use the harness's
	// own default home. Export and Forget look in both; Import writes where
	// the next run will read.
	Transcripts string
	// DefaultHome is the harness's own default home (account.DefaultHome),
	// where a run with no account keeps its conversation.
	DefaultHome string
}

// Exported is a conversation's files, as Export found them.
type Exported struct {
	// Files are every file of the conversation, by name.
	Files []ConversationFile
	// Dir is the directory the conversation's files are in here, which the
	// conversation itself names when it points at one of them.
	Dir string
}

// ConversationFile is one file of a conversation.
type ConversationFile struct {
	// Name is its path inside the conversation, slash-separated and
	// relative: what the archive calls it on every runner.
	Name string
	// Path is where it is on this machine.
	Path string
	Mode fs.FileMode
}

var (
	// ErrNoConversation is a session whose harness has no conversation on
	// this machine to export: no run of it has started one, or its files are
	// gone.
	ErrNoConversation = errors.New("the harness has no conversation for this session on this runner")
	// ErrConversationExists is an import of a conversation the harness
	// already has here, under the same native id. Never overwritten: the one
	// here is somebody's.
	ErrConversationExists = errors.New("the harness already has a conversation with this id on this runner")
)
