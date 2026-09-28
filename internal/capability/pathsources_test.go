package capability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
)

// The document says path_sources only when it is off, so a runner that takes
// them sends what it sent before the field existed, fingerprint and all, and
// a hub reading an older runner's document reads the same thing (decision
// 0061).
func TestPathSourcesIsSentOnlyWhenOff(t *testing.T) {
	noTools(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		set  *bool
		want string // the field as it appears in the JSON; "" for absent
	}{
		{"unset", nil, ""},
		{"on", new(true), ""},
		{"off", new(false), `"path_sources":false`},
	} {
		cfg := config.Default()
		cfg.Workdirs.PathSources = tc.set
		b, err := json.Marshal(Build(ctx, "r1", cfg, nil))
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(string(b), "path_sources")
		if tc.want == "" && got || tc.want != "" && !strings.Contains(string(b), tc.want) {
			t.Errorf("%s: the document is %s, want path_sources %q", tc.name, b, tc.want)
		}
	}

	on := Build(ctx, "r1", config.Default(), nil)
	cfg := config.Default()
	cfg.Workdirs.PathSources = new(false)
	if Fingerprint(on) == Fingerprint(Build(ctx, "r1", cfg, nil)) {
		t.Error("switching path sources off did not move the fingerprint, so a hub would never hear of it")
	}
}
