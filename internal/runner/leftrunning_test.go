package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	hubdb "github.com/skkap/yad/internal/hub/store/db"
	"github.com/skkap/yad/internal/supervise"
)

// A result tells a run whose tool work was measured and found nothing apart
// from one that was not measured at all, and names what was found in a
// status event the result's last_seq covers (DEV-186).
func TestLeftRunningIsReportedApartFromNotMeasured(t *testing.T) {
	sleep := supervise.Leftover{PID: 4242, Name: "sleep", Age: 4 * time.Second}
	node := supervise.Leftover{PID: 4343, Name: "node", Age: 3*time.Minute + 10*time.Second}
	for _, tc := range []struct {
		name  string
		left  *[]supervise.Leftover
		want  *int
		event string
	}{
		{"not measured", nil, nil, ""},
		{"measured, nothing found", &[]supervise.Leftover{}, ptr(0), ""},
		{"one left", &[]supervise.Leftover{sleep}, ptr(1),
			"1 process the run's tool work started was still running when the turn ended: sleep (running 4s)"},
		{"two left", &[]supervise.Leftover{sleep, node}, ptr(2),
			"2 processes the run's tool work started were still running when the turn ended: sleep (running 4s), node (running 3m10s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			e.enqueue(t, testRun("a", "s1"))
			x := e.executor(fakeHarness(fake.Script{
				Events:  []v1.Event{{Kind: v1.EventText, Text: "tests are running, will report"}},
				Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "tests are running, will report", LeftRunning: tc.left},
			}))
			claimAndRun(t, l, x)
			res, ok := outboxResult(t, e, "a")
			if !ok {
				t.Fatal("no result")
			}
			if got := res.Metrics.LeftRunning; (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Errorf("left_running = %v; want %v", show(got), show(tc.want))
			}
			e.reporter(l).Flush(context.Background())
			evs, err := e.hubStore.EventsAfter(context.Background(), hubdb.EventsAfterParams{RunID: "a", Seq: 0, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			var status []v1.Event
			for _, row := range evs {
				var ev v1.Event
				if err := json.Unmarshal([]byte(row.Body), &ev); err != nil {
					t.Fatal(err)
				}
				if ev.Status == "left_running" {
					status = append(status, ev)
				}
			}
			switch {
			case tc.event == "" && len(status) != 0:
				t.Errorf("a left_running event with nothing left: %+v", status)
			case tc.event != "" && (len(status) != 1 || status[0].Text != tc.event || status[0].Kind != v1.EventStatus):
				t.Errorf("left_running events %+v; want one saying %q", status, tc.event)
			case tc.event != "" && res.LastSeq != status[0].Seq:
				t.Errorf("last_seq %d does not cover the left_running event at %d", res.LastSeq, status[0].Seq)
			}
			for _, ev := range status {
				for _, l := range *tc.left {
					if strings.Contains(ev.Text, strconv.Itoa(l.PID)) {
						t.Errorf("the event names pid %d: %q", l.PID, ev.Text)
					}
				}
			}
		})
	}
}

// Every turn measured adds to the run's figure, whatever came between them,
// and the figure survives the round trip a park makes through the run's row.
func TestLeftRunningIsSummedOverTheTurnsMeasured(t *testing.T) {
	var p progress
	one := []supervise.Leftover{{Name: "sleep"}}
	two := []supervise.Leftover{{Name: "node"}, {Name: "esbuild"}}
	steps := []struct {
		left *[]supervise.Leftover
		want *int
	}{
		{nil, nil},
		{&[]supervise.Leftover{}, ptr(0)},
		{&one, ptr(1)},
		{nil, ptr(1)},
		{&two, ptr(3)},
	}
	for i, s := range steps {
		p.absorb(adapter.Outcome{LeftRunning: s.left}, watch{}, time.Now(), time.Now())
		got := p.metrics(time.Now()).LeftRunning
		if (got == nil) != (s.want == nil) || got != nil && *got != *s.want {
			t.Fatalf("after turn %d: left_running = %v; want %v", i+1, show(got), show(s.want))
		}
		b, err := json.Marshal(p.spent)
		if err != nil {
			t.Fatal(err)
		}
		var back spent
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if g := back.LeftRunning; (g == nil) != (s.want == nil) || g != nil && *g != *s.want {
			t.Fatalf("after turn %d, through the row: %v; want %v", i+1, show(g), show(s.want))
		}
	}
}

func TestLeftRunningTextNamesTwentyAndCountsTheRest(t *testing.T) {
	var left []supervise.Leftover
	for i := range 25 {
		left = append(left, supervise.Leftover{PID: 1000 + i, Name: fmt.Sprintf("w%d", i), Age: time.Second})
	}
	got := leftRunningText(left, false)
	if !strings.HasPrefix(got, "25 processes ") || !strings.Contains(got, "w19 (running 1s), and 5 more") || strings.Contains(got, "w20") {
		t.Errorf("text = %q", got)
	}
}

// With the owner's stop_left_running the event ends by saying what yad
// stopped; without, it says nothing of stopping (decision 0086).
func TestLeftRunningTextSaysWhetherYadStoppedThem(t *testing.T) {
	sleep := supervise.Leftover{PID: 4242, Name: "sleep", Age: 4 * time.Second}
	node := supervise.Leftover{PID: 4343, Name: "node", Age: time.Second}
	stopped := func(l supervise.Leftover) supervise.Leftover { l.Stopped = true; return l }
	const one = "1 process the run's tool work started was still running when the turn ended: sleep (running 4s)"
	const two = "2 processes the run's tool work started were still running when the turn ended: sleep (running 4s), node (running 1s)"
	for _, tc := range []struct {
		name string
		left []supervise.Leftover
		stop bool
		want string
	}{
		{"off", []supervise.Leftover{sleep}, false, one},
		{"off, whatever a leftover says", []supervise.Leftover{stopped(sleep)}, false, one},
		{"it", []supervise.Leftover{stopped(sleep)}, true, one + "; yad stopped it"},
		{"them", []supervise.Leftover{stopped(sleep), stopped(node)}, true, two + "; yad stopped them"},
		{"not it", []supervise.Leftover{sleep}, true, one + "; yad could not stop it"},
		{"not them", []supervise.Leftover{sleep, node}, true, two + "; yad could not stop them"},
		{"some", []supervise.Leftover{stopped(sleep), node}, true, two + "; yad stopped 1 of them and could not stop 1"},
	} {
		if got := leftRunningText(tc.left, tc.stop); got != tc.want {
			t.Errorf("%s: text = %q; want %q", tc.name, got, tc.want)
		}
	}
	if got := leftRunningLog([]supervise.Leftover{stopped(sleep), node}, true); got != "sleep[4242 stopped] node[4343 not stopped]" {
		t.Errorf("log = %q", got)
	}
	if got := leftRunningLog([]supervise.Leftover{sleep}, false); got != "sleep[4242]" {
		t.Errorf("log, off = %q", got)
	}
}

func ptr(n int) *int { return &n }

func show(n *int) string {
	if n == nil {
		return "absent"
	}
	return strconv.Itoa(*n)
}
