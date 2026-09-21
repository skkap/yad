package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/shellword"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// A turn on an account that ends badly tells its owner to check the login —
// in the account's own home. Bare, `codex login status` reads the owner's
// default home and answers about another login entirely. The home is a path
// under the owner's home and the message goes to a hub, so it stays a
// placeholder, named by the yad command that shows it on that runner.
func TestExitWithoutCompletionChecksTheAccountsLogin(t *testing.T) {
	h := &harness{fixture: cutBefore(t, "plain", `{"method":"turn/completed"`), env: map[string]string{"CODEX_TEST_MODE": "exit"}}
	spec := h.spec(t)
	spec.Home = "/Users/owner/.local/share/yad/accounts/codex/work"
	spec.HomeVar, spec.Account = "CODEX_HOME", "work"
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
	check := shellwordtest.Commands(msg, "codex ")
	if len(check) != 1 {
		t.Fatalf("want one codex command in %q", msg)
	}
	shellwordtest.CheckEnv(t, check[0], map[string]string{"CODEX_HOME": "<home>"}, "codex", "login", "status")
	list := shellwordtest.Commands(msg, "yad ")
	if len(list) != 1 {
		t.Fatalf("want the yad command that shows the home in %q", msg)
	}
	shellwordtest.Check(t, list[0], "yad", "--profile", "side", "account", "list")
}
