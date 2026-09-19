package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
)

// Connect registers this runner with the hub at hubURL, exchanging the
// one-time registration token for a runner credential, and records the
// connection: the credential in credentials/<name> at 0600, the connection in
// config.toml. The token is used for the one request and kept nowhere.
//
// Connecting again under the same name and URL registers again, with a token
// the hub issued for this runner, which gives the runner a fresh credential —
// the recovery for a lost or leaked one.
func Connect(ctx context.Context, p config.Paths, hubURL, token, name string) (config.Connection, v1.RegisterResponse, error) {
	var none v1.RegisterResponse
	token = strings.TrimSpace(token)
	if token == "" {
		return config.Connection{}, none, errors.New("no registration token — create one at the hub (`yad hub token create`, or its Add runner) and pass it with --token")
	}
	if err := config.CheckHubURL(hubURL); err != nil {
		return config.Connection{}, none, err
	}
	if name == "" {
		name = NameFromURL(hubURL)
	}
	cfg, err := config.Load(p)
	if err != nil {
		return config.Connection{}, none, err
	}
	conn := config.Connection{Name: name, URL: hubURL}
	existing := -1
	for i, c := range cfg.Connections {
		switch {
		case c.Name == name && c.URL != hubURL:
			return conn, none, fmt.Errorf("connection %q already points at %s — pick another --name for this hub", name, c.URL)
		case c.Name != name && c.URL == hubURL:
			return conn, none, fmt.Errorf("this runner is already connected to %s as %q — a runner has one connection per hub; to register it again, run `yad connect %s --name %s --token <new token>`", hubURL, c.Name, hubURL, c.Name)
		case c.Name == name:
			existing, conn = i, c
		}
	}
	// Checked before the token is spent: a config that cannot be saved would
	// otherwise burn the token and leave the hub with a runner nobody can use.
	next := cfg
	next.Connections = append([]config.Connection(nil), cfg.Connections...)
	if existing < 0 {
		next.Connections = append(next.Connections, conn)
	}
	if err := next.Validate(); err != nil {
		return conn, none, err
	}

	if err := p.Ensure(); err != nil {
		return conn, none, err
	}
	id, err := p.RunnerID()
	if err != nil {
		return conn, none, err
	}
	client, err := hubclient.New(hubURL, "")
	if err != nil {
		return conn, none, err
	}
	// A runner registers with the accounts the owner configured, whatever
	// state they are in: a harness whose accounts all need login is still a
	// runner a hub should know about.
	// A state database one migration behind — this binary newer than the
	// daemon that owns it — must not stop a registration. cmd_daemon takes the
	// same failure the same way, and capability.Build falls back to reporting
	// the owner's configured labels as free.
	accounts, err := account.Read(ctx, p, cfg)
	if err != nil {
		accounts = nil
	}
	res, err := client.Register(ctx, token, v1.RegisterRequest{Capabilities: capability.Build(ctx, id, cfg, accounts)})
	if err != nil {
		return conn, none, fmt.Errorf("register with %s: %w", hubURL, err)
	}
	if res.RunnerCredential == "" {
		return conn, none, fmt.Errorf("%s answered register without a runner credential — it is not a working YAD hub", hubURL)
	}
	if err := p.SaveCredential(name, res.RunnerCredential); err != nil {
		return conn, none, fmt.Errorf("the hub registered this runner but the credential could not be saved (%w) — fix the config directory and connect again with a new token", err)
	}
	if existing < 0 {
		if err := config.Save(p, next); err != nil {
			// A credential with no connection naming it is a secret nobody
			// will use or clean up.
			return conn, none, errors.Join(fmt.Errorf("save %s: %w — connect again with a new token", p.ConfigFile(), err), p.DeleteCredential(name))
		}
	}
	return conn, res, nil
}

var notName = regexp.MustCompile(`[^a-z0-9_-]+`)

// NameFromURL derives a connection name from the hub's host, so
// `yad connect https://zumino.cc/api/yad/v1` needs no --name.
func NameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "hub"
	}
	name := strings.Trim(notName.ReplaceAllString(strings.ToLower(u.Hostname()), "-"), "-_")
	if len(name) > 64 {
		name = strings.TrimRight(name[:64], "-_")
	}
	if name == "" {
		return "hub"
	}
	return name
}
