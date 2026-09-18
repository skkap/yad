package runner

import (
	"github.com/skkap/yad/internal/adapter"
)

// Registry maps a harness id to the adapter that drives it. It is the only way
// the executor finds an adapter, so a harness with no entry here cannot run
// whatever the catalog says about it.
type Registry struct {
	byHarness map[string]adapter.Adapter
}

// NewRegistry registers each adapter under its own harness id; a later one
// with the same id replaces an earlier one, which is how a test swaps in the
// fake.
func NewRegistry(adapters ...adapter.Adapter) *Registry {
	r := &Registry{byHarness: map[string]adapter.Adapter{}}
	for _, a := range adapters {
		r.byHarness[a.Harness()] = a
	}
	return r
}

// Lookup returns the adapter for a harness.
func (r *Registry) Lookup(harness string) (adapter.Adapter, bool) {
	if r == nil {
		return nil, false
	}
	a, ok := r.byHarness[harness]
	return a, ok
}
