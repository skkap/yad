package codex

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Codex names no models in the catalog: which it offers depends on the
// account's plan, and the list moves with Codex releases rather than with
// yad's. Codex fetches that list itself and keeps it in its home, and this
// reads it back, so a runner reports what its logins are actually offered
// without spending a request of its own (DEV-124).

// modelsCache is the file in a Codex home holding the model list Codex last
// fetched for that login.
const modelsCache = "models_cache.json"

// modelsCacheCap bounds the read. Codex keeps each model's whole instructions
// template in the file, so it runs to a few hundred kilobytes; a file past
// this is not one Codex wrote, and reporting nothing is the safe answer.
const modelsCacheCap = 8 << 20

// maxModels bounds what one runner reports. Codex lists a handful; the bound
// keeps a damaged cache from growing every capability document.
const maxModels = 64

// modelName is what a model slug looks like: gpt-5.5, gpt-5.6-luna,
// o4-mini. The charset is the guarantee that nothing else from the file —
// a path, a URL, a sentence — reaches the capability document every
// connected hub reads (DEV-67).
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// cachedModel is the part of an entry in models_cache.json that is read.
// Visibility is Codex's own: "list" is shown in its picker, "hide" is a
// model it keeps for itself, such as the one that reviews its own work.
type cachedModel struct {
	Slug       string `json:"slug"`
	Visibility string `json:"visibility"`
	Priority   int    `json:"priority"`
}

// DefaultHome is the home Codex uses for a run with no account: CODEX_HOME
// in the runner's environment, which a run inherits, or ~/.codex.
func DefaultHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// Models is every model Codex lists in these homes' caches, each once, in
// Codex's own order: the priority it gives each model, which is the order of
// its picker. A model two logins both offer sorts where the first one puts
// it. A home with no cache, or one that does not parse, adds nothing:
// absence is reported by leaving the list short, never as an error, because
// a runner that has not yet run Codex under a login knows no models for it.
func Models(homes ...string) []string {
	type ranked struct {
		name     string
		priority int
	}
	seen := map[string]bool{}
	var all []ranked
	for _, home := range homes {
		if home == "" {
			continue
		}
		for _, m := range readModels(filepath.Join(home, modelsCache)) {
			if seen[m.Slug] || m.Visibility != "list" || !modelName.MatchString(m.Slug) {
				continue
			}
			seen[m.Slug] = true
			all = append(all, ranked{m.Slug, m.Priority})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].priority < all[j].priority })
	var out []string
	for _, r := range all {
		if len(out) == maxModels {
			break
		}
		out = append(out, r.name)
	}
	return out
}

func readModels(path string) []cachedModel {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, modelsCacheCap+1))
	if err != nil || len(b) > modelsCacheCap {
		return nil
	}
	var cache struct {
		Models []cachedModel `json:"models"`
	}
	if json.Unmarshal(b, &cache) != nil {
		return nil
	}
	return cache.Models
}
