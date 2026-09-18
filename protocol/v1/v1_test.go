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
