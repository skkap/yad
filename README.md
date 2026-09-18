# YAD

**One process per machine that runs coding agents for something else.**

YAD sits on a Mac or a Linux box, notices which coding-agent CLIs are installed,
tells a control plane what it can do, and then runs those agents when the control
plane asks — against a named session, with a named agent, on a named model.

It is the half that is missing from both of the things it serves:

- **[zumino](../zumino)** has the queue and no way to execute anything. Its README
  is explicit that agents are ordinary participants and that there is *no
  executor registry*.
- **[yashiki](../yashiki)** runs `claude -p --resume` on the one machine it is
  installed on, and cannot reach any other.

YAD is deliberately not a tracker, a UI, or an orchestrator. It claims work,
runs a process, streams what happened, and says whether it is still alive.

```
$ yad doctor
AGENT               STATUS      VERSION                PATH
Claude Code         ready       2.1.276 (Claude Code)  /Users/me/.local/bin/claude
Codex               ready       codex-cli 0.147.0      /Users/me/.local/bin/codex
Gemini CLI          no adapter  0.29.2                 /…/bin/gemini
GitHub Copilot CLI  —
OpenCode            —
Cursor Agent        no adapter  2025.09.12-4852336     /Users/me/.local/bin/cursor-agent

2 agent(s) this runner can be given work for.
```

## Status

**M0 — scaffolding, working.** Detection, the capability document, runner
identity and a heartbeat loop that talks to nobody. No protocol yet, no process
supervision yet, no sessions yet. `DESIGN.md` has the shape and the order.

## Build

```bash
make check      # gofmt, vet, test, build — the whole bar, see CHECKS.md
make build      # ./bin/yad
make install    # ~/.local/bin/yad
make dist       # linux/amd64, linux/arm64, darwin/arm64
```

Go 1.27, standard library only. No dependencies is a choice, not an accident —
this binary is copied onto machines by hand for now, and every dependency is
something that has to be audited on each of them.

## Commands today

| | |
|---|---|
| `yad doctor` | what is installed here, and what YAD can drive |
| `yad agents` | the capability document, exactly as a control plane would receive it |
| `yad daemon start --foreground` | the runner loop: identity, re-probe, heartbeat, clean shutdown |
| `yad version` | version and commit |

Everything else — `run`, `sessions`, `config`, `service`, `setup` — refuses with
the milestone that will bring it.

## The name

YAD. That is all it is.

## Prior art

Read before adding anything; most of this problem is solved somewhere.

| | What it is |
|---|---|
| **Anthropic self-hosted environments** | The official version of this idea, and the source of the word *runner*: long-lived processes that pick up sessions and start one Claude Code process each, in fixed or on-demand pools. Claude only |
| **[multica](https://github.com/multica-ai/multica)** — `~/projects/multica` | The closest complete implementation: `multica daemon` detects CLIs, registers a runtime per agent per workspace, polls every 3s, heartbeats every 15s, GCs workspaces. ~14k lines of Go adapters in `server/pkg/agent`, modified Apache-2.0. **The reference for every adapter written here** |
| **Paseo** | Self-hosted TypeScript/AGPL daemon running Claude Code, Codex, Copilot, OpenCode and Pi in parallel, driven from desktop/web/mobile |
| **Open Session**, **claude-code-runner**, **Agents Anywhere** | Self-hosted control planes driving sessions in git worktrees |
| **ACP** (Agent Client Protocol) | The emerging agent↔client protocol. Multica already uses it for kiro, qoder and trae; it is the likely answer to "must we write an adapter per CLI forever" |
