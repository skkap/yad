#!/bin/sh
# Installs yad into ~/.local/bin from the newest tagged release.
#
# The repository is private (decided 2026-09-19), so there is no URL to curl
# without a token — for the binaries or for this script. `gh` does the
# fetching, and the login you already have is what grants access:
#
#   gh api -H "Accept: application/vnd.github.raw" \
#     repos/skkap/yad/contents/scripts/install.sh > yad-install.sh &&
#     sh yad-install.sh
#
# The && is the point. A pipeline reports only the status of its last command,
# so a gh that cannot fetch this file — not installed, not logged in, no
# network — hands sh an empty stream and sh exits 0 having installed nothing;
# a bare redirection followed by a separate `sh` does the same, because the
# file is created whether gh ran or not. That is the one failure a chained
# provisioning script reads as success, and it is the case this file's own
# "gh is not on PATH" refusal can never reach, since gh is what fetched it.
# The file is kept because the YAD_VERSION example below re-runs it, not as an
# inspection point — the && runs it the moment the fetch succeeds. To read it
# first, fetch and run separately and check gh's own exit status, since the
# redirection creates the file either way.
#
# Fetch it with gh rather than curl: a curl carrying `Authorization: Bearer
# $(gh auth token)` puts the live token in curl's argv, where /proc and ps
# hand it to every local account for the length of the request. gh reads the
# same token from its own keyring and never passes it as an argument.
#
# YAD_VERSION pins a release, YAD_INSTALL_DIR moves where it lands, and
# YAD_REPO points at a fork — set the same YAD_REPO for `yad upgrade` later,
# since nothing records where the binary came from:
#
#   YAD_VERSION=v0.3.1 sh yad-install.sh
#
# Nothing here restarts anything: a runner already running keeps the binary it
# started from until someone restarts it.
set -eu

REPO="${YAD_REPO:-skkap/yad}"
DIR="${YAD_INSTALL_DIR:-$HOME/.local/bin}"

die() { echo "install: $*" >&2; exit 1; }

case "$(uname -s)" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *)      die "yad releases are built for Linux and macOS only, not $(uname -s) — build it from source with \`make install\`" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *)             die "yad releases are built for amd64 and arm64 only, not $(uname -m) — build it from source with \`make install\`" ;;
esac
asset="yad-$os-$arch"

command -v gh >/dev/null 2>&1 ||
  die "the GitHub CLI \`gh\` is not on PATH, and $REPO is private — install gh (https://cli.github.com) and run \`gh auth login\`"
# No `gh auth status` gate: it exits non-zero when an account on *any* host has
# a problem, so a stale GitHub Enterprise entry refuses an install that would
# have worked. The calls below fail on their own and say why.

# One of these two exists on every Linux distribution and every macOS; without
# one there is no way to check what was downloaded, and an unchecked binary is
# not worth installing.
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }  # a pipeline's status is cut's, so the caller checks the hash is non-empty
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "neither sha256sum nor shasum is on this machine, so the download cannot be verified"
fi

tmp=$(mktemp -d)
staged="$DIR/.yad.install.$$"
# A signal handler that does not exit returns to where it interrupted, so the
# script would clean up and then carry on against the directory it just
# removed. 130 and 143 are the conventional statuses for the two signals.
trap 'rm -rf "$tmp" "$staged"' EXIT
trap 'rm -rf "$tmp" "$staged"; exit 130' INT
trap 'rm -rf "$tmp" "$staged"; exit 143' TERM

# The directory is made and tested before the download, so an install into a
# place this account cannot write says so in a second rather than after
# fetching the whole release — and says it in our words.
mkdir -p "$DIR" || die "cannot create $DIR — install yad somewhere you own, such as ~/.local/bin"
[ -w "$DIR" ] ||
  die "cannot write to $DIR — install yad somewhere you own, such as ~/.local/bin, or re-run with YAD_INSTALL_DIR set"
# `mv file dir/` moves the file *into* a directory and succeeds, so a $DIR/yad
# that is one would leave the staged file inside it and nothing at the path
# this script then says it installed to.
[ -d "$DIR/yad" ] &&
  die "$DIR/yad is a directory — move it aside, or set YAD_INSTALL_DIR to install somewhere else"

tag="${YAD_VERSION:-}"
if [ -z "$tag" ]; then
  # gh fails the same way for a repository with no releases as for a login
  # problem, and only one of those is fixed by logging in again.
  tag=$(gh release view --repo "$REPO" --json tagName --jq .tagName 2>"$tmp/gh.err") || {
    grep -q "release not found" "$tmp/gh.err" &&
      die "$REPO has published no release yet — build yad from source with \`make install\`"
    die "could not ask $REPO for its newest release — it is private, so \`gh auth status\` is the first thing to check"
  }
fi
[ -n "$tag" ] || die "$REPO has published no release yet — build yad from source with \`make install\`"

gh release download "$tag" --repo "$REPO" --pattern "$asset" --pattern checksums.txt --dir "$tmp" 2>"$tmp/gh.err" >/dev/null || {
  grep -q "release not found" "$tmp/gh.err" &&
    die "$REPO has no release $tag — leave YAD_VERSION unset to take the newest"
  die "could not download $asset from $tag — $REPO is private, so \`gh auth status\` is the first thing to check"
}

# gh takes the two --pattern flags as alternatives: it fails only when *all* of
# them match nothing, so a release carrying the binary and no checksums.txt
# exits 0 here. Reading a file that is not there dies in awk's words rather
# than ours, and a missing binary hashes to nothing and reads as a checksum
# mismatch — sending the operator after tampering that did not happen.
[ -f "$tmp/$asset" ] ||
  die "release $tag has no $asset — nothing was installed"
[ -f "$tmp/checksums.txt" ] ||
  die "release $tag has no checksums.txt to check $asset against — nothing was installed"

# The checksum is checked before anything is written to $DIR: a bad download
# must leave whatever yad is already installed working. One line is pulled out
# of checksums.txt rather than running a checker over the whole file, which
# lists all four targets and would fail on the three not downloaded.
want=$(awk -v a="$asset" '{ n = $2; sub(/^\*/, "", n); if (n == a) print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "checksums.txt in $tag does not list $asset — nothing was installed"
got=$(sha256 "$tmp/$asset")
[ -n "$got" ] || die "could not compute a SHA-256 for $asset — nothing was installed"
[ "$want" = "$got" ] ||
  die "$asset from $tag does not match its published checksum (got $got, the release says $want) — nothing was installed"

had_one=no
if [ -e "$DIR/yad" ]; then had_one=yes; fi
# Staged inside $DIR and renamed, so the last step is one atomic mv within one
# filesystem: a yad that is running is never a half-written file. Each step
# carries its own refusal — an install that dies in cp's words gives the
# operator a temporary path they never chose and no next action.
cp "$tmp/$asset" "$staged" || die "could not write $staged — nothing was installed"
chmod 0755 "$staged" || die "could not make $staged executable — nothing was installed"
mv -f "$staged" "$DIR/yad" || die "could not put $asset in place at $DIR/yad — nothing was installed"

echo "installed $tag as $DIR/yad"
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) echo "note: $DIR is not on PATH — add it with: export PATH=\"$DIR:\$PATH\"" ;;
esac
if [ "$had_one" = yes ]; then
  # Under a service manager a clean stop is meant to stay stopped, so
  # `yad daemon restart` would replace the unit's supervision with a loose
  # process. Decision 0028 makes re-running install the upgrade path.
  # This script has no profile concept, so it names the flag rather than
  # filling it in: bare, both commands act on the default profile, and
  # `service install` would bootstrap a unit for one nobody meant to supervise.
  echo "note: a runner already running keeps the old binary until it is restarted — \`yad service install\` if it runs as a service, otherwise \`yad daemon restart\` (each takes --profile if this runner is not the default: after \`service install\`, but before \`daemon\`)"
fi
echo "next: yad doctor"
