---
date: 2026-09-21
---

# Runner credentials stay in 0600 files; the OS keychain is not used, for now

The owner decided to skip storing runner credentials in the OS keychain (macOS
Keychain, Secret Service on Linux) for the time being. The backlog item for it,
DEV-51, is closed as won't-do rather than left open, so nothing in the backlog
suggests the storage model is about to change.

What stays is what [0010](0010-registration-token-exchanged-for-runner-credential.md)
and [0009](0009-machine-owns-credentials-hubs-grant-per-run.md) already say: a
runner credential is a `0600` file in its profile's data, and a grant is a
`0600` file that lives only as long as its run. `yad doctor` checks both
(`internal/config/exposure.go`), and that check is the protection the owner
relies on.

## Why not now

- **A runner is a background service** ([0028](0028-a-runner-is-a-per-user-service-with-its-login-path.md)):
  a launchd agent or a systemd `--user` unit, often started with no one logged
  in at the screen. A keychain there may be locked, may prompt, or, on a
  headless Linux machine, may have no Secret Service at all. A runner that
  cannot read its credential cannot sync, so a keychain store would need a file
  fallback anyway, and then there are two storage paths to secure and test
  instead of one.
- **It adds a dependency** to the curated list (ARCHITECTURE.md §6) for a gain
  the exposure check already covers in the common case: a file only its owner
  can read, on a machine the owner controls.
- **Grants would not move regardless.** They are per-run and deleted when the
  run ends; the keychain question is only ever about the long-lived runner
  credential.

## Revisit when

An owner needs the credential encrypted at rest on a machine they share, or a
platform keychain can be read reliably by a service running with nobody logged
in. Either one is a new decision record, not an edit to this one.

## Considered options

**The keychain where available, with a `0600` file as the fallback.** It gives
the benefit only on machines with a usable keychain and doubles the storage
paths everywhere. **The keychain only.** It strands a runner on any machine
without one, which includes most headless Linux hosts.
