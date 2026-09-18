package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// CheckHubURL accepts a hub's base URL only where a bearer secret can travel
// safely: https anywhere, or plain http to this machine alone. Every request to
// a hub carries the registration token or the runner credential, and a
// cleartext hop to another host hands either to anyone on the path. Loopback
// stays allowed because `yad hub serve` binds there and prints an http URL.
func CheckHubURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("hub URL %q must be an absolute https URL, like https://hub.example/yad/v1", raw)
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return fmt.Errorf("hub URL %q is plain http to another host, which would send the runner credential in cleartext — use https (plain http is allowed only for localhost)", raw)
	}
	return nil
}

func loopback(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
