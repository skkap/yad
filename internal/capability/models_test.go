package capability

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
)

// asker stands in for every harness: it answers by the home the environment
// names, and records what it was asked with.
type asker struct {
	mu     sync.Mutex
	answer func(harnessID, home string) ([]string, error)
	asked  []askedWith
}

type askedWith struct {
	harness, home string
	env           []string
}

func (a *asker) install(t *testing.T) {
	t.Helper()
	modelsMu.Lock()
	modelsAsked = map[string]modelsAnswer{}
	modelsMu.Unlock()
	ListModelsForTests = func(ctx context.Context, id, bin, dir string, env []string) ([]string, error) {
		home := ""
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, account.HomeVar(id)+"="); ok {
				home = v
			}
		}
		a.mu.Lock()
		a.asked = append(a.asked, askedWith{id, home, env})
		a.mu.Unlock()
		return a.answer(id, home)
	}
	t.Cleanup(func() { ListModelsForTests = nil })
}

func (a *asker) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.asked)
}

func ready(t *testing.T, id string) harness.Detected {
	t.Helper()
	h, ok := harness.Lookup(id)
	if !ok {
		t.Fatalf("no %s in the catalog", id)
	}
	return harness.Detected{Harness: h, Present: true, Path: "/x/" + id, Version: "1.0.0"}
}

func codexCache(t *testing.T, slugs ...string) string {
	t.Helper()
	home := t.TempDir()
	body := `{"models":[`
	for i, s := range slugs {
		if i > 0 {
			body += ","
		}
		body += `{"slug":"` + s + `","visibility":"list","priority":1}`
	}
	body += "]}"
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func harnessReport(reps []v1.HarnessReport, id string) v1.HarnessReport {
	for _, r := range reps {
		if r.ID == id {
			return r
		}
	}
	return v1.HarnessReport{}
}

func accountModels(r v1.HarnessReport) map[string][]string {
	out := map[string][]string{}
	for _, a := range r.Accounts {
		if a.Models != nil {
			out[a.Label] = a.Models
		}
	}
	return out
}

// Each login a run may use is asked, in that login's environment, and the
// document says what each account is offered as well as what the harness
// offers any of them. An account that needs login is not asked: what a
// harness lists with no login describes no account.
func TestModelsAreAskedOfEveryLogin(t *testing.T) {
	work, home, gone := t.TempDir(), t.TempDir(), t.TempDir()
	if err := account.SetToken(home, "sk-ant-oat01-"+strings.Repeat("a", 80)); err != nil {
		t.Fatal(err)
	}
	a := &asker{answer: func(id, h string) ([]string, error) {
		switch {
		case id == "claude" && h == work:
			return []string{"default", "opus", "haiku"}, nil
		case id == "claude" && h == home:
			return []string{"default", "sonnet", "haiku"}, nil
		case id == "codex" && h == "":
			return []string{"gpt-6-astra", "gpt-5.5"}, nil
		}
		return nil, errors.New("asked about a login no run uses")
	}}
	a.install(t)
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work", "home", "gone"}}}
	accounts := []account.Account{
		{Harness: "claude", Label: "work", Home: work, State: v1.AccountFree},
		{Harness: "claude", Label: "home", Home: home, State: v1.AccountLimited},
		{Harness: "claude", Label: "gone", Home: gone, State: v1.AccountNeedsLogin},
	}
	found := []harness.Detected{ready(t, "claude"), ready(t, "codex")}
	addModels(context.Background(), found, cfg, accounts)
	reps := Harnesses(found, cfg, accounts)

	c := harnessReport(reps, "claude")
	if want := []string{"default", "opus", "haiku", "sonnet"}; !slices.Equal(c.Models, want) || c.ModelsSource != v1.ModelsFromHarness {
		t.Errorf("claude models %v from %q, want %v from the harness", c.Models, c.ModelsSource, want)
	}
	per := accountModels(c)
	if !slices.Equal(per["work"], []string{"default", "opus", "haiku"}) || !slices.Equal(per["home"], []string{"default", "sonnet", "haiku"}) {
		t.Errorf("per account: %v", per)
	}
	if _, ok := per["gone"]; ok {
		t.Errorf("an account that needs login was reported models: %v", per)
	}
	x := harnessReport(reps, "codex")
	if want := []string{"gpt-6-astra", "gpt-5.5"}; !slices.Equal(x.Models, want) || x.ModelsSource != v1.ModelsFromHarness || x.Accounts != nil {
		t.Errorf("codex: %+v, want %v from its default login and no accounts", x, want)
	}

	// Each ask ran in the environment a run on that login gets: the
	// account's home and, for a token account, its token (DEV-62).
	for _, w := range a.asked {
		if w.home == gone {
			t.Errorf("the account that needs login was asked")
		}
		want := account.Env(w.harness, w.home)
		if !slices.Equal(w.env, want) {
			t.Errorf("%s at %q was asked with %d variables, not a run's %d", w.harness, w.home, len(w.env), len(want))
		}
	}
	if a.count() != 3 {
		t.Errorf("%d asks, want one per login a run may use (3)", a.count())
	}
}

// A harness that cannot be asked, or does not answer, keeps the catalog's
// list and the document says it is the catalog's. Codex has no catalog list;
// its own cache of what it last fetched answers for it, which is still the
// harness's word.
func TestModelsFallBack(t *testing.T) {
	a := &asker{answer: func(string, string) ([]string, error) { return nil, errors.New("no answer") }}
	a.install(t)

	t.Setenv("CODEX_HOME", codexCache(t, "gpt-cached"))
	found := []harness.Detected{ready(t, "claude"), ready(t, "codex")}
	addModels(context.Background(), found, config.Default(), nil)
	reps := Harnesses(found, config.Default(), nil)
	if c := harnessReport(reps, "claude"); !slices.Equal(c.Models, []string{"opus", "sonnet", "haiku"}) || c.ModelsSource != v1.ModelsFromCatalog {
		t.Errorf("claude models %v from %q, want the catalog's, said to be", c.Models, c.ModelsSource)
	}
	if x := harnessReport(reps, "codex"); !slices.Equal(x.Models, []string{"gpt-cached"}) || x.ModelsSource != v1.ModelsFromHarness {
		t.Errorf("codex models %v from %q, want its cache's", x.Models, x.ModelsSource)
	}

	// With no cache either, Codex has nothing to say, and says nothing.
	t.Setenv("CODEX_HOME", t.TempDir())
	found = []harness.Detected{ready(t, "codex")}
	addModels(context.Background(), found, config.Default(), nil)
	if x := Harnesses(found, config.Default(), nil)[0]; x.Models != nil || x.ModelsSource != "" {
		t.Errorf("codex: models %v from %q, want neither", x.Models, x.ModelsSource)
	}
}

// Nothing that cannot take a run is asked: a harness with an error, one not
// installed, and an owner's accounts whose states could not be read — whose
// default home describes a login no run uses.
func TestModelsAskOnlyWhatRunsUse(t *testing.T) {
	a := &asker{answer: func(string, string) ([]string, error) { return []string{"asked"}, nil }}
	a.install(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	broken := ready(t, "claude")
	broken.Error = "not logged in on this machine"
	missing, _ := harness.Lookup("codex")
	withAccounts := config.Default()
	withAccounts.Harness = map[string]config.HarnessConfig{"codex": {Accounts: []string{"work"}}}
	for _, tc := range []struct {
		name  string
		found []harness.Detected
		cfg   config.Config
	}{
		{"an error", []harness.Detected{broken}, config.Default()},
		{"not installed", []harness.Detected{{Harness: missing}}, config.Default()},
		{"accounts unread", []harness.Detected{ready(t, "codex")}, withAccounts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addModels(context.Background(), tc.found, tc.cfg, nil)
			if slices.Contains(tc.found[0].Models, "asked") {
				t.Errorf("asked: %+v", tc.found[0])
			}
		})
	}
	if a.count() != 0 {
		t.Errorf("%d asks, want none", a.count())
	}
}

// An answer is kept: the daemon rebuilds the document every fifteen seconds,
// and each ask starts the harness. Once stale it is asked again; a failed ask
// keeps the last answer and tries again sooner. A login change forgets them.
func TestModelsAreKept(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	modelsNow = func() time.Time { return now }
	t.Cleanup(func() { modelsNow = time.Now })
	fail := false
	a := &asker{answer: func(string, string) ([]string, error) {
		if fail {
			return nil, errors.New("no answer")
		}
		return []string{"opus"}, nil
	}}
	a.install(t)
	build := func() v1.HarnessReport {
		found := []harness.Detected{ready(t, "claude")}
		addModels(context.Background(), found, config.Default(), nil)
		return Harnesses(found, config.Default(), nil)[0]
	}
	steps := []struct {
		name    string
		advance time.Duration
		fail    bool
		forget  bool
		asks    int
		source  string
	}{
		{"first", 0, false, false, 1, v1.ModelsFromHarness},
		{"fresh", modelsRecheck - time.Minute, false, false, 1, v1.ModelsFromHarness},
		{"stale, and the harness fails", 2 * time.Minute, true, false, 2, v1.ModelsFromHarness},
		{"failed, waiting to retry", modelsRetry - time.Minute, true, false, 2, v1.ModelsFromHarness},
		{"retried", 2 * time.Minute, false, false, 3, v1.ModelsFromHarness},
		{"forgotten", 0, false, true, 4, v1.ModelsFromHarness},
	}
	for _, s := range steps {
		now = now.Add(s.advance)
		fail = s.fail
		if s.forget {
			ForgetModels("claude")
		}
		r := build()
		if a.count() != s.asks || r.ModelsSource != s.source || !slices.Equal(r.Models, []string{"opus"}) {
			t.Errorf("%s: %d asks, models %v from %q; want %d asks and the harness's list", s.name, a.count(), r.Models, r.ModelsSource, s.asks)
		}
	}
}

// A login that changes while its harness is being asked — a hub login ending
// mid-build — is not undone by that ask: its answer may be the old login's,
// so it is not kept, and the next document asks again.
func TestModelsForgottenMidAskAreNotKept(t *testing.T) {
	asking, release := make(chan struct{}), make(chan struct{})
	first := true
	a := &asker{answer: func(string, string) ([]string, error) {
		if first {
			first = false
			close(asking)
			<-release
			return []string{"old-plan"}, nil
		}
		return []string{"new-plan"}, nil
	}}
	a.install(t)
	build := func() []string {
		found := []harness.Detected{ready(t, "claude")}
		addModels(context.Background(), found, config.Default(), nil)
		return found[0].Models
	}
	done := make(chan []string, 1)
	go func() { done <- build() }()
	<-asking
	ForgetModels("claude")
	close(release)
	if got := <-done; !slices.Equal(got, []string{"old-plan"}) {
		t.Fatalf("the ask in flight reported %v", got)
	}
	if got := build(); !slices.Equal(got, []string{"new-plan"}) || a.count() != 2 {
		t.Errorf("after the forget: models %v after %d asks, want the new login's after asking again", got, a.count())
	}
}

// A harness that hangs is given up on at the bound, and the document goes
// out without it: a registration never waits on a model list.
func TestModelsDoNotHoldTheDocument(t *testing.T) {
	old := modelsTimeout
	modelsTimeout = 100 * time.Millisecond
	t.Cleanup(func() { modelsTimeout = old })
	modelsMu.Lock()
	modelsAsked = map[string]modelsAnswer{}
	modelsMu.Unlock()
	ListModelsForTests = func(ctx context.Context, _, _, _ string, _ []string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { ListModelsForTests = nil })
	start := time.Now()
	found := []harness.Detected{ready(t, "claude")}
	addModels(context.Background(), found, config.Default(), nil)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the document waited %s on a hanging harness", took)
	}
	if found[0].ModelsSource != v1.ModelsFromCatalog {
		t.Errorf("models from %q, want the catalog", found[0].ModelsSource)
	}
}
