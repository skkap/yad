package runner

import (
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

func TestPool(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 3, ByHarness: map[string]int{"codex": 1}})

	r := p.Reserve()
	if got := r.Free(); got.Total != 3 || got.ByHarness["codex"] != 1 {
		t.Fatalf("free = %+v", got)
	}
	// While one sync holds the capacity, another sees none of it.
	if other := p.Reserve(); other.Free().Total != 0 {
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

	r2 := p.Reserve()
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
	if got := p.Reserve().Free(); got.ByHarness["codex"] != 1 {
		t.Errorf("codex cap not restored: %+v", got)
	}
}

func TestPutBackReturnsToTheReservation(t *testing.T) {
	p := NewPool(v1.Capacity{Total: 1, ByHarness: map[string]int{"claude": 1}})
	r := p.Reserve()
	if _, ok := r.Take("claude"); !ok {
		t.Fatal("take")
	}
	r.putBack("claude")
	if _, ok := r.Take("claude"); !ok {
		t.Error("a put-back unit could not be taken again in the same sync")
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
