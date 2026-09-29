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
  built with `shellword.Command` and naming `/usr/bin/security` by its path,
  as it is run. A home and its Keychain login go together or not at all.
  `config.toml` has already changed by then, by design (0043), so no new
  run takes the account meanwhile, and no report names it either, so a hub
  that asked stops asking. Three things finish it: `yad account remove`
  again (a no-op on `config.toml`, which still finishes the rest), a hub
  repeating the removal, and the daemon's next start, which finishes every
  home a removal left set aside or marked (`account.Unfinished`; the marker
  is the amendment below). Deleting the items by
  hand and letting the next start delete the set-aside home is the fourth.
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
  A claude older than the variable ignores it. Every command yad prints for
  an account's login — a check in an error, a next action in health —
  carries both variables (`account.HomeVars`), because in a shell that
  exports the second, one naming only the home asks about another login.

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

## A removal outlives the daemon: the marker (DEV-160)

Added 2026-09-29. Found by review on DEV-134. The start sweep above found
only homes already set aside. Two removals never get that far:

- one waiting for a run still in the home, which `runner.Accounts` holds as
  an in-memory `doomed` flag until the last run lets go;
- one whose rename out of the account's path (`SetAside`) failed.

In both, `config.toml` has already dropped the label. If the daemon dies
then — a crash, SIGKILL, a power cut — the home stays at its path under a
label nothing lists, and its Keychain login stays with it. The next start
finds no set-aside copy, and a hub's repeat finds nothing to finish. The
owner is never told.

Deleting every unlisted home at start would be wrong. A `yad account add`
whose login is still running, or was walked away from, keeps an unlisted home
on purpose for the next try (0043).

- **Every removal marks the home first.** It writes `.yad-removing`, 0600, at
  the top of the home, synced along with its directory entry. This happens
  under `config.toml`'s lock and before the write that drops the label
  (`account.Unlist`). A marker that cannot be written leaves the file
  unchanged and the removal fails. So there is never a moment when the label
  is gone and the home carries no marker. This covers every path:
  `yad account remove` with or without a daemon, which marks whatever home it
  finds because the owner asked for it to go; a hub's `remove_account`, which
  marks only a label something lists; and the daemon's own `Reload`, which
  marks again under the lock that dooms the home.
- **The daemon's start finishes every marked home `config.toml` does not
  list.** `account.Unfinished` names it, and the home is set aside and
  deleted, Keychain first, exactly as any other removal. Nothing can hold the
  home at start. A hub repeating the removal finishes it the same way, unless
  a run is still in the home. The owner running `yad account remove` again
  also finishes it.
- **A home without the marker is never touched.** The sweep reads only the
  marker, never the absence of a label. That keeps a pending add's home safe.
- **A marked home whose label is still listed was never removed.** The
  removal died between the marker and the write. `config.toml` is the record
  of what is listed, so the account keeps its home and its runs. The start
  takes the marker off and logs that the removal did not happen, so a later
  hand edit of the file is not read as this removal. Running the removal
  again removes it.
- **The start decides by `config.toml` as it reads then, under its lock.**
  It does not use the lists the daemon loaded before its socket opened, since
  both can have changed in that gap. A `yad account remove` may have marked a
  home and dropped its label, and be waiting for the daemon to answer. That
  removal is in flight: the label is still in the daemon's lists, so the
  start leaves it for the `Reload` it is waiting on, and its marker stays. A
  `yad account add` or a hand edit may have listed a label again. The file
  lists it, so its home is kept. The lock is the one `Unlist` holds from the
  marker to the write, so the start never sees a removal half-done.
- **An add takes the marker off.** This covers `yad account add` (the
  daemon's `Keep`, and the CLI itself when no daemon runs), a hub's add, and a
  reload that lists the label again. Each does it with the in-memory flag,
  before the login, so a daemon that dies during the login does not delete
  the home at its next start. The unlink is synced, as the marker is. An
  unlink that cannot be synced is undone, so an error always means the
  marker is still there. An add whose marker will not come off does not
  begin, and the removal it would have cancelled stands whole. The same goes
  for a daemon that refuses `Keep`, and for one that holds the lock and does
  not answer it (DEV-167, 0043). With no daemon running, the CLI takes the
  marker off itself, under
  `config.toml`'s lock (`account.Reclaim`), so a daemon starting at that
  moment never looks at the marker and moves the home in the gap between.
- **The marker stays out of the harness's way.** Neither Claude nor Codex
  uses the name. It sits at the top of the home and is always a new file,
  written under a fresh `O_EXCL` name and renamed over whatever held the
  name. That replaces a symlink, a hard link or a FIFO without touching what
  it led to. It also never leaves the name empty, so marking a home again
  never unmarks it for a moment. Nothing the home links to — the shared transcripts, the
  machine's shared config (0054) — is named like it, so it never reaches
  another account or the owner's own harness. A run making its home again
  (`Ensure`) leaves it where it is: only an add takes it off.

A run the killed daemon left behind, a harness process still in the home, has
its home deleted under it at the next start. The daemon that started that run
is gone, and the next start reports the run lost (0030). A removal the owner
asked for is not held back for a process nothing supervises any more.

### Considered options

**Move the home aside at once and let runs keep the old path open.** The
ticket's first direction, and it needs nothing on disk but the rename. It was
rejected because a running claude keeps paths under its home open and goes on
creating files there by path. Moving the directory from under it is a change
of behaviour nobody has measured, and the marker needs no such measurement.

**Record the removal in `state.db`.** The CLI never writes `state.db` (0043),
and `yad account remove` is one of the writers that must record it.

**Delete every unlisted home at start.** This deletes a pending add's home,
which 0043 keeps on purpose.

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
