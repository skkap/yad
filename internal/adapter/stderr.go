package adapter

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// The end of a harness's stderr a run's error carries. Five lines is the
// error and the few before it that say where the harness was — a hang's last
// line alone is usually a log line, not a reason (DEV-170). A KiB keeps a
// harness that writes one enormous line from filling the message.
const (
	StderrLines = 5
	StderrBytes = 1 << 10
)

// minSecret is the shortest value StderrTail replaces. Every credential is
// far longer; a shorter value — a "1" a grant sets — would blank out every
// digit in the text and hide nothing.
const minSecret = 8

// Redacted stands in for a value yad handed a harness, in its stderr.
const Redacted = "[redacted]"

// StderrTail is the end of what a harness wrote to stderr as a run's error
// may quote it (decision 0075): its last StderrLines non-empty lines, at most
// StderrBytes, joined by " | " into one line, with every value yad put in the
// harness's environment (env — the account's token, its home, the run's
// grants and their file paths) and every one of secrets (Spec.Secrets — what
// the run's file grants hold) replaced first. Those are the credentials yad
// itself handed the child, and the likeliest to come back: a harness whose
// login is refused may quote the token it was refused. Nothing of either is
// ever added to it, and stderr that held only blank lines is "".
//
// truncated says the tail began mid-stream — the supervisor keeps a bounded
// tail and its first line may start part-way through — and then that first
// line is dropped, since a credential cut at its start would no longer match
// and would go out in part.
func StderrTail(stderr string, env, secrets []string, truncated bool) string {
	stderr = Mask(stderr, env, secrets)
	lines := strings.Split(stderr, "\n")
	if truncated && len(lines) > 0 {
		lines = lines[1:]
	}
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < StderrLines; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			kept = append(kept, l)
		}
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	out := strings.Join(kept, " | ")
	if len(out) <= StderrBytes {
		return out
	}
	// From the end, as the lines were chosen: the last words are the reason.
	// After the replacement, so a cut can split "[redacted]" but never a
	// credential.
	cut := len(out) - StderrBytes
	for cut < len(out) && !utf8.RuneStart(out[cut]) {
		cut++
	}
	return "…" + out[cut:]
}

// Mask is s with every value of env and every one of secrets replaced as
// StderrTail replaces them, for anything else of a harness's stderr a run's
// error quotes — Claude's line refusing an effort.
func Mask(s string, env, secrets []string) string {
	var masked []string
	for _, kv := range env {
		if _, v, ok := strings.Cut(kv, "="); ok {
			masked = appendSecret(masked, v)
		}
	}
	for _, v := range secrets {
		masked = appendSecret(masked, v)
	}
	return mask(s, masked)
}

// appendSecret adds to secrets what Mask replaces of one value: the
// value, when it is at least minSecret long.
func appendSecret(secrets []string, v string) []string {
	if len(v) >= minSecret {
		secrets = append(secrets, v)
	}
	// Each line of a value that has several — a PEM key a grant delivers —
	// on its own too: a tail cut part-way through the value holds its later
	// lines whole and the value as a whole nowhere. A file's content that
	// ends in a newline its harness does not print is one of these.
	if strings.Contains(v, "\n") {
		for _, line := range strings.Split(v, "\n") {
			if line = strings.TrimSpace(line); len(line) >= minSecret {
				secrets = append(secrets, line)
			}
		}
	}
	return secrets
}

// mask replaces every occurrence of every secret in s with Redacted. Each
// one is found in s as it was, and overlapping or touching occurrences are
// merged into one: replacing them one after another would let the first
// replacement break a second that shares characters with it — a URL inside
// the same URL with a token in its query, or one value ending where another
// begins — and leave that one's token whole.
func mask(s string, secrets []string) string {
	type span struct{ from, to int }
	var spans []span
	for _, sec := range secrets {
		for at := 0; ; {
			i := strings.Index(s[at:], sec)
			if i < 0 {
				break
			}
			spans = append(spans, span{at + i, at + i + len(sec)})
			at += i + 1
		}
	}
	if len(spans) == 0 {
		return s
	}
	slices.SortFunc(spans, func(a, b span) int { return a.from - b.from })
	var b strings.Builder
	last := 0
	for i := 0; i < len(spans); {
		from, to := spans[i].from, spans[i].to
		for i++; i < len(spans) && spans[i].from <= to; i++ {
			to = max(to, spans[i].to)
		}
		b.WriteString(s[last:from])
		b.WriteString(Redacted)
		last = to
	}
	b.WriteString(s[last:])
	return b.String()
}
