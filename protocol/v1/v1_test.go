package v1

import (
	"bufio"
	"bytes"
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
	failed, exit := false, 0 // both zero values, which must survive: absent is another answer
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
				Harness: "claude", Model: "opus", Effort: "high",
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
		{"event batch", EventBatch{Events: []Event{{Seq: 1, At: at, Kind: EventToolCall, Tool: &ToolEvent{ID: "t", Name: "Bash", Input: "ls"}},
			{Seq: 2, At: at, Kind: EventToolResult, Tool: &ToolEvent{ID: "t", Output: "x", IsError: &failed, ExitCode: &exit}}}}, &EventBatch{}},
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
	var events, states, accounts []string
	for _, k := range EventKinds() {
		events = append(events, string(k))
	}
	for _, s := range RunStates() {
		states = append(states, string(s))
	}
	for _, s := range AccountStates() {
		accounts = append(accounts, string(s))
	}
	check("Event", events)
	check("Run state", states)
	check("Account", accounts)
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
	var held, terminal, events, controls, accounts, reasons []string
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
	for _, s := range AccountStates() {
		accounts = append(accounts, string(s))
	}
	for _, r := range SessionCloseReasons() {
		reasons = append(reasons, string(r))
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
		{AccountReport{}, "State", accounts},
		{ClosedSession{}, "Reason", reasons},
		{Grant{}, "As", []string{string(GrantEnv), string(GrantFile)}},
		{SessionRef{}, "Mode", []string{string(SessionPerRun), string(SessionLive)}},
		{LoginReport{}, "Method", strs(LoginMethods())},
		{LoginReport{}, "State", strs(LoginStates())},
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

func strs[S ~string](xs []S) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = string(x)
	}
	return out
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
		// Long enough to push Claude's warning quoting it out of the stderr
		// the runner keeps, so its refusal would go unseen.
		{"effort longer than a level", func(r *Run) { r.Effort = strings.Repeat("x", 65) }},
		{"effort with a space", func(r *Run) { r.Effort = "high please" }},
		{"effort with a newline", func(r *Run) { r.Effort = "high\nmax" }},
		{"effort with a quote", func(r *Run) { r.Effort = "hi'gh" }},
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
	// Validity is the shape only: a level no harness has is the harness's to
	// refuse.
	for _, effort := range []string{"low", "xhigh", "ultra", "some_level-2", strings.Repeat("x", 64)} {
		r := good
		r.Effort = effort
		if err := r.Validate(); err != nil {
			t.Errorf("effort %q refused: %v", effort, err)
		}
	}
	for _, src := range []Source{{Git: &GitSource{URL: "u"}}, {Path: "/p"}} {
		if err := src.Validate(); err != nil {
			t.Errorf("valid source %+v refused: %v", src, err)
		}
	}
}

// Any valid environment variable name is a grant name (decision 0038), except
// four that would break the run and the ones that would move it off its
// account (0040), each refused whatever its case.
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
		{"ANTHROPIC_MODEL", ""}, {"CLAUDE_CODE_MAX_OUTPUT_TOKENS", ""}, {"CODEX_SANDBOX", ""},
		{"OPENAI_ORG_ID", ""}, {"GIT_SSH_COMMAND", ""}, {"NODE_OPTIONS", ""},
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
		// The account names are matched whole too: a project's own key under
		// another name is exactly what the refusal asks a hub to send.
		{"MY_ANTHROPIC_API_KEY", ""}, {"ANTHROPIC_API_KEY_TESTS", ""}, {"OPENAI_API_KEY_2", ""},
		{"CODEX_HOME_DIR", ""}, {"CLAUDE_CONFIG", ""}, {"CLAUDE_CODE_USER", ""},
		// Inert behind a refused switch or pair (see accountGrantNames), and
		// a project's cloud deploy may need them.
		{"ANTHROPIC_BEDROCK_BASE_URL", ""}, {"ANTHROPIC_AWS_API_KEY", ""}, {"ANTHROPIC_FOUNDRY_API_KEY", ""},
		{"ANTHROPIC_IDENTITY_TOKEN_FILE", ""}, {"ANTHROPIC_WORKSPACE_ID", ""},

		// Not an environment variable name, or not a plain file name. The
		// pattern is ASCII, so a Cyrillic lookalike of PATH is not a name at
		// all — it never reaches the deny list.
		{"", "environment variable name"}, {"1_TOKEN", "environment variable name"},
		{"../X_TOKEN", "environment variable name"}, {"a/b_TOKEN", "environment variable name"},
		{"X_TOKEN.json", "environment variable name"}, {"X-TOKEN", "environment variable name"},
		{"X TOKEN", "environment variable name"}, {"X=Y", "environment variable name"},
		{"X\x00Y", "environment variable name"}, {"\u0420\u0410\u0422\u041d", "environment variable name"},

		// The four, in every case. Not because "path" would break a run —
		// it arrives as "path" and leaves PATH alone — but because the list
		// catches a hub's mistake, and a hub author whose platform folds
		// environment case writes "Path" meaning PATH (see grant.go).
		{"PATH", "PATH"}, {"path", "PATH"}, {"Path", "PATH"},
		{"HOME", "HOME"}, {"home", "HOME"}, {"hOmE", "HOME"},
		{"LD_PRELOAD", "LD_"}, {"ld_preload", "LD_"}, {"Ld_Library_Path", "LD_"}, {"LD_", "LD_"},
		{"DYLD_INSERT_LIBRARIES", "DYLD_"}, {"dyld_insert_libraries", "DYLD_"},

		// The variables that choose whose credential a harness uses, or which
		// home it logs in from. A hub's grant lands after the owner's copy was
		// scrubbed and wins, so each would run the turn on a credential the
		// account layer knows nothing about (0040). The refusal names the
		// decision and the way round it.
		{"ANTHROPIC_API_KEY", "0040"}, {"anthropic_api_key", "0040"}, {"ANTHROPIC_AUTH_TOKEN", "0040"},
		{"CLAUDE_CODE_OAUTH_TOKEN", "0040"}, {"CLAUDE_CONFIG_DIR", "0040"}, {"Claude_Config_Dir", "0040"},
		{"ANTHROPIC_BASE_URL", "0040"}, {"ANTHROPIC_CUSTOM_HEADERS", "0040"},
		{"ANTHROPIC_PROFILE", "0040"}, {"ANTHROPIC_FEDERATION_RULE_ID", "0040"},
		{"ANTHROPIC_ORGANIZATION_ID", "0040"}, {"ANTHROPIC_CONFIG_DIR", "0040"},
		{"CODEX_HOME", "0040"}, {"codex_home", "0040"}, {"OPENAI_API_KEY", "0040"},
		{"CODEX_API_KEY", "0040"}, {"CODEX_ACCESS_TOKEN", "0040"}, {"OPENAI_BASE_URL", "0040"},
		{"CODEX_REFRESH_TOKEN_URL_OVERRIDE", "0040"}, {"AWS_BEARER_TOKEN_BEDROCK", "0040"},
		// Every provider switch, the ones Claude has shipped and the next one:
		// behind them, a provider's own keys move nothing.
		{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_*"}, {"CLAUDE_CODE_USE_VERTEX", "0040"},
		{"CLAUDE_CODE_USE_FOUNDRY", "0040"}, {"CLAUDE_CODE_USE_ANTHROPIC_AWS", "0040"},
		{"claude_code_use_mantle", "0040"}, {"CLAUDE_CODE_USE_", "0040"},
		{"ANTHROPIC_API_KEY", "under another name"}, {"CODEX_HOME", "yad account add"},
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

// A runner built before AccountReport.state existed sends accounts as
// {"label": "work"}. yad hub validates request bodies against the generated
// schema, so a state listed in required would give that runner a 422 and it
// could not register at all — and hubs upgrade centrally while runners sit on
// other people's machines.
//
// This pins the compatibility rather than the tag: removing `omitempty` and
// regenerating puts state back in required, and nothing else in the suite
// would notice.
func TestAccountReportStaysOptionalForOlderRunners(t *testing.T) {
	doc, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The schema block for AccountReport, up to the next component.
	const head = "    AccountReport:\n"
	i := bytes.Index(doc, []byte(head))
	if i < 0 {
		t.Fatal("openapi.yaml has no AccountReport schema")
	}
	block := doc[i+len(head):]
	if j := bytes.Index(block, []byte("\n    Ack:")); j >= 0 {
		block = block[:j]
	}
	at := bytes.Index(block, []byte("required:"))
	if at < 0 {
		t.Fatalf("AccountReport has no required list at all:\n%s", block)
	}
	req := block[at:]
	if bytes.Contains(req, []byte("- state")) {
		t.Errorf("AccountReport requires state, so a runner from before the field cannot register:\n%s", req)
	}
	if !bytes.Contains(req, []byte("- label")) {
		t.Errorf("AccountReport no longer requires label:\n%s", req)
	}

	// Optional in the schema still means constrained when present: the enum
	// has to be published, or a hub gains no way to reject a value outside
	// the set.
	if !bytes.Contains(block, []byte("- needs_login")) {
		t.Errorf("AccountReport.state publishes no enum:\n%s", block)
	}
	// And an older runner's account object, which carries no state at all,
	// still decodes.
	var rep AccountReport
	if err := json.Unmarshal([]byte(`{"label":"work"}`), &rep); err != nil {
		t.Fatalf("an older runner's account object no longer decodes: %v", err)
	}
	if rep.Label != "work" || rep.State != "" {
		t.Errorf("decoded %+v", rep)
	}
}
