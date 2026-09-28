package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// deviceLogin starts the fake app-server playing a device-code login in a
// home of its own, and returns it with what the fake saw.
func deviceLogin(t *testing.T, name string) (*DeviceLogin, *harness, string) {
	t.Helper()
	h := &harness{fixture: fixture(name)}
	spec := h.spec(t)
	home := t.TempDir()
	d, err := StartDeviceLogin(context.Background(), os.Args[0], t.TempDir(), append(spec.Env, "CODEX_HOME="+home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(time.Second) })
	return d, h, home
}

func begin(t *testing.T, d *DeviceLogin) (DeviceCode, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return d.Begin(ctx)
}

func completed(t *testing.T, d *DeviceLogin) {
	t.Helper()
	select {
	case <-d.Completed():
	case <-time.After(10 * time.Second):
		t.Fatal("the login never completed")
	}
}

// The device-code login over the app-server: the link and the code Codex
// answers with, and its completion, success or not — each read from
// structure, as the schema gives it.
func TestADeviceLoginReportsItsCodeAndItsCompletion(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		success bool
	}{
		{"login-device", true},
		{"login-device-refused", false},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			d, h, home := deviceLogin(t, tc.fixture)
			dc, err := begin(t, d)
			if err != nil {
				t.Fatal(err)
			}
			if dc.URL != "https://auth.openai.com/codex/device" || dc.UserCode != "K7QM-4XPD" {
				t.Errorf("the device code is %+v", dc)
			}
			completed(t, d)
			if d.Succeeded() != tc.success {
				t.Errorf("succeeded = %v, want %v", d.Succeeded(), tc.success)
			}
			d.Close(5 * time.Second)
			if p := h.seen(t).params(t, "account/login/start"); p["type"] != "chatgptDeviceCode" {
				t.Errorf("the login started with %v", p)
			}
			if _, err := os.Stat(filepath.Join(home, "auth.json")); (err == nil) != tc.success {
				t.Errorf("the credential is there: %v, want %v", err == nil, tc.success)
			}
		})
	}
}

// A login given up on is cancelled in Codex too, by the id it gave, so an
// owner who types the code late logs nothing in.
func TestADeviceLoginIsCancelledByItsID(t *testing.T) {
	d, h, _ := deviceLogin(t, "login-device-cancel")
	if _, err := begin(t, d); err != nil {
		t.Fatal(err)
	}
	if err := d.Cancel(); err != nil {
		t.Fatal(err)
	}
	d.Close(5 * time.Second)
	if p := h.seen(t).params(t, "account/login/cancel"); p["loginId"] != "5f0d3c9e-7a41-4b8e-9d2a-1c6e8f3b2a70" {
		t.Errorf("the cancel named %v", p)
	}
}

// A codex whose app-server has no device-code login answers with an error,
// which comes back as one — never a login waiting for ever.
func TestACodexWithoutDeviceLoginSaysSo(t *testing.T) {
	d, _, _ := deviceLogin(t, "login-device-unsupported")
	_, err := begin(t, d)
	if _, ok := errors.AsType[*RPCError](err); !ok {
		t.Fatalf("Begin = %v, want the app-server's error", err)
	}
}

// What leaves the app-server for a hub is a link and a short code: anything
// else in their place is no device code at all.
func TestOnlyAUsableDeviceCodeIsTaken(t *testing.T) {
	for _, tc := range []struct {
		url, code string
		ok        bool
	}{
		{"https://auth.openai.com/codex/device", "K7QM-4XPD", true},
		{"http://auth.openai.com/codex/device", "K7QM-4XPD", false},
		{"https://auth.openai.com/codex/device", "", false},
		{"https://auth.openai.com/codex/device", "K7QM\n4XPD", false},
		{"https://auth.openai.com/codex/device\nhttps://elsewhere", "K7QM-4XPD", false},
		{"", "K7QM-4XPD", false},
	} {
		if got := usableURL(tc.url) && usableCode(tc.code); got != tc.ok {
			t.Errorf("%q %q usable = %v, want %v", tc.url, tc.code, got, tc.ok)
		}
	}
}
