package acp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// schemaDir is the pinned release's bundle.
var schemaDir = filepath.Join("testdata", "acp-schema-"+SchemaVersion)

// surfaceHash hashes every definition the surface reaches, in name order, as
// Go re-encodes it — sorted keys, so the hash is the schema's and not its
// formatting's. A name the surface holds and the bundle lacks is hashed as
// missing, so a removed definition moves the hash too.
func surfaceHash(bundle []byte) (string, error) {
	var s struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(bundle, &s); err != nil {
		return "", err
	}
	seen := map[string]bool{}
	var walk func(name string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		raw, ok := s.Defs[name]
		if !ok {
			return
		}
		for _, part := range strings.Split(string(raw), `"#/$defs/`)[1:] {
			ref, _, _ := strings.Cut(part, `"`)
			walk(ref)
		}
	}
	for _, name := range surface {
		walk(name)
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	slices.Sort(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n + "\n"))
		raw, ok := s.Defs[n]
		if !ok {
			h.Write([]byte("missing\n"))
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", err
		}
		b, _ := json.Marshal(v)
		h.Write(append(b, '\n'))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// The committed bundle is the one the core is pinned to, and the only one
// (decision 0067): a new release is pinned by replacing the bundle and the
// hash together.
func TestPinnedSchema(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(schemaDir, "schema.unstable.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := surfaceHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if sum != schemaSum {
		t.Errorf("the core's surface of ACP schema %s hashes to %s, and %s is pinned — read what changed, then pin the new hash in schema.go", SchemaVersion, sum, schemaSum)
	}
	dirs, _ := filepath.Glob(filepath.Join("testdata", "acp-schema-*"))
	if len(dirs) != 1 {
		t.Errorf("schema bundles %v: only the pinned release is kept (decision 0067)", dirs)
	}
	var meta struct {
		AgentMethods  map[string]string `json:"agentMethods"`
		ClientMethods map[string]string `json:"clientMethods"`
	}
	mb, err := os.ReadFile(filepath.Join(schemaDir, "meta.unstable.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatal(err)
	}
	var known []string
	for _, m := range meta.AgentMethods {
		known = append(known, m)
	}
	for _, m := range meta.ClientMethods {
		known = append(known, m)
	}
	for _, m := range methods {
		if !slices.Contains(known, m) {
			t.Errorf("the core uses %s, which ACP schema %s does not define", m, SchemaVersion)
		}
	}
}
