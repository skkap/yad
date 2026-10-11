package supervise

import (
	"os"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// What the owner's [env] may and may not set. A refusal names the rule and
// the way round it; an ordinary machine variable passes.
func TestOwnerRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		// want is a word the reason must carry; "" means allowed.
		want string
	}{
		{"ANTHROPIC_API_KEY", "0060"},
		{"anthropic_api_key", "0060"},
		{"OPENAI_API_KEY", "add an account"},
		{"CLAUDE_CODE_OAUTH_TOKEN", "0040"},
		{"CLAUDE_CONFIG_DIR", "where the runner starts"},
		{"CODEX_HOME", "where the runner starts"},
		{"CLAUDE_SECURESTORAGE_CONFIG_DIR", "where the runner starts"},
		{"CLAUDECODE", "settings.json"},
		{"CLAUDE_CODE_FOO", "settings.json"},
		{"YAD_PROFILE", "yad's own configuration"},
		{"IS_SANDBOX", "root check"},
		{"PATH", "install the service again"},
		{"HOME", "configuration and credentials"},
		{"WT_SLOT", "setup hook"},
		{"DOCKER_HOST", ""},
		{"BASH_MAX_TIMEOUT_MS", ""},
		{"TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", ""},
		{"ANTHROPIC_MODEL", ""},
		// Environments are case-sensitive here: "path" is not PATH.
		{"path", ""},
	} {
		got := OwnerRefusal(tc.name)
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%s is refused (%s), want it allowed", tc.name, got)
		case tc.want != "" && !strings.Contains(got, tc.want):
			t.Errorf("%s: refusal %q, want one mentioning %q", tc.name, got, tc.want)
		case tc.want != "" && !strings.Contains(got, " — "):
			t.Errorf("%s: refusal %q gives no way round it", tc.name, got)
		}
	}
}

// Every name Scrub removes, and every name a grant is refused for choosing a
// credential, is refused in [env] too: a declared value the scrub then
// removed would be a setting that silently does nothing. The names are read
// from the grant list's own source, so one added there is checked here
// without anyone remembering to.
func TestOwnerRefusalCoversTheScrub(t *testing.T) {
	src, err := os.ReadFile("../../protocol/v1/grant.go")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"CLAUDECODE", "CLAUDE_CODE_X", "YAD_X", "YAD_CONFIG_DIR"}
	for _, m := range regexp.MustCompile(`"([A-Z][A-Z0-9_]*)"`).FindAllStringSubmatch(string(src), -1) {
		n := m[1]
		if strings.HasSuffix(n, "_") {
			n += "X" // a prefix entry
		}
		names = append(names, n)
	}
	for _, kv := range os.Environ() {
		n, _, _ := strings.Cut(kv, "=")
		names = append(names, n)
	}
	checked := 0
	for _, n := range names {
		_, account := v1.AccountVariable(n)
		if len(Scrub([]string{n + "=v"}, nil)) == 0 || account {
			checked++
			if OwnerRefusal(n) == "" {
				t.Errorf("%s is removed by the scrub or chooses a credential, and [env] may set it", n)
			}
		}
	}
	if checked < 20 {
		t.Fatalf("only %d names checked: the grant list's source no longer reads as this test expects", checked)
	}
}

// The layer is kept in name order, cleared by nil, and handed out as a copy.
func TestSetOwnerEnv(t *testing.T) {
	t.Cleanup(func() { SetOwnerEnv(nil) })
	SetOwnerEnv(map[string]string{"B": "2", "A": "1"})
	got := OwnerEnv()
	if strings.Join(got, ",") != "A=1,B=2" {
		t.Errorf("OwnerEnv = %v", got)
	}
	got[0] = "A=changed"
	if OwnerEnv()[0] != "A=1" || !Declared("A") || Declared("C") || Declared("a") {
		t.Errorf("OwnerEnv after a caller's change = %v", OwnerEnv())
	}
	SetOwnerEnv(nil)
	if len(OwnerEnv()) != 0 || Declared("A") {
		t.Errorf("OwnerEnv after a clear = %v", OwnerEnv())
	}
}
