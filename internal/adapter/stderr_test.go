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
		{"values that overlap end to start are both masked",
			"GET https://artifacts.example.com/downloads/build?token=sk-deploy-0123456789 failed\n",
			[]string{"SERVICE_URL=https://artifacts.example.com/downloads/", "DOWNLOAD_PATH=/build?token=sk-deploy-0123456789"}, false,
			"GET " + Redacted + " failed"},
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
			if got := StderrTail(tc.stderr, tc.env, nil, tc.truncated); got != tc.want {
				t.Errorf("StderrTail = %q, want %q", got, tc.want)
			}
		})
	}
}

// What a file grant holds is in no variable — the environment has only its
// path — and is masked under the same rules as an environment value.
func TestStderrTailMasksSecretsOutsideTheEnvironment(t *testing.T) {
	const pem = "-----BEGIN PRIVATE KEY-----\nzyxwvutsrqponmlk\nABCDEFGHIJKLMNOP\nQRSTUVWXYZ012345\n-----END PRIVATE KEY-----\n"
	for _, tc := range []struct {
		name      string
		stderr    string
		secrets   []string
		truncated bool
		want      string
	}{
		{"a file's value is replaced",
			"the credential sk-test-file-value-0123456789 was refused\n",
			[]string{"sk-test-file-value-0123456789"}, false,
			"the credential " + Redacted + " was refused"},
		{"without the newline the file ends in",
			"the credential sk-test-file-value-0123456789 was refused\n",
			[]string{"sk-test-file-value-0123456789\n"}, false,
			"the credential " + Redacted + " was refused"},
		{"a multi-line file is masked line by line",
			"ABCDEFGHIJKLMNOP\nQRSTUVWXYZ012345\n-----END PRIVATE KEY-----\nError: bad key\n",
			[]string{pem}, false,
			Redacted + " | " + Redacted + " | " + Redacted + " | Error: bad key"},
		{"a line too short to be a credential is left alone",
			"short1\nError: bad key\n",
			[]string{"short1\nlong-enough-line\n"}, false,
			"short1 | Error: bad key"},
		{"a value too short to be a credential is left alone",
			"exit 1 after 1 try\n", []string{"1"}, false,
			"exit 1 after 1 try"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StderrTail(tc.stderr, nil, tc.secrets, tc.truncated); got != tc.want {
				t.Errorf("StderrTail = %q, want %q", got, tc.want)
			}
		})
	}
}

// A harness that writes one enormous line is held to StderrBytes, from the
// end, without splitting a rune.
func TestStderrTailIsBounded(t *testing.T) {
	long := strings.Repeat("é", StderrBytes) + " the reason"
	got := StderrTail(long, nil, nil, false)
	if len(got) > StderrBytes+len("…") || !strings.HasSuffix(got, " the reason") || !strings.HasPrefix(got, "…") {
		t.Errorf("tail of %d bytes: %q…", len(got), got[:20])
	}
	if !utf8.ValidString(got) {
		t.Error("the tail split a rune")
	}
}
