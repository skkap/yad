---
date: 2026-10-16
status: accepted
---

# A browser is a host tool

Host tools were git, gh and docker, and only those three (YAD-31): what a run
needs from the machine before it can do anything, with anything else
installed by the run in its own workdir. Zumino's outsourcing (ZUM-128) sends
verifier runs that take before-and-after screenshots and run Playwright
suites, and a hub routing those has to know which runners can. A run cannot
install a browser in its workdir on a machine whose owner has not: Chrome's
installer is the system's, and a Playwright download is hundreds of
megabytes and system libraries that a run on Linux is not root enough to
add. YAD-201.

## Decision

**`browser` is a fourth entry in `host_tools`**, reported after git, gh and
docker, present or not, under the same rule a hub already reads: usable when
`present` with no `error`.

**One entry, with a `kind`: `chrome`, `chromium` or `playwright`.** The first
found, in that order. A hub routing "needs a browser" wants one answer; a
list of every browser on the machine is a second shape for a hub to learn,
for a question none has asked. `kind` is a new optional field on `HostTool`,
set for the browser alone, so a hub generated before it ignores it. It is a
documented open set rather than an enum: v1's enums are closed
([0047](0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md)), and a browser yad learns to find later
would otherwise need a feature advertised before a hub may be told of it.

**Where it is looked for.** Chrome and Chromium by name on `PATH`, then at the
standard install locations for macOS (`/Applications` and `~/Applications`),
Linux (`/opt/google/chrome`, `/usr/bin`, `/snap/bin` — what a daemon a
service manager started with a bare `PATH` still finds) and Windows
(`Program Files`, `LOCALAPPDATA`). Then Playwright's cache:
`PLAYWRIGHT_BROWSERS_PATH`, or its per-OS default, holding a browser
directory Playwright finished installing (`INSTALLATION_COMPLETE`). Edge,
Brave and the other Chromium builds are not looked for until a hub has a
reason to want one.

**The version is read, not asked, where the install states it.** From the app
bundle's `Info.plist` on macOS — Chrome there takes about four seconds to
answer `--version`, most of the probe's five — and from the version-named
directory beside `chrome.exe` on Windows, where `--version` opens a window.
On Linux and for a browser found on `PATH`, `--version` is asked, through the
same supervised probe as the other tools. For Playwright, the version of its
newest Chromium, where that can be read; a cache of only Firefox or WebKit is
present with no version. A version that cannot be read is no error.

**No display is probed.** What a run does with a browser on a runner is
headless, which needs none, and a display check would be a fact about the
desktop session the daemon happens to run in rather than about the machine.

**No `YAD_BROWSER_PATH`.** The three binaries take an override because each is
one binary a run is handed by name (0045). A browser is not — a harness's
Playwright finds its own, a script names its own Chrome — so an override
would name a file nothing uses. A browser outside the standard places is found
on `PATH`.

## Consequences

- Every runner's document gains a `browser` entry once, so its fingerprint
  moves once on upgrade.
- Detection costs one spawn at most, and only on Linux or for a browser on
  `PATH`; on macOS and Windows it is a few stats and one small file read.
- `yad doctor` now lists every host tool beside the harnesses, with the path
  each was found at — the document leaves the path out.
- The catalog that `YAD_<ID>_PATH`, `Locate` and the run's links read is still
  the three binaries; the browser is reported beside it, not in it.
