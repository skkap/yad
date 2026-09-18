package config

import "testing"

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
