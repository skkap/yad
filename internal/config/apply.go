package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// A work machine's config.toml has an author besides yad: the machine spec it
// was built from (decision 0059). The spec owns every setting but two things
// only the machine can know — the hubs it is connected to, and the accounts
// logged in on it, whether at its terminal or by a hub. So a spec's accounts
// are made sure of and never subtracted: a label the spec does not list may
// be one added at the machine, and a diff of two lists cannot tell that from
// one the spec dropped. Removing an account stays `yad account remove`'s, the
// one path that knows whether a run is on it (0043).

// Change is one setting Apply changed, keyed as TOML names it —
// "harness.claude.accounts". From or To is empty when the setting was or is
// now absent.
type Change struct{ Key, From, To string }

func (c Change) String() string {
	switch {
	case c.From == "":
		return fmt.Sprintf("%s = %s (was unset)", c.Key, c.To)
	case c.To == "":
		return fmt.Sprintf("%s removed (was %s)", c.Key, c.From)
	}
	return fmt.Sprintf("%s = %s (was %s)", c.Key, c.To, c.From)
}

// Applied is what Apply did: wrote config.toml where there was none, changed
// the settings listed, or — neither — left the file exactly as it was.
type Applied struct {
	Created bool
	Changes []Change
}

// Changed is whether config.toml was written.
func (a Applied) Changed() bool { return a.Created || len(a.Changes) > 0 }

// Apply brings the profile's config.toml in line with spec, under the lock
// every writer takes, and writes it only when a setting differs — so applying
// the same spec twice leaves the file untouched the second time.
func Apply(ctx context.Context, p Paths, spec Config) (Applied, error) {
	var out Applied
	_, err := Update(ctx, p, func(have *Config) (bool, error) {
		_, statErr := os.Stat(p.ConfigFile())
		switch {
		case errors.Is(statErr, fs.ErrNotExist):
			out.Created = true
		case statErr != nil:
			return false, statErr
		}
		next := merge(spec, *have)
		before, err := flatten(*have)
		if err != nil {
			return false, err
		}
		after, err := flatten(next)
		if err != nil {
			return false, err
		}
		if !out.Created {
			out.Changes = diff(before, after)
		}
		*have = next
		return out.Changed(), nil
	})
	if err != nil {
		return Applied{}, err
	}
	return out, nil
}

// merge is spec with what only the machine knows kept from have: its
// connections, and every account it lists that spec does not.
func merge(spec, have Config) Config {
	next := spec
	next.Labels = slices.Clone(spec.Labels)
	next.Workdirs.Roots = slices.Clone(spec.Workdirs.Roots)
	if p := spec.Workdirs.PathSources; p != nil {
		next.Workdirs.PathSources = new(*p)
	}
	// A connection needs a credential that only `yad connect` on the machine
	// makes, so the spec's are never taken: one taken would name a hub the
	// runner cannot sync with.
	next.Connections = slices.Clone(have.Connections)
	next.Harness = make(map[string]HarnessConfig, len(spec.Harness))
	for id, h := range spec.Harness {
		// The spec's accounts first, in its order — the order breaks ties
		// (0039) and is the spec's to set — then the machine's own, in
		// theirs.
		accounts := slices.Clone(h.Accounts)
		for _, a := range have.Harness[id].Accounts {
			if !slices.Contains(accounts, a) {
				accounts = append(accounts, a)
			}
		}
		h.Accounts = accounts
		next.Harness[id] = h
	}
	for id, h := range have.Harness {
		if _, ok := spec.Harness[id]; ok || len(h.Accounts) == 0 {
			continue
		}
		// A harness the spec has no section for keeps only its accounts;
		// its settings, like any the spec leaves out, go back to the
		// default.
		next.Harness[id] = HarnessConfig{Accounts: slices.Clone(h.Accounts)}
	}
	if len(next.Harness) == 0 {
		next.Harness = nil
	}
	return next
}

// flatten is c as the settings config.toml would hold, one per dotted key, each
// value in TOML's own notation. Built from the file's own encoding rather
// than field by field, so a setting added to Config later is compared and
// reported without anyone remembering to add it here, and so "unset" and
// "empty" — which the file cannot tell apart — compare equal.
func flatten(c Config) (map[string]string, error) {
	b, err := toml.Marshal(c)
	if err != nil {
		return nil, err
	}
	var tree map[string]any
	if err := toml.Unmarshal(b, &tree); err != nil {
		return nil, err
	}
	out := map[string]string{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			if sub, ok := v.(map[string]any); ok {
				walk(prefix+k+".", sub)
				continue
			}
			out[prefix+k] = tomlValue(v)
		}
	}
	walk("", tree)
	return out, nil
}

func tomlValue(v any) string {
	switch v := v.(type) {
	case string:
		return strconv.Quote(v)
	case []any:
		parts := make([]string, len(v))
		for i, e := range v {
			parts[i] = tomlValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		parts := make([]string, 0, len(v))
		for _, k := range slices.Sorted(maps.Keys(v)) {
			parts = append(parts, k+" = "+tomlValue(v[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(v)
	}
}

// diff is every key whose value differs, in key order so the report reads the
// same twice.
func diff(before, after map[string]string) []Change {
	keys := slices.Sorted(maps.Keys(before))
	for k := range after {
		if _, ok := before[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var out []Change
	for _, k := range keys {
		if before[k] != after[k] {
			out = append(out, Change{Key: k, From: before[k], To: after[k]})
		}
	}
	return out
}
