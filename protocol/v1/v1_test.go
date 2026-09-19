package v1

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A sync response survives a JSON round trip unchanged — the cheapest guard
// against a tag typo that would silently drop a field on the wire.
func TestRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cost := 0.42
	for _, tc := range []struct {
		name string
		v    any
		into any
	}{
		{"sync response", SyncResponse{
			NextSyncMS: 15000, LeaseMS: 60000,
			Runs: []Run{{
				RunID: "r1", Session: SessionRef{ID: "s1", New: true, Mode: SessionPerRun},
				Harness: "claude", Model: "opus",
				Brief:   Brief{Context: "ctx", Instruction: "do it"},
				Sources: []Source{{Git: &GitSource{URL: "git@github.com:a/b", Base: "master", Branch: "yad/r1"}}, {Path: "/srv/home"}},
				Grants:  []Grant{{Name: "ZUMINO_TOKEN", Value: "x", As: GrantEnv}},
				StartAt: &at, MaxWaitMS: 1, WallClockMS: 2, InactivityMS: 3,
			}},
			Controls: []Control{{Kind: ControlSteer, RunID: "r1", Text: "also"}},
		}, &SyncResponse{}},
		{"result", Result{
			State: RunSucceeded, FinalText: "done",
			Usage:   RunUsage{ByModel: map[string]Usage{"opus": {Input: 1, Output: 2, CostUSD: &cost}}},
			Metrics: Metrics{DurationMS: 9, ToolCalls: 3}, LastSeq: 7,
		}, &Result{}},
		{"event batch", EventBatch{Events: []Event{{Seq: 1, At: at, Kind: EventToolCall, Tool: &ToolEvent{ID: "t", Name: "Bash", Input: "ls"}}}}, &EventBatch{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, tc.into); err != nil {
				t.Fatal(err)
			}
			if got := reflect.ValueOf(tc.into).Elem().Interface(); !reflect.DeepEqual(got, tc.v) {
				t.Errorf("round trip changed the value:\n got %#v\nwant %#v", got, tc.v)
			}
		})
	}
}

func TestTerminalStates(t *testing.T) {
	terminal := map[RunState]bool{RunSucceeded: true, RunFailed: true, RunCancelled: true, RunTimedOut: true, RunLost: true}
	for _, s := range RunStates() {
		if s.IsTerminal() != terminal[s] {
			t.Errorf("%s.IsTerminal() = %v", s, s.IsTerminal())
		}
		if !s.Valid() {
			t.Errorf("%s not Valid", s)
		}
	}
	if RunState("paused").Valid() {
		t.Error("an unknown state is Valid")
	}
}

// DOMAIN.md owns the vocabulary, and a closed set written there is also an enum
// here and a set of values every hub stores. This test is what keeps the three
// from drifting apart: change one, and it names the other.
func TestKindsMatchDomain(t *testing.T) {
	kinds := domainKinds(t, "../../DOMAIN.md")
	check := func(entry string, got []string) {
		t.Helper()
		want, ok := kinds[entry]
		if !ok {
			t.Fatalf("DOMAIN.md has no _Kinds_ line under **%s**", entry)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("**%s** kinds: DOMAIN.md says %v, protocol/v1 has %v", entry, want, got)
		}
	}
	var events, states []string
	for _, k := range EventKinds() {
		events = append(events, string(k))
	}
	for _, s := range RunStates() {
		states = append(states, string(s))
	}
	check("Event", events)
	check("Run state", states)
	check("Session", []string{string(SessionPerRun), string(SessionLive)})
}

// domainKinds maps each entry headword to the values on its _Kinds_ line.
func domainKinds(t *testing.T, path string) map[string][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][]string{}
	var entry string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "**") {
			if end := strings.Index(line[2:], "**"); end > 0 {
				entry = line[2 : 2+end]
			}
		}
		if rest, ok := strings.CutPrefix(line, "_Kinds_:"); ok && entry != "" {
			var vals []string
			for _, v := range strings.Split(rest, "|") {
				vals = append(vals, strings.Fields(v)[0])
			}
			out[entry] = vals
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The enum tags are what the OpenAPI document publishes; they must be exactly
// the Go sets — non-terminal states where a run is held, terminal ones where a
// result is reported.
func TestEnumTagsMatchSets(t *testing.T) {
	var held, terminal, events, controls []string
	for _, s := range RunStates() {
		if s.IsTerminal() {
			terminal = append(terminal, string(s))
		} else {
			held = append(held, string(s))
		}
	}
	for _, k := range EventKinds() {
		events = append(events, string(k))
	}
	for _, k := range ControlKinds() {
		controls = append(controls, string(k))
	}
	for _, tc := range []struct {
		v     any
		field string
		want  []string
	}{
		{HeldRun{}, "State", held},
		{Result{}, "State", terminal},
		{Event{}, "Kind", events},
		{Control{}, "Kind", controls},
		{Grant{}, "As", []string{string(GrantEnv), string(GrantFile)}},
		{SessionRef{}, "Mode", []string{string(SessionPerRun), string(SessionLive)}},
	} {
		f, ok := reflect.TypeOf(tc.v).FieldByName(tc.field)
		if !ok {
			t.Fatalf("%T has no field %s", tc.v, tc.field)
		}
		if got := strings.Split(f.Tag.Get("enum"), ","); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%T.%s enum tag = %v, want %v", tc.v, tc.field, got, tc.want)
		}
	}
}

func TestRunValidate(t *testing.T) {
	good := Run{RunID: "r", Session: SessionRef{ID: "s", New: true}, Harness: "claude", Model: "opus", Brief: Brief{Instruction: "hi"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid run refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Run)
	}{
		{"no run id", func(r *Run) { r.RunID = "" }},
		{"no session", func(r *Run) { r.Session.ID = "" }},
		{"no harness", func(r *Run) { r.Harness = "" }},
		{"no model", func(r *Run) { r.Model = "" }},
		{"no instruction", func(r *Run) { r.Brief.Instruction = "" }},
		{"unknown session mode", func(r *Run) { r.Session.Mode = "warm" }},
		{"empty source", func(r *Run) { r.Sources = []Source{{}} }},
		{"source with both", func(r *Run) { r.Sources = []Source{{Git: &GitSource{URL: "u"}, Path: "/p"}} }},
		{"git source without url", func(r *Run) { r.Sources = []Source{{Git: &GitSource{}}} }},
		{"grant without name", func(r *Run) { r.Grants = []Grant{{As: GrantEnv}} }},
		{"grant delivered by argv", func(r *Run) { r.Grants = []Grant{{Name: "ZUMINO_TOKEN", As: "argv"}} }},
		{"grant that sets the loader", func(r *Run) { r.Grants = []Grant{{Name: "LD_PRELOAD", As: GrantEnv}} }},
		{"grant given twice", func(r *Run) {
			r.Grants = []Grant{{Name: "ZUMINO_TOKEN", As: GrantEnv}, {Name: "ZUMINO_TOKEN", As: GrantFile}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := good
			tc.edit(&r)
			if r.Validate() == nil {
				t.Error("accepted")
			}
		})
	}
	for _, src := range []Source{{Git: &GitSource{URL: "u"}}, {Path: "/p"}} {
		if err := src.Validate(); err != nil {
			t.Errorf("valid source %+v refused: %v", src, err)
		}
	}
}

// Any valid environment variable name is a grant name (decision 0038); the
// four that remain are refused because they would break the run, and they are
// refused whatever their case.
func TestGrantNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string // "" when allowed, else a word the refusal must contain
	}{
		// The names 0024 asked for still pass, and so do the ones it cost the
		// owner: a database URL, a cloud deploy credential, a harness's own
		// settings. Filtering these never protected the machine — the brief
		// could ask the harness for the same thing (0038).
		{"ZUMINO_TOKEN", ""}, {"DATABASE_URL", ""}, {"GH_TOKEN", ""}, {"DEPLOY_KEY", ""},
		{"AWS_SECRET_ACCESS_KEY", ""}, {"GOOGLE_APPLICATION_CREDENTIALS", ""}, {"AZURE_OPENAI_API_KEY", ""},
		{"ANTHROPIC_BASE_URL", ""}, {"ANTHROPIC_API_KEY", ""}, {"CLAUDE_CONFIG_DIR", ""},
		{"CODEX_HOME", ""}, {"OPENAI_BASE_URL", ""}, {"GIT_SSH_COMMAND", ""}, {"NODE_OPTIONS", ""},
		{"NPM_CONFIG__AUTH_TOKEN", ""}, {"BUN_AUTH_TOKEN", ""}, {"YAD_TOKEN", ""},
		{"IS_SANDBOX", ""}, {"SHELL", ""}, {"TMPDIR", ""}, {"BASH_ENV", ""}, {"ENV", ""},
		{"HTTPS_PROXY", ""}, {"NODE_EXTRA_CA_CERTS", ""}, {"SHELLOPTS", ""}, {"PS4", ""},
		// Any valid name: lower case, mixed case, a digit, a leading
		// underscore, and one that is only an underscore.
		{"npm_token", ""}, {"http_proxy", ""}, {"Zumino_Token", ""}, {"A1", ""}, {"_X", ""}, {"_", ""},
		// Near the deny list, and outside it: a prefix is a prefix, not a
		// substring, and the two names are matched whole.
		{"PATHX", ""}, {"MY_PATH", ""}, {"HOMEBREW_TOKEN", ""}, {"LDAP_PASSWORD", ""},
		{"LD", ""}, {"DYLDX", ""},

		// Not an environment variable name, or not a plain file name. The
		// pattern is ASCII, so a Cyrillic lookalike of PATH is not a name at
		// all — it never reaches the deny list.
		{"", "environment variable name"}, {"1_TOKEN", "environment variable name"},
		{"../X_TOKEN", "environment variable name"}, {"a/b_TOKEN", "environment variable name"},
		{"X_TOKEN.json", "environment variable name"}, {"X-TOKEN", "environment variable name"},
		{"X TOKEN", "environment variable name"}, {"X=Y", "environment variable name"},
		{"X\x00Y", "environment variable name"}, {"\u0420\u0410\u0422\u041d", "environment variable name"},

		// The four, in every case: a file grant's name is a file name, and
		// macOS folds case, so "path" is the same mistake as PATH.
		{"PATH", "PATH"}, {"path", "PATH"}, {"Path", "PATH"},
		{"HOME", "HOME"}, {"home", "HOME"}, {"hOmE", "HOME"},
		{"LD_PRELOAD", "LD_"}, {"ld_preload", "LD_"}, {"Ld_Library_Path", "LD_"}, {"LD_", "LD_"},
		{"DYLD_INSERT_LIBRARIES", "DYLD_"}, {"dyld_insert_libraries", "DYLD_"},
	} {
		for _, as := range []GrantDelivery{GrantEnv, GrantFile} {
			err := Grant{Name: tc.name, Value: "v", As: as}.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("%s as %s refused: %v", tc.name, as, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("%s as %s: err %v, want one mentioning %q", tc.name, as, err, tc.want)
			}
		}
	}
}

// Two file grants whose names differ only by case are one file where the
// filesystem folds case, and the second would silently overwrite the first.
// Two env grants by those names are two variables, and both arrive: the
// environment and the filesystem have different rules.
func TestGrantNamesThatCollideAsFiles(t *testing.T) {
	run := Run{RunID: "r", Session: SessionRef{ID: "s"}, Harness: "claude", Model: "opus", Brief: Brief{Instruction: "hi"}}
	for _, tc := range []struct {
		name   string
		grants []Grant
		ok     bool
	}{
		{"two file grants differing by case", []Grant{{Name: "DEPLOY_KEY", As: GrantFile}, {Name: "deploy_key", As: GrantFile}}, false},
		{"two env grants differing by case", []Grant{{Name: "DEPLOY_KEY", As: GrantEnv}, {Name: "deploy_key", As: GrantEnv}}, true},
		{"one of each", []Grant{{Name: "DEPLOY_KEY", As: GrantEnv}, {Name: "deploy_key", As: GrantFile}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run
			r.Grants = tc.grants
			err := r.Validate()
			if tc.ok != (err == nil) {
				t.Errorf("err = %v, want ok = %v", err, tc.ok)
			}
		})
	}
}
