package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/shellword"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// The same for Claude: the login check runs in the account's config
// directory, which CLAUDE_CONFIG_DIR names, and the path itself never leaves
// the machine.
func TestExitWithoutResultChecksTheAccountsLogin(t *testing.T) {
	h := &harness{fixture: derive(t, "plain", func(l []string) []string { return l[:indexOf(l, "result")] })}
	spec := h.spec(t)
	spec.Home = "/Users/owner/.local/share/yad/accounts/claude/work"
	spec.HomeVar, spec.Account = "CLAUDE_CONFIG_DIR", "work"
	spec.Yad = func(args ...string) string {
		return shellword.Command(append([]string{"yad", "--profile", "side"}, args...)...)
	}
	_, out, _ := drive(t, context.Background(), spec, nil)
	if out.Error == nil || out.Error.Class != adapter.ClassHarnessExited {
		t.Fatalf("outcome = %+v (%+v)", out, out.Error)
	}
	msg := out.Error.Message
	if strings.Contains(msg, spec.Home) {
		t.Errorf("the message carries the account's home, and goes to a hub: %s", msg)
	}
	check := shellwordtest.Commands(msg, "claude ")
	if len(check) != 1 {
		t.Fatalf("want one claude command in %q", msg)
	}
	shellwordtest.CheckEnv(t, check[0], map[string]string{"CLAUDE_CONFIG_DIR": "<home>"}, "claude", "-p", "hello")
	list := shellwordtest.Commands(msg, "yad ")
	if len(list) != 1 {
		t.Fatalf("want the yad command that shows the home in %q", msg)
	}
	shellwordtest.Check(t, list[0], "yad", "--profile", "side", "account", "list")
}
