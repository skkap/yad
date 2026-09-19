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
	var held, terminal, events, controls, accounts []string
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

// Every class a grant name can fall in, each refused for its own reason.
func TestGrantNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string // "" when allowed, else a word the refusal must contain
	}{
		{"ZUMINO_TOKEN", ""}, {"GH_TOKEN", ""}, {"DEPLOY_KEY", ""}, {"DB_PASSWORD", ""},
		{"STRIPE_SECRET", ""}, {"SERVICE_CREDENTIALS", ""}, {"SERVICE_CREDENTIAL", ""}, {"_X_TOKEN", ""},

		// Not an environment variable name, or not a plain file name.
		{"", "environment variable name"}, {"zumino_token", "environment variable name"},
		{"Zumino_TOKEN", "environment variable name"}, {"1_TOKEN", "environment variable name"},
		{"../X_TOKEN", "environment variable name"}, {"a/b_TOKEN", "environment variable name"},
		{"X_TOKEN.json", "environment variable name"}, {"X-TOKEN", "environment variable name"},

		// Reserved names.
		{"PATH", "reserved"}, {"HOME", "reserved"}, {"SHELL", "reserved"}, {"TMPDIR", "reserved"},
		{"BASH_ENV", "reserved"}, {"ENV", "reserved"}, {"NODE_OPTIONS", "reserved"}, {"IS_SANDBOX", "reserved"},

		// Reserved namespaces, secret-shaped or not.
		{"LD_PRELOAD", "LD_"}, {"LD_TOKEN", "LD_"}, {"DYLD_INSERT_LIBRARIES", "DYLD_"},
		{"YAD_TOKEN", "YAD_"}, {"CLAUDE_CONFIG_DIR", "CLAUDE"}, {"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE"},
		{"ANTHROPIC_BASE_URL", "ANTHROPIC_"}, {"ANTHROPIC_API_KEY", "ANTHROPIC_"},
		{"CODEX_HOME", "CODEX_"}, {"CODEX_API_KEY", "CODEX_"},
		{"OPENAI_BASE_URL", "OPENAI_"}, {"OPENAI_API_KEY", "OPENAI_"},
		{"GIT_SSH_COMMAND", "GIT_"}, {"GIT_TOKEN", "GIT_"}, {"NODE_AUTH_TOKEN", "NODE_"},
		{"NPM_CONFIG__AUTH_TOKEN", "NPM_CONFIG_"}, {"BUN_AUTH_TOKEN", "BUN_"},
		{"AWS_BEARER_TOKEN_BEDROCK", "AWS_"}, {"AWS_SECRET_ACCESS_KEY", "AWS_"}, {"AWS_SESSION_TOKEN", "AWS_"},
		{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_"}, {"AZURE_OPENAI_API_KEY", "AZURE_"},

		// Steering variables nobody listed: refused for not being secrets.
		{"HTTPS_PROXY", "not named as a secret"}, {"HTTP_PROXY", "not named as a secret"},
		{"ALL_PROXY", "not named as a secret"}, {"NO_PROXY", "not named as a secret"},
		{"SSL_CERT_FILE", "not named as a secret"}, {"REQUESTS_CA_BUNDLE", "not named as a secret"},
		{"CURL_CA_BUNDLE", "not named as a secret"}, {"SHELLOPTS", "not named as a secret"},
		{"PS4", "not named as a secret"}, {"GCONV_PATH", "not named as a secret"},
		{"JAVA_TOOL_OPTIONS", "not named as a secret"}, {"_JAVA_OPTIONS", "not named as a secret"},
		{"TOKEN", "not named as a secret"}, {"_TOKEN", "not named as a secret"}, {"A_TOKEN_X", "not named as a secret"},
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
