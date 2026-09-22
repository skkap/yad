# Trying YAD for real on one machine

This walks through running YAD for real work on the machine in front of you,
and taking it all away afterwards: a `yad hub` on loopback, a runner in a
profile of its own installed as a per-user service, real tasks submitted and
watched, and then every file removed.

It is the step after the README's quickstart. The quickstart shows one run in
your `default` profile with the runner in the background; this is the setup
you would leave running for a few days to see whether YAD fits your work.

**Before you start**

- `yad` is installed (README, *Install*) and `yad doctor` shows the harness you
  mean to use as `ready`. The harness must be installed and logged in on its
  own — `claude`, `codex` — because YAD uses its login and keeps no token.
- You have read *Your responsibility* in the README. A run here can do
  anything you can do on this machine, and every task you submit spends your
  subscription.

## Why a profile of its own

A **profile** is one runner's directories: its identity, its connections and
credentials, its sessions and workdirs, its logs. Two profiles on one machine
share nothing, so a trial in a profile called `dogfood` cannot disturb a runner
you already have, and removing the profile's two directories removes the
trial. They are:

```
~/.config/yad/profiles/dogfood        identity, config.toml, credentials
~/.local/share/yad/profiles/dogfood   state, hub database, workdirs, logs
```

(or under `$XDG_CONFIG_HOME` and `$XDG_DATA_HOME` when those are set). The
hub below keeps its database and admin token in the same profile, so it goes
with it.

`--profile dogfood` goes *before* the command — `yad --profile dogfood status`
— except for `yad service`, which takes it as its own flag.

**While the `dogfood` profile exists, commands YAD prints for your `default`
profile name it:** `yad --profile default daemon start` rather than `yad
daemon start`. With a second profile on the machine, a shell exporting
`YAD_PROFILE` could otherwise send a pasted command to the wrong runner. It is
harmless, and it stops once the profile's directories are gone.

## 1. Check the machine

```bash
yad --profile dogfood doctor
```

Every harness the catalog knows gets a row; the ones marked `ready` are what
this runner can be given. Warnings under the table are about the machine
itself — [run-it-safely.md](run-it-safely.md) says what each one means.

## 2. Start the hub

In a terminal you keep open:

```bash
yad --profile dogfood hub serve
```

It listens on `127.0.0.1:7878` — loopback only; exposing a hub is a deliberate
act — and prints the commands that act on it, each naming the profile.

## 3. Connect the runner

In another terminal:

```bash
yad --profile dogfood hub admin-token create
yad --profile dogfood hub token create |
  yad --profile dogfood connect http://127.0.0.1:7878/v1 --token - --name local
```

The first saves an admin token that `yad hub submit` and `yad hub watch` read.
The second issues a one-time registration token and hands it straight to
`yad connect` through the pipe, so it never appears in your terminal or in
argv; `connect` exchanges it for the runner's credential and saves that.

## 4. Install the runner as a service

```bash
yad service install --profile dogfood
```

On macOS this is a launchd agent, `yad.runner.dogfood`, in
`~/Library/LaunchAgents`; on Linux a systemd user unit,
`yad-runner-dogfood.service`. It runs `yad daemon start --foreground` as you,
never as root, starts it now and at login, and restarts it after a crash.
`install` prints where the unit is, the command it runs, its log file, how long
a stop may take, and the `PATH` it will use.

That `PATH` is your login shell's, read at install time: the harness has to be
on it. After changing `PATH`, upgrading `yad` or editing `[drain] wait`, run
`install` again. On Linux, a user unit stops when you log out unless lingering
is on; `install` tells you, and `loginctl enable-linger` is yours to run.

Check it came up:

```bash
yad service status --profile dogfood
yad --profile dogfood status
```

`status` shows the connection `local` as `syncing`, the capacity free, and no
runs held.

## 5. Submit real work

Each `yad hub submit` prints the run's id and its session, and `--watch`
follows it until it ends, exiting 0 only if it succeeded.

**A question, with no checkout:**

```bash
yad --profile dogfood hub submit --harness claude --model sonnet --watch \
  "What does the POSIX 'sh -e' flag do in a pipeline? Two sentences."
```

**Work on a repository, in a worktree of its own.** `--git` takes an https or
ssh URL, or the absolute path of a repository on this machine; the run gets a
fresh worktree on a branch of its own (`--branch`, or one named after the
session), so your checkout is untouched:

```bash
yad --profile dogfood hub submit --harness claude --model sonnet \
  --git ~/src/myproject --new-session myproject-readme \
  "Read the README and list three things a new contributor would trip on."
```

`--path` is the other kind of source: a directory worked *in place*. The
harness edits your files directly, so point it only at a directory you are
happy to have changed. Either kind is taken only inside the directories
`[workdirs] roots` in the profile's `config.toml` allows — your home directory
when it lists none.

**Continue the conversation.** A session keeps the harness's transcript and
its workdir between runs, so a follow-up names the session and not the source:

```bash
yad --profile dogfood hub submit --harness claude --model sonnet \
  --session myproject-readme --watch "Now fix the first of those three."
```

**Follow, stop, and steer a run by its id:**

```bash
yad --profile dogfood hub watch <run>       # its events as they arrive, then its result
yad --profile dogfood hub cancel <run>      # stop it
yad --profile dogfood hub interrupt <run>   # end its current turn, keep the session
yad --profile dogfood hub steer <run> "use the existing helper instead"
```

## 6. Watch it work

```bash
yad --profile dogfood status          # connections, capacity, runs held, recent errors
yad --profile dogfood sessions        # sessions, their runs and their workdirs
yad --profile dogfood daemon logs -f  # the runner's log as it happens
yad --profile dogfood hub runners     # what the hub knows: health, accounts, usage windows
```

A session you are done with can be closed, which deletes its workdir on the
runner:

```bash
yad --profile dogfood hub close-session myproject-readme
```

## 7. Remove it all

Let the runs in flight end first — `status` shows none held — or accept that
stopping the service cancels them after its drain wait.

```bash
yad service uninstall --profile dogfood       # stops the runner and removes the unit
```

Stop the hub with Ctrl-C in its terminal. Then delete the profile's two
directories, and the `profiles` directories above them if nothing else is in
them — `rmdir` removes only empty directories, so it leaves a `default`
profile you already had exactly as it was:

```bash
rm -rf ~/.config/yad/profiles/dogfood ~/.local/share/yad/profiles/dogfood
rmdir ~/.config/yad/profiles ~/.local/share/yad/profiles \
      ~/.config/yad ~/.local/share/yad 2>/dev/null
ls ~/.config/yad ~/.local/share/yad
```

The last line shows what is left: your own `default` profile if you had one,
or `No such file or directory` for both if you did not. The service log,
workdirs, repository caches, account homes and the hub's database were all
inside the profile's data directory. What YAD cannot take back is what the
runs themselves did — commits pushed or files changed through `--path` —
which are yours to review.
