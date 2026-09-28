---
date: 2026-09-28
---

# A hub may add and remove a runner's accounts, unless its owner turns that off for it

[0055](0055-a-hub-may-log-an-account-in-by-link-or-by-token.md) let a hub log
in an account the owner had listed in `config.toml`, and never add one. On a
work machine, a container or any box nobody opens a shell on, that left a
second subscription — and every Codex login — needing a trip to the machine
(DEV-137, DEV-135). The owner decided three things on 2026-09-28.

**A hub-added account is the machine's, like any other.** It is listed in
`config.toml` beside the ones added at the machine, every connected hub's runs
rotate through it by the soonest reset (0039), and `yad account list` shows
it. It does not belong to the connection that added it. The runner stays the
unit of trust: work whose quota must not be shared runs on two runners, and
this decision changes nothing about that.

**Every connected hub may add and remove accounts, and the owner may turn it
off per connection.** A connection's `manage_accounts = false` in
`config.toml` does it; absent means allowed. The reasoning is 0038's and
0055's: the owner trusts the hubs it connects, and a hub could already ask a
harness to edit `config.toml` on a machine where runs are unsandboxed (0036).
Whether this particular hub may add accounts is sent to the hub as a protocol
feature, `accounts`. A runner advertises it only to a connection that is
allowed, and only beside `login`, so a hub knows from the capability document
alone whether to offer adding or removing. A permitted hub may remove any
account, including one the owner added at the machine.

**Adding is a login allowed to create its label; removing is its own
control.** `start_login` and `login_token` gain `add: true`. The runner makes
the account's home, runs the login there, and lists the label in
`config.toml` only once its own login check says yes, as `yad account add`
does (0043). A login that ends without taking lists nothing. `add` naming a
label that is already listed fails, so a mistyped re-login never creates a
second account. A new control, `remove_account`, names a harness and a
label. The hub sends it until the account is gone from the runner's health, or
the runner stops advertising `accounts`. The removal is `yad account remove`'s:
runs on the account finish there and its home goes when the last one lets go.
No new report list: an add ends in `logins`, a removal shows as the account's
absence.

**The daemon now writes `config.toml`'s account lists as well as the CLI.**
Both read the file fresh and change only the one list, under a lock held on
the file, so neither overwrites what the other or a hand edit just wrote. The
rest of `config.toml` is still read once, at start, so changing
`manage_accounts` takes a restart. 0043's rule, that the CLI never writes
`state.db`, is untouched.

**Codex logs in from a hub by device code, and nothing about that is new on
the wire.** A Codex `start_login` is a `link` login whose `waiting` report
carries `url` and `user_code`. No code comes back through the hub, so the hub
sends no `login_code`. The runner drives Codex's app-server login
(`account/login/start` with `chatgptDeviceCode`), which answers with
structure rather than wording. yad's own login check (`codex login status`)
still decides whether the login took.

## Considered options

**An account belongs to the hub that added it.** Protects one hub's quota from
another on the same runner, a case two runners already cover. The cost is a
new relationship between accounts and connections, and account choice,
capacity and waiting runs would all have to learn it.

**Opt-in per connection.** Safer against a mistake that spends a
subscription, but it is the gate 0055 declined, and every existing runner
would need a trip to the machine before its hub could use this.

**A hub removes only what it added.** Needs every account to record which
connection added it (a structured entry in `config.toml`, an `origin` field
on the wire) to protect accounts from hubs the owner already trusts.

**Separate `add_account` control with its own report list.** It keeps
*account* and *login* apart on the wire. But an add is still a login inside,
so a hub would follow two report lists for one flow.

**A login for an unlisted label creates it, with no flag.** The smallest wire
change. It also means a typo in a re-login creates a second account as soon
as someone finishes signing in.
