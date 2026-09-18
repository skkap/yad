---
date: 2026-09-19
---

# The daemon lock is a held flock beside the socket

One daemon per profile. Two would both sync as the same runner id, and each
would give up as lost the runs the other holds. The obvious lock is the
control socket itself: listening on it proves a daemon is up. But a daemon
that crashes leaves the socket file behind, and whether a file is "stale" can
only be guessed by connecting to it. Two starts racing each find the file dead,
remove it and listen, and the second quietly takes the path from the first.

So the daemon takes `flock(2)` on `yad.lock` in the data directory for its whole
life, and writes its pid there. The kernel drops the lock however the process
ends, so a held lock means a live daemon and a free one means none. The socket
file is then only for talking: a start that holds the lock removes whatever
socket file it finds, because no live daemon can own it. The CLI checks the
lock (shared, released at once) only when the socket does not answer. That
tells "no daemon" apart from "a daemon too wedged to answer", and gives the
wedged one's pid to `yad daemon stop` without the pid-reuse risk of a pid file,
because the pid is only read while its writer holds the lock. A daemon starting
at the moment a CLI probes retries the lock for half a second before it
concludes another daemon is running.

The socket is `0600`, and the data directory must be `0700` and owned by the
caller, or the daemon refuses to start. bind(2) creates the socket under the
umask before the chmod, so the directory is what keeps it private in between.
A socket path longer than `sun_path` allows (103 bytes on macOS, 107 on Linux)
is refused with the fix: a shorter `YAD_DATA_DIR` or profile.

## Considered options

**The socket alone, probed by connecting.** This is what the task first
described. It has the race above, and a wedged daemon then has no pid anyone
can trust. **A pid file.** It goes stale on a crash, and after pid reuse it
names some other process, which `stop` would then signal. **A shorter socket
path under `$TMPDIR` or `/tmp` when the data directory is deep.** It is
silently somewhere the directory checks do not cover, and in a `/tmp` shared
with other users. Refusing, with the fix named, is one line of the owner's
time.
