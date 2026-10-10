---
date: 2026-10-11
---

# A token account's home carries its login, and every account shares the machine's plugins

Amends [0054](0054-a-claude-account-may-be-a-token-and-every-account-shares-the-machines-config.md).
DEV-172 and DEV-174. Both are about the same thing 0054 set out to do: a run
on an account should behave as a run on the machine's own login would, and
each fell short of it in a way nobody saw until a run tried.

## What was found

Read from claude 2.1.296, the binary on the owner's Mac, on 2026-10-11. No
token was spent and nothing logged in: the measurements below use a made-up
token, which the API refuses.

- **Claude Code hands `CLAUDE_CODE_OAUTH_TOKEN` to no child.** It is on the
  list of its own credentials that Claude removes from every command it runs,
  the Bash tool's included. `CLAUDE_CONFIG_DIR` and
  `CLAUDE_SECURESTORAGE_CONFIG_DIR` are not. So a token account's run works,
  and anything it starts that itself calls claude — revmux's reviewers, a
  script — gets the account's home and no login. It answers "Not logged in",
  and revmux quietly runs on its Codex reviewers alone. That is what happened
  to every review in the JapanLaw outsourcing on 2026-10-11.
- **The variable becomes a login object in memory**:
  `{accessToken, refreshToken: null, expiresAt: null, scopes: ["user:inference"], subscriptionType: null, rateLimitTier: null}`.
- **Claude's credential store is "keychain with plaintext fallback".** On
  macOS it reads the Keychain item for the home (0070), and when there is
  none, `.credentials.json` in `CLAUDE_SECURESTORAGE_CONFIG_DIR`, or in the
  home when that is unset. On Linux that file is the whole store. Under
  `claudeAiOauth`, the object above is a login Claude finds there.
- **Claude never saves an inference-only login.** Its save returns early,
  writing nothing, for a login with no refresh token or no expiry. Every
  login Claude writes has a refresh token. A file holding the token's login
  is never rewritten by Claude on its own account, and an inference-only
  login has nothing to refresh.
- **Measured** with a fake token in a temporary home, macOS. Run with only
  the two home variables, as a nested claude is, `claude auth status` says
  `loggedIn: true`, `authMethod: claude.ai`, and `claude --print` gets
  `401 OAuth access token is invalid`. So the file's token is the one sent.
  An empty home says "Not logged in". No Keychain item appeared. The same
  check through this branch's `yad account add claude probe --token -`, in
  a throwaway profile, gave the same answers.
- **The empty `plugins/` in an account's home is Claude's, not yad's.** yad
  never made one. Claude makes `plugins/` with an empty
  `installed_plugins.json` in any home that has none, and 0054's list did not
  name it. `settings.json`, which every account shares, enables the
  machine's plugins by name, so a run on an account had settings naming
  plugins it did not have.
- **`~/.claude/plugins` is the machine's, not a login's.**
  `installed_plugins.json` records absolute install paths under it, and
  marketplaces are git clones. The one part that depends on a login,
  `synced/`, is keyed by organisation and user id. Every claude session on
  the default home already shares the directory at once.

## Decision

**A token account's home carries the token as Claude's own login.** yad writes
`.credentials.json` in the home with the object above under `claudeAiOauth`,
plus a `yadTokenLogin` entry saying that yad wrote it. It is `0600`, written
beside and renamed over, like the token file. The secret is the one already
kept `0600` in the same home, so no new place holds it, and as before nothing
prints, logs or sends it.

- **When.** `SetToken` writes it, and `ClearToken` removes it. Every
  preparation of the home (`account.Ensure`) also brings it in line with the
  token file. So an account stored before this gets it at its next run, a
  token yad will not use (a file others can read) reaches no nested claude
  either, and one that drifted from `0600` is written again.
- **Only yad's own is touched.** That means a file carrying the marker whose
  login has no refresh token. A login Claude made is left alone: say an
  account logged in its own way before it got a token, or a login made over
  yad's, which keeps the marker because Claude keeps entries it does not
  know. Such a login is a working login for a nested claude anyway. So is
  anything yad cannot read as Claude's format, a link, or an unmarked login.
  Other entries Claude keeps in the file, such as an MCP server's OAuth,
  survive a rewrite. To tell whose the file is, yad reads whether the login
  has a refresh token, and nothing else of it.
- **A login made beside a token is checked without it.** That is
  `yad account add` with no `--token`, or a hub's link login. `claude auth
  status` would answer yes to the file just as it does to the variable, so
  `account.OwnLogin` takes yad's login out of the file first. The token
  stays stored, and the next preparation writes the login back while it is.

**Every Claude account shares the machine's `plugins/`**, linked like
`skills/`. 0054 shares "never a file the harness keeps state in" because two
logins' state in one place is wrong. Plugins are state of the machine, which
already has many sessions in it at once, and none of it belongs to a login.

- **An account home's own `plugins/` is replaced only when it is Claude's
  empty default.** That means nothing in it but `installed_plugins.json` and
  `known_marketplaces.json` recording nothing, plus empty directories. The
  files are removed, `rmdir` takes the directory, and fails if something
  was written in meanwhile, and the link is renamed in. A run starting at
  the same moment never finds the path missing. These are the race rules
  of the transcript link (`account.link`).
- **Anything else stays**: a plugin installed for that account, a
  marketplace of its own, a file, or a link the owner made. Each is the
  owner's choice for the account, as a real `skills/` is.

## Considered options

**Document a shell hook instead.** The owner's machine had one in `~/.zshenv`:
it exported the token from `$CLAUDE_CONFIG_DIR/yad-oauth-token` when it was
unset. The ticket allowed this as the fallback "if that is not possible
without writing secrets somewhere unsafe". The file is no less safe than the
token file beside it. The hook works only for a shell that reads that file
(zsh, and none of `sh`, `bash -c` or a script's exec), and it is a step each
machine's owner must know to take. The fix belongs in yad.

**Write the login to the macOS Keychain.** That is the store Claude reads
first, so it would outrank any other login in the home. But it needs
`security add-generic-password` with the secret on stdin, a Keychain that is
unlocked whenever a token is stored (a hub's token login included), and a
second copy that removal must find. The file needs none of that, and Claude
reads it when the Keychain has nothing.

**Point `CLAUDE_SECURESTORAGE_CONFIG_DIR` at a directory holding only the
token's login.** On macOS that moves the Keychain name too (0070), so it is a
different login for every account already logged in, and nothing is gained
over the home's own file.

**Copy `~/.claude/plugins` into each home**, or link only its files. The
install paths in it are absolute and point into the default home, and a
copy goes stale with every plugin update. A link of the whole directory is
what the owner's workaround was, and it is what works.

## Limits

- **On macOS, a token rotation can miss a nested claude.** If Claude ever
  saves something else into a token home's credential store, the save goes
  to the Keychain, the store it tries first. An MCP server's OAuth is one
  such save. When the Keychain had no item, Claude copies the file's
  contents into it and deletes the file. From then on the Keychain item
  outranks the file. The next preparation writes the file again, but after
  a token rotation a nested claude keeps the old token from the Keychain
  until that item is deleted. The run itself is unaffected, since it has the
  variable. The fix by hand is the `security delete-generic-password`
  commands 0070 describes, and the next run puts the login back as the
  file. Clearing the Keychain on every `SetToken` was not done: it can
  fail on a locked Keychain in the middle of a hub's login, and it would
  delete a working login of the owner's.
- **This is Claude's private code**, as 0070's naming is. The line that
  re-checks the store's order against an upgrade:

  ```sh
  LC_ALL=C grep -a -o 'function Jn(){if(W)return W;return M(O,T)}' ~/.local/share/claude/versions/<version>
  ```

  It finds the keychain-with-plaintext-fallback store on 2.1.296. The
  minified names change from release to release, so a miss means read again
  around `Warning: Storing credentials in plaintext.`. It does not mean the
  fallback is gone.
