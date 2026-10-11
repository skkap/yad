package fake

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/skkap/yad/internal/adapter"
)

// The fake's conversation is one file, <root>/fake/<native>, where root is
// the shared transcripts directory when the runner names one and the default
// home otherwise — the two places a real harness keeps its own. A test writes
// it with WriteConversation, as a harness would by running a turn.

func (a *Adapter) Releases() bool { return !a.NoRelease }

// ConversationPath is where the fake keeps a conversation under root.
func ConversationPath(root, native string) string {
	return filepath.Join(root, "fake", native)
}

// WriteConversation writes a conversation where the fake finds it.
func WriteConversation(root, native, text string) error {
	if err := os.MkdirAll(filepath.Join(root, "fake"), 0o700); err != nil {
		return err
	}
	return os.WriteFile(ConversationPath(root, native), []byte(text), 0o600)
}

func fakeRoots(c adapter.Conversation) []string {
	var out []string
	for _, r := range []string{c.Transcripts, c.DefaultHome} {
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

func (a *Adapter) Export(c adapter.Conversation) (adapter.Exported, error) {
	for _, root := range fakeRoots(c) {
		p := ConversationPath(root, c.Native)
		if fi, err := os.Stat(p); err == nil {
			return adapter.Exported{Dir: filepath.Dir(p), Files: []adapter.ConversationFile{{Name: c.Native, Path: p, Mode: fi.Mode().Perm()}}}, nil
		}
	}
	return adapter.Exported{}, adapter.ErrNoConversation
}

func (a *Adapter) Import(c adapter.Conversation, staged, _ string) error {
	roots := fakeRoots(c)
	if len(roots) == 0 {
		return errors.New("no root to import into")
	}
	for _, root := range roots {
		if _, err := os.Stat(ConversationPath(root, c.Native)); err == nil {
			return adapter.ErrConversationExists
		}
	}
	body, err := os.ReadFile(filepath.Join(staged, c.Native))
	if err != nil {
		return err
	}
	return WriteConversation(roots[0], c.Native, string(body))
}

func (a *Adapter) Forget(c adapter.Conversation) error {
	var errs []error
	for _, root := range fakeRoots(c) {
		if err := os.Remove(ConversationPath(root, c.Native)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
