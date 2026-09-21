package config

import (
	"strings"
	"testing"
)

func TestCheckHubURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://hub.example/yad/v1", true},
		{"https://10.0.0.5:8443/v1", true},
		{"http://127.0.0.1:7777/v1", true},
		{"http://127.0.0.2/v1", true},
		{"http://[::1]:7777/v1", true},
		{"http://localhost:7777/v1", true},
		{"http://LOCALHOST./v1", true},
		{"http://hub.example/v1", false},
		{"http://10.0.0.5/v1", false},
		{"http://ashikaga.tail.ts.net/yad/v1", false},
		{"http://localhost.evil.example/v1", false},
		{"ftp://hub.example/v1", false},
		{"hub.example/v1", false},
		{"", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			if err := CheckHubURL(tc.url); (err == nil) != tc.ok {
				t.Errorf("CheckHubURL(%q) = %v, want ok=%v", tc.url, err, tc.ok)
			}
		})
	}
}

// Whatever CheckHubURL refuses, it says so without the credential a URL may
// carry: the refusal reaches stderr, and from there scripts' kept output. Every
// refusal path is here — a URL that does not parse, one with no host, one with
// the wrong scheme, one in plain http to another host — each with the secret
// in the userinfo, where a URL-shaped credential usually sits, and in the
// shapes that do not parse as userinfo at all.
func TestARefusedHubURLNeverCarriesItsCredential(t *testing.T) {
	const secret = "hunter2"
	for _, raw := range []string{
		"http://runner:" + secret + "@hub.example/v1",
		"http://" + secret + "@hub.example/v1",
		"ftp://runner:" + secret + "@hub.example/v1",
		// No scheme: url.Parse reads "runner" as one and the rest as opaque,
		// so there is no userinfo for a redactor to find.
		"runner:" + secret + "@hub.example/v1",
		secret + "@hub.example/v1",
		// The same two with the @ escaped, which String writes back as given.
		"runner:" + secret + "%40hub.example/v1",
		"https:///runner:" + secret + "%40hub.example/v1",
		// No host: the credential is in the path, not the userinfo.
		"https:///" + secret + "@hub.example/v1",
		// url.Parse refuses both, and its own error quotes the URL.
		"http://runner:" + secret + "@hub.example:port/v1",
		"http://runner:" + secret + "%zz@hub.example/v1",
		"http://runner:" + secret + "@hub.example/v1\x7f",
	} {
		t.Run(raw, func(t *testing.T) {
			err := CheckHubURL(raw)
			if err == nil {
				t.Fatalf("CheckHubURL(%q) accepted it; every case here is one it must refuse", raw)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the refusal carries the credential: %v", err)
			}
		})
	}
}

// A refusal still has to say which URL it refused wherever that can be said
// safely, or the owner is left guessing which of their hubs is wrong.
func TestARefusedHubURLStillNamesItsHost(t *testing.T) {
	err := CheckHubURL("http://runner:hunter2@hub.example/v1")
	if err == nil || !strings.Contains(err.Error(), "hub.example/v1") {
		t.Errorf("the refusal no longer names the hub: %v", err)
	}
}

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		// Nothing to take out: printed exactly as the owner wrote it, not
		// re-rendered, so a status line matches config.toml.
		{"https://hub.example/yad/v1", "https://hub.example/yad/v1"},
		{"HTTPS://Hub.Example/v1/", "HTTPS://Hub.Example/v1/"},
		// Both halves, not the password alone: `https://<token>@host/` puts
		// the credential in the username, and url.URL.Redacted keeps that.
		{"https://runner:hunter2@hub.example/v1", "https://redacted@hub.example/v1"},
		{"https://sk-token@hub.example/v1", "https://redacted@hub.example/v1"},
		// Go's transport errors print a URL whose password it already starred.
		{"https://runner:***@hub.example/v1/runners", "https://redacted@hub.example/v1/runners"},
		{"https://hub.example/v1?token=abc", "https://hub.example/v1?redacted"},
		{"https://hub.example/v1#abc", "https://hub.example/v1#redacted"},
		{"runner:hunter2@hub.example/v1", UnprintableURL},
		{"http://runner:hunter2@hub.example:port/v1", UnprintableURL},
		{"git@github.com:org/repo.git", UnprintableURL},
		{"", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := RedactURL(tc.raw); got != tc.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
