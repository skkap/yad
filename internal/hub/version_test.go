package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

func TestMeetsFloor(t *testing.T) {
	for _, c := range []struct {
		reported, floor string
		want            bool
	}{
		{"0.4.0", "", true},                 // no floor: every runner passes
		{"0.3.9", "0.4.0", false},           // the case this whole path exists for
		{"0.4.0", "0.4.0", true},            // the floor itself is allowed
		{"v0.4.0", "0.4.0", true},           // the v prefix `git describe` writes
		{"0.4.1", "v0.4.0", true},           // and on the floor's side too
		{"1.0.0", "0.9.9", true},            // major before minor
		{"0.10.0", "0.9.0", true},           // compared as numbers, not strings
		{"0.4", "0.4.0", true},              // a missing component is zero
		{"0.4.0-4-gabc1234", "0.4.0", true}, // four commits after the tag, not before it
		{"0.4.0-rc1", "0.4.0", true},        // and a prerelease of it counts as it
		{"dev", "0.4.0", true},              // an unstamped build is not refused
		{"", "0.4.0", true},                 // nor a runner that reports nothing
		{"0.3.0", "later", true},            // nor anyone, on a floor nobody can read
		{"0.3.0", "1.2.3.4", true},
	} {
		if got := meetsFloor(c.reported, c.floor); got != c.want {
			t.Errorf("meetsFloor(%q, %q) = %v, want %v", c.reported, c.floor, got, c.want)
		}
	}
}

func TestValidateMinVersion(t *testing.T) {
	for _, c := range []struct {
		in string
		ok bool
	}{{"", true}, {"0.4.0", true}, {"v0.4.0", true}, {"0.4", true}, {"dev", false}, {"latest", false}, {"0.4.0.1", false}, {"0.x", false}} {
		if err := ValidateMinVersion(c.in); (err == nil) != c.ok {
			t.Errorf("ValidateMinVersion(%q) = %v, want ok=%v", c.in, err, c.ok)
		}
	}
}

// floored is this fixture's hub with a version floor, over the same store:
// what an operator gets by restarting `yad hub serve --min-version`, runners
// already registered and all.
func (f *fixture) floored(min string) *Hub {
	return New(Options{Store: f.store, Now: f.clock.Now, MinVersion: min})
}

func versioned(id, version string) v1.Capabilities {
	d := doc(id)
	d.YadVersion = version
	return d
}

// A runner below the floor is turned away at register, with both versions and
// the next action in the error, and its registration token survives to be used
// by the upgraded runner.
func TestRegisterRefusesBelowMinVersion(t *testing.T) {
	const floor = "0.4.0"
	for _, c := range []struct {
		version string
		code    int
	}{
		{"0.3.9", http.StatusUpgradeRequired},
		{"0.4.0", http.StatusOK},
		{"0.4.1-2-gdeadbee", http.StatusOK},
		{"dev", http.StatusOK},
	} {
		t.Run(c.version, func(t *testing.T) {
			f := newFixture(t)
			tok := f.token(t, time.Hour)
			res, env := post(t, f.floored(floor), "/v1/runners/register", registerBody(t, versioned("r1", c.version)), headers(tok))
			if res.StatusCode != c.code {
				t.Fatalf("register %s: %d %+v, want %d", c.version, res.StatusCode, env, c.code)
			}
			if c.code == http.StatusOK {
				return
			}
			if env.Error.Code != v1.CodeVersionTooOld {
				t.Errorf("code %q, want %q", env.Error.Code, v1.CodeVersionTooOld)
			}
			for _, want := range []string{floor, c.version} {
				if !strings.Contains(env.Error.Message, want) {
					t.Errorf("message %q does not name %q", env.Error.Message, want)
				}
			}
			if !strings.Contains(env.Error.NextAction, "yad upgrade") {
				t.Errorf("next action %q does not say what to run", env.Error.NextAction)
			}
			// The refusal came before the burn: the same token registers the
			// runner once it has been upgraded.
			if res, env := post(t, f.floored(floor), "/v1/runners/register", registerBody(t, versioned("r1", floor)), headers(tok)); res.StatusCode != http.StatusOK {
				t.Fatalf("register after upgrade: %d %+v", res.StatusCode, env)
			}
		})
	}
}

// A runner that registered before the floor was raised is turned away at its
// next sync, and that sync changes nothing on the hub.
func TestSyncRefusesBelowMinVersion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tok := f.token(t, time.Hour)
	var reg v1.RegisterResponse
	res, env := post(t, f.hub, "/v1/runners/register", registerBody(t, versioned("r1", "0.3.9")), headers(tok))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register: %d %+v", res.StatusCode, env)
	}
	if err := json.NewDecoder(res.Body).Decode(&reg); err != nil {
		t.Fatal(err)
	}
	if reg.MinVersion != "" {
		t.Errorf("min_version %q from a hub with no floor", reg.MinVersion)
	}
	initial := req("r1", 1)
	d := versioned("r1", "0.3.9")
	initial.Capabilities = &d
	f.mustSync(t, "r1", reg.RunnerCredential, initial)
	before, err := f.store.GetRunner(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}

	f.clock.Advance(time.Minute)
	raised := f.floored("0.4.0")
	body, err := json.Marshal(req("r1", 1))
	if err != nil {
		t.Fatal(err)
	}
	res, env = post(t, raised, "/v1/runners/r1/sync", string(body), headers(reg.RunnerCredential))
	if res.StatusCode != http.StatusUpgradeRequired || env.Error.Code != v1.CodeVersionTooOld {
		t.Fatalf("sync: %d %+v, want 426 %s", res.StatusCode, env, v1.CodeVersionTooOld)
	}
	if !strings.Contains(env.Error.NextAction, "yad upgrade") {
		t.Errorf("next action %q does not say what to run", env.Error.NextAction)
	}
	after, err := f.store.GetRunner(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if after.LastSyncAt != before.LastSyncAt {
		t.Errorf("a refused sync was recorded: last sync %v, was %v", after.LastSyncAt, before.LastSyncAt)
	}
}

// The floor travels to every runner that meets it, so one can say why a hub
// will stop taking it.
func TestMinVersionIsAdvertised(t *testing.T) {
	f := newFixture(t)
	const floor = "0.4.0"
	h := f.floored(floor)
	res, env := post(t, h, "/v1/runners/register", registerBody(t, versioned("r1", "0.9.0")), headers(f.token(t, time.Hour)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register: %d %+v", res.StatusCode, env)
	}
	var reg v1.RegisterResponse
	if err := json.NewDecoder(res.Body).Decode(&reg); err != nil {
		t.Fatal(err)
	}
	if reg.MinVersion != floor {
		t.Errorf("register min_version %q, want %q", reg.MinVersion, floor)
	}
	body, err := json.Marshal(req("r1", 1))
	if err != nil {
		t.Fatal(err)
	}
	res, env = post(t, h, "/v1/runners/r1/sync", string(body), headers(reg.RunnerCredential))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("sync: %d %+v", res.StatusCode, env)
	}
	var sync v1.SyncResponse
	if err := json.NewDecoder(res.Body).Decode(&sync); err != nil {
		t.Fatal(err)
	}
	if sync.MinVersion != floor {
		t.Errorf("sync min_version %q, want %q", sync.MinVersion, floor)
	}
}
