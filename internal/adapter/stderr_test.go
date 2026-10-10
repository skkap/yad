package adapter

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStderrTail(t *testing.T) {
	const token = "sk-ant-oat01-abcdefghijklmnop"
	for _, tc := range []struct {
		name      string
		stderr    string
		env       []string
		truncated bool
		want      string
	}{
		{"nothing", "", nil, false, ""},
		{"only blank lines", "\n  \n\t\n", nil, false, ""},
		{"one line", "Error: boom\n", nil, false, "Error: boom"},
		{"the last five non-empty lines, in order",
			"one\ntwo\n\nthree\nfour\n  five  \nsix\r\nseven\n", nil, false,
			"three | four | five | six | seven"},
		{"a value yad put in the environment is replaced",
			"Error: token " + token + " was refused\n",
			[]string{"CLAUDE_CODE_OAUTH_TOKEN=" + token}, false,
			"Error: token " + Redacted + " was refused"},
		{"every occurrence, of every value",
			"a " + token + " b /tmp/grant-file-xyz c " + token + "\n",
			[]string{"CLAUDE_CODE_OAUTH_TOKEN=" + token, "DEPLOY_KEY=/tmp/grant-file-xyz"}, false,
			"a " + Redacted + " b " + Redacted + " c " + Redacted},
		{"a value inside another does not shield the longer one",
			"fetching https://deploy.example?token=sk-deploy-0123456789\n",
			[]string{"SERVICE_URL=https://deploy.example", "DEPLOY_URL=https://deploy.example?token=sk-deploy-0123456789"}, false,
			"fetching " + Redacted},
		{"a value too short to be a credential is left alone",
			"exit 1 after 1 try\n", []string{"RETRIES=1"}, false,
			"exit 1 after 1 try"},
		{"a multi-line value cut part-way loses its whole lines too",
			"ABCDEFGHIJKLMNOP\nQRSTUVWXYZ012345\n-----END PRIVATE KEY-----\nError: bad key\n",
			[]string{"KEY=-----BEGIN PRIVATE KEY-----\nzyxwvutsrqponmlk\nABCDEFGHIJKLMNOP\nQRSTUVWXYZ012345\n-----END PRIVATE KEY-----"}, false,
			Redacted + " | " + Redacted + " | " + Redacted + " | Error: bad key"},
		{"a cut tail loses its first line, which may hold part of a credential",
			"oat01-abcdefghijklmnop leaked\nError: boom\n", []string{"T=" + token}, true,
			"Error: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StderrTail(tc.stderr, tc.env, tc.truncated); got != tc.want {
				t.Errorf("StderrTail = %q, want %q", got, tc.want)
			}
		})
	}
}

// A harness that writes one enormous line is held to StderrBytes, from the
// end, without splitting a rune.
func TestStderrTailIsBounded(t *testing.T) {
	long := strings.Repeat("é", StderrBytes) + " the reason"
	got := StderrTail(long, nil, false)
	if len(got) > StderrBytes+len("…") || !strings.HasSuffix(got, " the reason") || !strings.HasPrefix(got, "…") {
		t.Errorf("tail of %d bytes: %q…", len(got), got[:20])
	}
	if !utf8.ValidString(got) {
		t.Error("the tail split a rune")
	}
}
