---
date: 2026-09-29
---

# A Claude account's Keychain login goes with its home

Amends [0039](0039-accounts-log-in-themselves-and-the-soonest-reset-goes-first.md)
and [0060](0060-the-owners-own-account-variables-are-removed-from-every-run.md).
DEV-134.

0039 says an account's login lives in its home, so that removing the home logs
the account out of the machine. On Linux that is true. On macOS it is not.
While testing hub login (DEV-133), an account removed with
`yad account remove claude hubtest --yes` and listed again in `config.toml`
read free, with no credential file in the re-created home. The owner read
claude 2.1.284's own code on 2026-09-29 and found why:

- **On macOS Claude keeps a `CLAUDE_CONFIG_DIR` home's login in the user's
  login Keychain**, as a generic password. Its service name is
  `Claude Code-credentials-<h>`, where `<h>` is the first eight hex digits
  of the SHA-256 of the config directory as Claude was given it,
  NFC-normalised. The account attribute is `$USER`, or the user's name when
  `$USER` is empty, or `claude-code-user` when that is not
  `[a-zA-Z0-9._-]+`. With `CLAUDE_CONFIG_DIR` unset there is no suffix at
  all, which is the owner's own login.
- **A second item, `Claude Code-<h>`, holds an API key**, the one a Console
  login stores, and the legacy key Claude still reads. It is named from the
  home the same way.
- **`CLAUDE_SECURESTORAGE_CONFIG_DIR`, when set, replaces the config
  directory in that hash.** Set but empty, it means the default login, with
  no suffix. The same variable moves the plaintext `.credentials.json` that
  Claude writes where there is no Keychain: on Linux it is written under
  `CLAUDE_SECURESTORAGE_CONFIG_DIR` instead of the home.
- **On Linux the login is `.credentials.json` inside the home**, measured in
  DEV-26. Deleting the home deletes it, and no keyring is consulted.

So deleting a home on macOS leaves its login in the Keychain. A home made
again at the same path — the same label, added again — finds that login, and
the new account runs on the old subscription while `yad account list` calls it
free.

## Decision

- **A Claude home's Keychain items are part of the home.** Where a removal
  finishes, `account.RemoveSetAside`, the items go with it:
  `/usr/bin/security delete-generic-password -s <service> -a <account>` for
  each of the two. This one place covers every removal. `yad account remove`
  with no daemon runs through it, since `account.Remove` now sets the home
  aside first as the daemon does. So does the daemon's removal for the owner
  or for a hub's `remove_account`
  ([0057](0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)),
  whether it happens at once or when the last run lets go. An item that is not
  there (exit 44) counts as done.
- **The Keychain goes first.** If `security` fails for any other reason — a
  locked Keychain, say — the set-aside home is kept. The error names the
  cause and gives each `security` command that deletes an item by hand,
  built with `shellword.Command`. The next removal of the label tries both
  again: `yad account remove` on a label `config.toml` no longer lists is a
  no-op there and still finishes the rest. A home and its Keychain login go
  together or not at all. `config.toml` has already changed by then, by
  design (0043), so no new run takes the account meanwhile.
- **A new home starts with no Keychain login.** `account.Prepare`, which
  `Ensure` is, runs every time a home is made where there was none. On macOS
  it deletes any Claude item for that path before the directory exists: a
  login with no home can only be a leftover, from before this fix or from a
  removal that died half-way. This is the check that makes old leftovers
  harmless. An item it cannot delete stops the home being made, because a
  home there would run on it. A home that is already there keeps its login.
- **`yad account add` says so when it clears one.** A hub's add logs it. The
  owner may have removed the label expecting that login to survive somewhere,
  and a login they did not expect to need is the worse way to find out. The
  line prints before the login starts.
- **A removal that finishes after the label is back leaves the Keychain
  alone.** `RemoveSetAside` deletes the items only while no home is at the
  account's path. If one is there, the label was added again, `Prepare`
  cleared the leftover then, and the item for that path is the new home's
  login.
- **Tests never reach the real Keychain.** `security` is a package variable
  that `internal/account`'s tests point at the test binary playing it. Each
  test asserts the exact argv, and a printed command is run through `sh`. In
  any other test binary (`testing.Testing()`), a call that would reach
  `/usr/bin/security` does nothing, so no package's test can touch the
  owner's Keychain by accident. The one manual check, on 2026-09-29, used a
  throwaway item made for a temporary path. `yad account add` deleted it and
  printed the line above.
- **`CLAUDE_SECURESTORAGE_CONFIG_DIR` joins DEV-62's list, and every account
  sets it to its own home.** On the list, a hub may not grant it (0040). Set
  on the machine, it would point every account at one login. It passes
  `supervise.Scrub` the way `CLAUDE_CONFIG_DIR` does, and every account's
  environment (`account.TurnEnv`, `account.LoginEnv`) appends it with the
  same path as the home, after the scrub, so the account's value wins. With
  no accounts, the owner's copy is where the harness's own login is. An owner
  who points `CLAUDE_CONFIG_DIR` elsewhere and sets this variable empty to
  keep the default Keychain login would otherwise find every no-account run
  logged out. That is 0060's reason for letting the home variables through,
  and it applies unchanged. With accounts, the value appended is the path
  already in `CLAUDE_CONFIG_DIR`, so the Keychain name and the file location
  are exactly what they were without it, and existing logins are untouched.
  A claude older than the variable ignores it.

## Considered options

**Scrub `CLAUDE_SECURESTORAGE_CONFIG_DIR` and never set it.** This keeps
accounts safe too, since the owner's copy is gone and the name comes from
`CLAUDE_CONFIG_DIR`. It is what the ticket first proposed. It was rejected
because it quietly moves a no-account runner off a login its owner configured
with this variable. 0060 rejected the same thing for `CLAUDE_CONFIG_DIR`,
for the same reason.

**Delete by service name alone, with no `-a`.** That would catch an item
filed under another account attribute, say one made while `$USER` was
different. It was not chosen because a claude started by this runner reads
only the item under the account computed here. That is the item that can
log an account in as someone else, and deleting exactly it is the claim
this record makes. It is also the command the ticket asked for.

**Delete the home first and report a Keychain failure afterwards.** Fewer
moving parts, but it leaves the state that caused the bug, a login with no
home, as the normal result of a failed step. With the home kept until the
Keychain is clear, that state arises only from outside yad, and `Prepare`
clears it the next time a home is made there.

**Normalise the path to NFC before hashing, as Claude does.** That needs
`golang.org/x/text` as a direct dependency (the standard library's copy is
vendored and cannot be imported). NFC changes nothing in an ASCII path.
macOS account names, and so the homes under them, are ASCII, and every
path yad makes is under one unless the owner moved the data directory
somewhere spelled in decomposed Unicode. That case is named in
`KeychainServices` and not handled.

**Codex.** It keeps `auth.json` in `CODEX_HOME` unless that home's
`config.toml` chooses the keyring, and yad never writes one that does
(0054 shares only `AGENTS.md` and `prompts/`). Nothing is done for it here.

## Re-checking on a claude upgrade

The naming is Claude's private code, not a documented interface, and a
release can change it. The one line that settles it, against the binary the
owner runs (`~/.local/share/claude/versions/<version>` for the native
install):

```sh
LC_ALL=C grep -a -o 'process.env.CLAUDE_SECURESTORAGE_CONFIG_DIR,[^;]*;return`Claude Code[^`]*`' ~/.local/share/claude/versions/2.1.284
```

On 2.1.284 it prints the hash of the NFC-normalised directory, cut to
eight hex digits, and ``return`Claude Code${…OAUTH_FILE_SUFFIX}${n}${o}` ``,
where `n` is `-credentials` for the login and empty for the API key.
`OAUTH_FILE_SUFFIX` is empty for the production login. The account attribute
is `function …(){let n;try{n=process.env.USER||…().username}…` beside it.
If the grep prints nothing, the function has moved: the pinned values in
`internal/account/keychain_test.go` and this record are then out of date
until someone reads it again.
