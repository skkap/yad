package runner

import (
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

func TestPool(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 3, ByHarness: map[string]int{"codex": 1}})

	r := p.Reserve("hub")
	if got := r.Free(); got.Total != 3 || got.ByHarness["codex"] != 1 {
		t.Fatalf("free = %+v", got)
	}
	// While one sync holds the capacity, another of the same connection sees
	// none of it.
	if other := p.Reserve("hub"); other.Free().Total != 0 {
		t.Errorf("a second reservation got %d", other.Free().Total)
	}
	relCodex, ok := r.Take("codex")
	if !ok {
		t.Fatal("codex refused under its cap")
	}
	if _, ok := r.Take("codex"); ok {
		t.Error("codex taken past its cap of 1")
	}
	relClaude, ok := r.Take("claude")
	if !ok {
		t.Fatal("claude refused")
	}
	r.Close()
	if p.Free() != 1 {
		t.Errorf("free after close = %d, want 1", p.Free())
	}

	r2 := p.Reserve("hub")
	if got := r2.Free(); got.Total != 1 || got.ByHarness["codex"] != 0 {
		t.Errorf("second reservation = %+v", got)
	}
	r2.Close()
	relCodex()
	relCodex() // twice is harmless
	relClaude()
	if p.Free() != 3 {
		t.Errorf("free after releases = %d, want 3", p.Free())
	}
	if got := p.Reserve("hub").Free(); got.ByHarness["codex"] != 1 {
		t.Errorf("codex cap not restored: %+v", got)
	}
}

func TestPutBackReturnsToTheReservation(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 1, ByHarness: map[string]int{"claude": 1}})
	r := p.Reserve("hub")
	if _, ok := r.Take("claude"); !ok {
		t.Fatal("take")
	}
	r.putBack("claude")
	if _, ok := r.Take("claude"); !ok {
		t.Error("a put-back unit could not be taken again in the same sync")
	}
	if held, _ := p.Held("hub"); held != 1 {
		t.Errorf("held after put back and take again = %d, want 1", held)
	}
}

func TestEmptyReservation(t *testing.T) {
	r := emptyReservation()
	if r.Free().Total != 0 {
		t.Error("an empty reservation declares capacity")
	}
	if _, ok := r.Take("claude"); ok {
		t.Error("took from an empty reservation")
	}
	r.Close()
}

// syncOf is one sync of conn that claims up to n of what it is offered. It
// reports what it was offered and how to end each run it took.
func syncOf(t *testing.T, p *Pool, conn string, n int) (offered int, ends []func()) {
	t.Helper()
	r := p.Reserve(conn)
	offered = r.Free().Total
	for range min(offered, n) {
		end, ok := r.Take("claude")
		if !ok {
			t.Fatalf("%s could not take a unit it was offered", conn)
		}
		ends = append(ends, end)
	}
	r.Close()
	return offered, ends
}

// take is syncOf for a test that does not need the runs to end.
func take(t *testing.T, p *Pool, conn string, n int) int {
	t.Helper()
	offered, _ := syncOf(t, p, conn, n)
	return offered
}

// TestTwoConnectionsShareTheCapacity is the starvation case: both hubs want
// every unit, and each sync must come away with some of them.
func TestTwoConnectionsShareTheCapacity(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 4})
	p.Join("busy", 0)
	p.Join("quiet", 0)

	// The busy hub syncs first and asks for everything; half of the pool is
	// held back for the turn that is not its own.
	if got := take(t, p, "busy", 100); got != 2 {
		t.Errorf("the first sync was offered %d of 4, want 2", got)
	}
	if got := take(t, p, "quiet", 100); got != 2 {
		t.Errorf("the other hub was offered %d, want 2", got)
	}
	if p.Free() != 0 {
		t.Fatalf("free = %d, want 0", p.Free())
	}
	// The busy hub syncs over and over against a full pool and gets nothing,
	// which is the point: it is not what starves the other one.
	for range 10 {
		if got := take(t, p, "busy", 100); got != 0 {
			t.Fatalf("a full pool offered %d", got)
		}
	}
	if held, _ := p.Held("busy"); held != 2 {
		t.Errorf("busy holds %d, want 2", held)
	}
	if held, _ := p.Held("quiet"); held != 2 {
		t.Errorf("quiet holds %d, want 2", held)
	}
}

// TestAQuietConnectionYieldsItsTurn keeps the pool working when only one hub
// has work: a connection offered more than it uses passes until it asks again.
func TestAQuietConnectionYieldsItsTurn(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 4})
	p.Join("busy", 0)
	p.Join("quiet", 0)

	_, ends := syncOf(t, p, "busy", 100) // 2 of 4
	take(t, p, "quiet", 0)               // offered 2, takes none: it has no queue
	if got := take(t, p, "busy", 100); got != 2 {
		t.Fatalf("the busy hub was offered %d after the other passed, want the 2 it left", got)
	}
	if p.Free() != 0 {
		t.Errorf("a pool with one busy hub left %d units idle", p.Free())
	}
	// The quiet hub's next sync asks again, and its turn is waiting for it as
	// soon as a unit frees.
	if got := take(t, p, "quiet", 100); got != 0 {
		t.Fatalf("a full pool offered %d", got)
	}
	ends[0]() // a run of the busy hub's ends
	if got := take(t, p, "busy", 100); got != 0 {
		t.Errorf("the busy hub took %d units out of turn", got)
	}
	if got := take(t, p, "quiet", 100); got != 1 {
		t.Errorf("the freed unit reached the quiet hub as %d, want 1", got)
	}
}

// TestPassGivesUpTheTurn covers a connection that is draining, replaying or
// stopped: nothing syncs for it, so nothing may be held for it either.
func TestPassGivesUpTheTurn(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 4})
	p.Join("up", 0)
	p.Join("down", 0)
	p.Pass("down")
	if got := take(t, p, "up", 100); got != 4 {
		t.Errorf("a pool with one live connection offered %d of 4", got)
	}
}

// TestConnectionCapBoundsTheReservation is the acceptance criterion: a cap is
// what a sync may ask for, so no claim is ever refused for being over it.
func TestConnectionCapBoundsTheReservation(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 4})
	p.Join("capped", 1)
	p.Join("open", 0)

	if got := take(t, p, "capped", 100); got != 1 {
		t.Errorf("a connection capped at 1 was offered %d", got)
	}
	// At its cap it is offered nothing at all, rather than being offered work
	// and refusing it after the claim.
	r := p.Reserve("capped")
	if got := r.Free().Total; got != 0 {
		t.Errorf("a connection at its cap was offered %d", got)
	}
	if _, ok := r.Take("claude"); ok {
		t.Error("a connection at its cap took a unit")
	}
	r.Close()
	// Its unusable turn is not held against the other connection.
	if got := take(t, p, "open", 100); got != 3 {
		t.Errorf("the uncapped connection was offered %d of the 3 left", got)
	}
	if held, cap := p.Held("capped"); held != 1 || cap != 1 {
		t.Errorf("capped holds %d of %d, want 1 of 1", held, cap)
	}
}

// TestCapFreesAgainWhenARunEnds: the cap bounds what is held at once, not how
// many runs a hub may ever send.
func TestCapFreesAgainWhenARunEnds(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 4})
	p.Join("capped", 2)

	r := p.Reserve("capped")
	if got := r.Free().Total; got != 2 {
		t.Fatalf("offered %d, want the cap of 2", got)
	}
	rel1, _ := r.Take("claude")
	rel2, _ := r.Take("claude")
	r.Close()
	if got := p.Reserve("capped").Free().Total; got != 0 {
		t.Fatalf("offered %d at the cap", got)
	}
	rel1()
	if got := p.Reserve("capped").Free().Total; got != 1 {
		t.Errorf("offered %d after one run ended, want 1", got)
	}
	rel2()
}

// TestJoinOrderIsTheOwnersOrder: three hubs, one busy, and the two behind it
// in config.toml each get a turn rather than the next one always going first.
func TestTurnsGoRoundTheRing(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 3})
	for _, name := range []string{"a", "b", "c"} {
		p.Join(name, 0)
	}
	for _, name := range []string{"a", "b", "c"} {
		if got := take(t, p, name, 100); got != 1 {
			t.Fatalf("%s was offered %d of the 3, want 1 each", name, got)
		}
	}
	for _, name := range []string{"a", "b", "c"} {
		if held, _ := p.Held(name); held != 1 {
			t.Errorf("%s holds %d, want 1", name, held)
		}
	}
}

// TestOneFillDealsEveryConnectionItsShare is DEV-78: a fill is dealt from
// where it started, whatever order the connections sync in, so the hub that
// syncs last in an uneven division is not left with nothing. DEV-29's cases
// divided evenly and could not see it; these do not.
func TestOneFillDealsEveryConnectionItsShare(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int
		conns []string
		caps  map[string]int
		want  map[string]int // the first in the ring is dealt first
	}{
		{"three over four", 4, []string{"a", "b", "c"}, nil, map[string]int{"a": 2, "b": 1, "c": 1}},
		{"three over five", 5, []string{"a", "b", "c"}, nil, map[string]int{"a": 2, "b": 2, "c": 1}},
		{"four over six", 6, []string{"a", "b", "c", "d"}, nil, map[string]int{"a": 2, "b": 2, "c": 1, "d": 1}},
		// A connection that reaches its cap keeps the share it was dealt; it
		// is not dropped from the deal so that the next hub to sync is dealt
		// its turns as well as its own.
		{"a capped connection", 5, []string{"a", "b", "c"}, map[string]int{"a": 2}, map[string]int{"a": 2, "b": 2, "c": 1}},
	} {
		for _, order := range permutations(tc.conns) {
			t.Run(tc.name+"/"+strings.Join(order, ""), func(t *testing.T) {
				p := NewPool(v1.Capacity{Total: tc.total})
				for _, name := range tc.conns {
					p.Join(name, tc.caps[name])
				}
				for _, name := range order {
					if got := take(t, p, name, 100); got != tc.want[name] {
						t.Errorf("%s was offered %d of %d, want %d", name, got, tc.total, tc.want[name])
					}
				}
				if p.Free() != 0 {
					t.Errorf("the fill left %d of %d units free", p.Free(), tc.total)
				}
			})
		}
	}
}

// TestAPassedConnectionKeepsWhatItHolds: a hub with nothing more to ask for
// is out of the deal but still holds its runs, and what is free is shared
// among the hubs that do ask rather than going to whichever syncs first.
func TestAPassedConnectionKeepsWhatItHolds(t *testing.T) {
	for _, order := range [][]string{{"b", "c"}, {"c", "b"}} {
		p := NewPool(v1.Capacity{Total: 4})
		for _, name := range []string{"a", "b", "c"} {
			p.Join(name, 0)
		}
		take(t, p, "a", 2)
		p.Pass("a")
		for _, name := range order {
			if got := take(t, p, name, 100); got != 1 {
				t.Errorf("order %v: %s was offered %d of the 2 free, want 1", order, name, got)
			}
		}
	}
}

// TestTheRemainderGoesRoundTheRing is what DEV-29 guaranteed and a fixed deal
// must keep: when the capacity does not divide evenly, the extra unit moves
// on once a fill completes, so no hub is always the one short.
func TestTheRemainderGoesRoundTheRing(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 4})
	names := []string{"a", "b", "c"}
	for _, name := range names {
		p.Join(name, 0)
	}
	for fill, extra := range []string{"a", "b", "c", "a"} {
		var ends []func()
		for _, name := range names {
			offered, e := syncOf(t, p, name, 100)
			want := 1
			if name == extra {
				want = 2
			}
			if offered != want {
				t.Errorf("fill %d: %s was offered %d, want %d", fill, name, offered, want)
			}
			ends = append(ends, e...)
		}
		for _, end := range ends {
			end()
		}
	}
}

// TestAFreedUnitGoesToTheHubShortOfItsShare is rotation under churn: in a
// pool that has filled, the unit a run's end frees goes to the hub the next
// deal favours, not to whichever syncs first.
func TestAFreedUnitGoesToTheHubShortOfItsShare(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 3})
	p.Join("a", 0)
	p.Join("b", 0)
	_, endsA := syncOf(t, p, "a", 100) // 2: the extra unit of this fill
	take(t, p, "b", 100)               // 1, and the pool is full
	endsA[0]()
	if got := take(t, p, "a", 100); got != 0 {
		t.Errorf("a was offered %d of the unit its own run freed, want 0", got)
	}
	if got := take(t, p, "b", 100); got != 1 {
		t.Errorf("b was offered %d of the freed unit, want 1", got)
	}
}

// TestAPutBackUnitReturnedOnCloseStartsAFill: a claim that is never recorded
// puts its unit back, and a sync that ends without re-taking it frees it. The
// pool filling again after that is a fill of its own, and moves the deal on.
func TestAPutBackUnitReturnedOnCloseStartsAFill(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 4})
	for _, name := range []string{"a", "b", "c"} {
		p.Join(name, 0)
	}
	var ends []func()
	for _, name := range []string{"a", "b"} {
		_, e := syncOf(t, p, name, 100)
		ends = append(ends, e...)
	}
	r := p.Reserve("c")
	if _, ok := r.Take("claude"); !ok { // the pool is full, and the deal moves on to b
		t.Fatal("c could not take its unit")
	}
	r.putBack("claude")
	r.Close()
	// This fill was dealt from b, which is as short of its share as c and
	// comes first: c's next sync is offered nothing and b's takes the unit.
	// The pool fills again, and the next fill is dealt from c.
	if got := take(t, p, "c", 100); got != 0 {
		t.Errorf("c was offered %d of the unit that is b's turn, want 0", got)
	}
	_, e := syncOf(t, p, "b", 100)
	if len(e) != 1 {
		t.Fatalf("b took %d of the freed unit, want 1", len(e))
	}
	for _, end := range append(ends, e...) {
		end()
	}
	for name, want := range map[string]int{"a": 1, "b": 1, "c": 2} {
		// Offered without taking, so each sync sees the same deal.
		r := p.Reserve(name)
		if got := r.Free().Total; got != want {
			t.Errorf("%s was offered %d in the next fill, want %d", name, got, want)
		}
		defer r.Close()
	}
}

func permutations(s []string) [][]string {
	if len(s) <= 1 {
		return [][]string{append([]string(nil), s...)}
	}
	var out [][]string
	for i := range s {
		rest := append(append([]string(nil), s[:i]...), s[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]string{s[i]}, p...))
		}
	}
	return out
}

// hungry is a hub whose queue holds runs of one harness only, deeper than
// anything it could be offered.
type hungry struct{ name, harness string }

// syncHungry is one sync of such a hub: it takes every unit the reservation
// advertises room for, in its harness.
func syncHungry(t *testing.T, p *Pool, hub hungry) []func() {
	t.Helper()
	r := p.Reserve(hub.name)
	free := r.Free()
	n := free.Total
	if room, capped := free.ByHarness[hub.harness]; capped {
		n = min(n, room)
	}
	var ends []func()
	for range n {
		end, ok := r.Take(hub.harness)
		if !ok {
			t.Fatalf("%s could not take a %s unit it was offered", hub.name, hub.harness)
		}
		ends = append(ends, end)
	}
	r.Close()
	return ends
}

// TestTwoSyncsInFlightNeverPromiseOneUnitOfACap: a harness unit advertised
// by one sync is held for it until it closes, exactly as a unit of the pool
// is. Otherwise a second sync is dealt it again, both hubs offer a run for
// it, and whichever is claimed second is a run the runner cannot take.
func TestTwoSyncsInFlightNeverPromiseOneUnitOfACap(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 4, ByHarness: map[string]int{"claude": 2}})
	p.Join("first", 2)
	p.Join("second", 0)
	p.Pass("second")

	first := p.Reserve("first")
	if got := first.Free(); got.Total != 2 || got.ByHarness["claude"] != 2 {
		t.Fatalf("first was offered %+v, want 2 and the whole Claude cap", got)
	}
	second := p.Reserve("second")
	if got := second.Free(); got.Total+first.Free().Total > 4 || got.ByHarness["claude"]+first.Free().ByHarness["claude"] > 2 {
		t.Errorf("second was offered %+v while first holds %+v: one unit is promised twice", got, first.Free())
	}
	for range 2 {
		if _, ok := first.Take("claude"); !ok {
			t.Fatal("first could not take a Claude unit it was offered")
		}
	}
	first.Close()
	second.Close()

	// A take for one harness trims what the sync holds of another to the
	// units it has left, so those go back to the other syncs at once.
	p = NewPool(v1.Capacity{Total: 2, ByHarness: map[string]int{"claude": 2, "codex": 2}})
	p.Join("a", 0)
	r := p.Reserve("a")
	if _, ok := r.Take("codex"); !ok {
		t.Fatal("take codex")
	}
	if got := r.Free(); got.Total != 1 || got.ByHarness["claude"] != 1 {
		t.Errorf("after one take a holds %+v, want 1 unit and 1 of Claude", got)
	}
	r.Close()
	if got := p.Reserve("a").Free(); got.ByHarness["claude"] != 1 {
		t.Errorf("after close a is offered %+v of Claude, want the 1 codex leaves", got)
	}
}

// TestAHubCappedOutOfItsHarnessKeepsItsTurn is DEV-106 as it was filed. One
// hub holds the whole Claude cap, taken while the other had nothing queued.
// The other's work arrives: it is offered total capacity and no Claude, so its
// hub offers nothing and the units go back unused. That is not a hub with no
// work, and the Claude unit that frees next is its turn — not the first hub's,
// however soon that one syncs again.
func TestAHubCappedOutOfItsHarnessKeepsItsTurn(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 4, ByHarness: map[string]int{"claude": 2}})
	p.Join("first", 0)
	p.Join("second", 0)
	first, second := hungry{"first", "claude"}, hungry{"second", "claude"}

	take(t, p, "second", 0) // nothing queued yet: it passes
	ends := syncHungry(t, p, first)
	if len(ends) != 2 {
		t.Fatalf("the only hub asking took %d of the Claude cap of 2", len(ends))
	}
	r := p.Reserve("second")
	if got := r.Free(); got.Total == 0 || got.ByHarness["claude"] != 0 {
		t.Fatalf("second was offered %+v, want total capacity and no Claude", got)
	}
	r.Close()

	ends[0]()
	if got := syncHungry(t, p, first); len(got) != 0 {
		t.Errorf("the hub holding the cap took %d of the Claude unit its own run freed", len(got))
	}
	if got := syncHungry(t, p, second); len(got) != 1 {
		t.Errorf("the hub waiting for Claude took %d of the freed unit, want 1", len(got))
	}
}

// TestAHarnessCapGoesRoundTheHubs is DEV-106: a harness's cap is dealt round
// the hubs the way the whole pool is, and a hub its cap gave nothing to is
// still waiting for it rather than taken for one with no work.
//
// The order is the adversarial one: runs end oldest first, and the hub whose
// run ended syncs again at once, before anyone else — a loop that syncs when
// its runs end does exactly that. Before the fix, a hub that took the whole
// cap refilled it every time, and the other passed on every sync that offered
// it total capacity and none of the harness, and waited indefinitely.
func TestAHarnessCapGoesRoundTheHubs(t *testing.T) {
	claude := func(names ...string) []hungry {
		var hubs []hungry
		for _, n := range names {
			hubs = append(hubs, hungry{n, "claude"})
		}
		return hubs
	}
	for _, tc := range []struct {
		name  string
		total int
		caps  map[string]int
		hubs  []hungry
	}{
		{"two hubs over a cap of two", 4, map[string]int{"claude": 2}, claude("a", "b")},
		{"three hubs over a cap of two", 6, map[string]int{"claude": 2}, claude("a", "b", "c")},
		{"two hubs over a cap of one", 4, map[string]int{"claude": 1}, claude("a", "b")},
		{"three hubs over a cap of one", 4, map[string]int{"claude": 1}, claude("a", "b", "c")},
		{"an uncapped hub first, a capped one second", 4, map[string]int{"claude": 1},
			[]hungry{{"codex-only", "codex"}, {"claude-only", "claude"}}},
		{"a capped hub first, an uncapped one second", 4, map[string]int{"claude": 1},
			[]hungry{{"claude-only", "claude"}, {"codex-only", "codex"}}},
		{"two over a cap of two, one uncapped", 4, map[string]int{"claude": 2},
			[]hungry{{"a", "claude"}, {"b", "claude"}, {"c", "codex"}}},
		{"both harnesses capped", 4, map[string]int{"claude": 1, "codex": 3},
			[]hungry{{"a", "codex"}, {"b", "claude"}, {"c", "claude"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPool(v1.Capacity{Total: tc.total, ByHarness: tc.caps})
			for _, hub := range tc.hubs {
				p.Join(hub.name, 0)
			}
			type run struct {
				hub int
				end func()
			}
			var running []run
			started := make([]int, len(tc.hubs))
			last := make([]int, len(tc.hubs))
			gap := make([]int, len(tc.hubs))
			sync := func(i, step int) {
				for _, end := range syncHungry(t, p, tc.hubs[i]) {
					running = append(running, run{i, end})
					started[i]++
					gap[i] = max(gap[i], step-last[i])
					last[i] = step
				}
			}
			sweep := func(step int) {
				for i := range tc.hubs {
					sync(i, step)
				}
			}
			// What the hubs can use between them with every queue deep: the
			// pool, or the caps of the only harnesses they want.
			usable := 0
			for _, h := range []string{"claude", "codex"} {
				wanted := false
				for _, hub := range tc.hubs {
					wanted = wanted || hub.harness == h
				}
				if cap, capped := tc.caps[h]; wanted && capped {
					usable += cap
				} else if wanted {
					usable += tc.total
				}
			}
			usable = min(usable, tc.total)

			// A unit dealt to a hub that cannot use it — its harness is at its
			// cap — waits until that hub's next sync passes on it, so every
			// hub syncing once more than there are hubs is enough for the
			// pool to be as full as the queues can make it.
			settle := func(step int) {
				for range len(tc.hubs) + 1 {
					sweep(step)
				}
				if used := tc.total - p.Free(); used != usable {
					t.Fatalf("step %d: %d of %d units in use, want %d: capacity idle with every queue deep", step, used, tc.total, usable)
				}
			}
			settle(0)
			const steps = 60
			for step := 1; step <= steps; step++ {
				oldest := running[0]
				running = running[1:]
				oldest.end()
				sync(oldest.hub, step)
				settle(step)
			}
			// No hub goes longer without a start than a few run ends per hub:
			// long enough for its turn to come round, and far short of the
			// whole test, which is what waiting indefinitely looks like.
			bound := 3 * len(tc.hubs)
			for i, hub := range tc.hubs {
				gap[i] = max(gap[i], steps-last[i])
				if started[i] == 0 || gap[i] > bound {
					t.Errorf("%s started %d %s runs, and went %d of %d run ends without one (bound %d)",
						hub.name, started[i], hub.harness, gap[i], steps, bound)
				}
			}
		})
	}
}
