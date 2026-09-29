package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/acp/acptest"
	"github.com/skkap/yad/internal/adapter/jsonrpc"
)

// A failed prompt is classified from OpenCode's structure where it has one —
// the ACP code, errorName — and from its words only for the usage limits Zen
// says nowhere else, in the wording of OpenCode 1.18.33's console.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    jsonrpc.RPCError
		class  string
		auth   bool
		window string
		reset  time.Duration
	}{
		{"auth", jsonrpc.RPCError{Code: codeAuthRequired, Message: "Authentication required: provider authentication required", Data: []byte(`{"providerId":"anthropic"}`)},
			adapter.ClassHarness, true, "", 0},
		{"context overflow", jsonrpc.RPCError{Code: jsonrpc.CodeInternal, Message: "Internal error: Input exceeds context window of this model", Data: []byte(`{"service":"session","errorName":"ContextOverflowError"}`)},
			adapter.ClassPromptTooLong, false, "", 0},
		{"5-hour window", jsonrpc.RPCError{Code: jsonrpc.CodeInternal, Message: "Internal error: 5-hour usage limit reached. Resets in 2hr 30min. To continue using this model now, enable usage from your available balance: https://opencode.ai/workspace/w/go", Data: []byte(`{"service":"session","errorName":"APIError"}`)},
			adapter.ClassUsageLimit, false, "5-hour", 150 * time.Minute},
		{"weekly window", jsonrpc.RPCError{Code: jsonrpc.CodeInternal, Message: "Weekly usage limit reached. Resets in 3 days. To continue using this model now, enable usage from your available balance: https://opencode.ai/workspace/w/go", Data: []byte(`{"errorName":"APIError"}`)},
			adapter.ClassUsageLimit, false, "weekly", 72 * time.Hour},
		{"subscription quota", jsonrpc.RPCError{Code: jsonrpc.CodeInternal, Message: "Subscription quota exceeded. Retry in 45min.", Data: []byte(`{"errorName":"APIError"}`)},
			adapter.ClassUsageLimit, false, "subscription", 45 * time.Minute},
		// A provider's sentence that mentions a limit is not one.
		{"a mention", jsonrpc.RPCError{Code: jsonrpc.CodeInternal, Message: "Internal error: your request hit a usage limit reached. Resets in soon.", Data: []byte(`{"errorName":"APIError"}`)},
			adapter.ClassHarness, false, "", 0},
		// Rate limits OpenCode retries by itself; the one that outlasts its
		// retries is not a usage limit (DOMAIN.md).
		{"rate limit", jsonrpc.RPCError{Code: jsonrpc.CodeInternal, Message: "Internal error: Rate limit exceeded. Please try again later.", Data: []byte(`{"errorName":"APIError"}`)},
			adapter.ClassHarness, false, "", 0},
		{"provider down", jsonrpc.RPCError{Code: jsonrpc.CodeInternal, Message: "Internal error: Error from provider (Console): Upstream request failed: Endpoint is unavailable.", Data: []byte(`{"service":"session","errorName":"APIError"}`)},
			adapter.ClassHarness, false, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := classify(&tc.err, adapter.Spec{})
			if f.Class != tc.class || f.AuthRejected != tc.auth {
				t.Fatalf("classified %+v", f)
			}
			if tc.window == "" {
				if f.Limit != nil {
					t.Errorf("limit %+v", f.Limit)
				}
				return
			}
			if f.Limit == nil || f.Limit.Window != tc.window {
				t.Fatalf("limit %+v, want window %s", f.Limit, tc.window)
			}
			if d := time.Until(f.Limit.ResetAt) - tc.reset; d > 2*time.Minute || d < -2*time.Minute {
				t.Errorf("reset at %s, want in %s", f.Limit.ResetAt, tc.reset)
			}
		})
	}
	f := classify(&jsonrpc.RPCError{Code: codeAuthRequired, Data: []byte(`{"providerId":"anthropic"}`)}, adapter.Spec{})
	if !strings.Contains(f.Message, "anthropic") || !strings.Contains(f.Message, "opencode auth login") {
		t.Errorf("auth message %q", f.Message)
	}
}

func TestResetIn(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"1 day": 24 * time.Hour, "2 days": 48 * time.Hour, "3hr 20min": 200 * time.Minute, "45min": 45 * time.Minute,
	} {
		if got, ok := resetIn(in); !ok || got != want {
			t.Errorf("resetIn(%q) = %s %v, want %s", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "soon", "3 fortnights"} {
		if _, ok := resetIn(in); ok {
			t.Errorf("resetIn(%q) read a time", in)
		}
	}
}

// Only the pinned release goes without a warning, and the warning never
// quotes more than the version.
func TestVersionWarning(t *testing.T) {
	if w := VersionWarning(PinnedVersion); w != "" {
		t.Errorf("the pinned release warns: %s", w)
	}
	if w := VersionWarning("1.19.0"); !strings.Contains(w, "1.19.0") || !strings.Contains(w, PinnedVersion) {
		t.Errorf("another release: %q", w)
	}
}

// `opencode models` is the model list, and nothing on it that is not shaped
// provider/model is reported (DEV-67). The list recorded in a home with no
// login is OpenCode Zen's free models.
func TestListModels(t *testing.T) {
	recorded, err := filepath.Abs(filepath.Join(fixtures, "list-models.txt"))
	if err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(t.TempDir(), "models")
	os.WriteFile(junk, []byte("opencode/big-pickle\nWARN something /home/user/.cache said\n\x1b[0mopencode/x\n"), 0o600)
	for _, tc := range []struct {
		name, file string
		want       []string
		kind       error
	}{
		{"recorded", recorded, nil, nil},
		{"junk", junk, []string{"opencode/big-pickle"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{acptest.EnvFixture + "=unused", acptest.EnvModels + "=" + tc.file}
			got, err := ListModels(context.Background(), os.Args[0], t.TempDir(), env)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != nil && !slices.Equal(got, tc.want) {
				t.Errorf("models %v, want %v", got, tc.want)
			}
			for _, m := range got {
				if !modelName.MatchString(m) {
					t.Errorf("%q reported", m)
				}
			}
			if len(got) == 0 {
				t.Error("no models")
			}
		})
	}
	empty := filepath.Join(t.TempDir(), "none")
	os.WriteFile(empty, nil, 0o600)
	_, err = ListModels(context.Background(), os.Args[0], t.TempDir(), []string{acptest.EnvFixture + "=unused", acptest.EnvModels + "=" + empty})
	if !errors.Is(err, adapter.ErrModelsUnread) {
		t.Errorf("an empty list: %v", err)
	}
}
