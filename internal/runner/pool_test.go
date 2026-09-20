package runner

import (
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
