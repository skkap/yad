#!/bin/sh
# Installs yad into ~/.local/bin from the newest tagged release.
#
# The repository is private (decided 2026-09-19), so there is no URL to curl
# without a token — for the binaries or for this script. `gh` does the
# fetching, and the login you already have is what grants access:
#
#   gh api -H "Accept: application/vnd.github.raw" \
#     repos/skkap/yad/contents/scripts/install.sh | sh
#
# Fetch it with gh rather than curl: a curl carrying `Authorization: Bearer
# $(gh auth token)` puts the live token in curl's argv, where /proc and ps
# hand it to every local account for the length of the request. gh reads the
# same token from its own keyring and never passes it as an argument.
#
# YAD_VERSION pins a release, YAD_INSTALL_DIR moves where it lands, and
# YAD_REPO points at a fork. The assignment belongs on `sh`, not on `gh` — a
# prefix applies to the one command it prefixes and does not cross the pipe:
#
#   gh api -H "Accept: application/vnd.github.raw" \
#     repos/skkap/yad/contents/scripts/install.sh | YAD_VERSION=v0.3.1 sh
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

tag="${YAD_VERSION:-}"
if [ -z "$tag" ]; then
  tag=$(gh release view --repo "$REPO" --json tagName --jq .tagName) ||
    die "could not ask $REPO for its newest release — it is private, so \`gh auth status\` is the first thing to check"
fi
[ -n "$tag" ] || die "$REPO has published no release yet — build yad from source with \`make install\`"

tmp=$(mktemp -d)
staged="$DIR/.yad.install.$$"
trap 'rm -rf "$tmp" "$staged"' EXIT INT TERM

gh release download "$tag" --repo "$REPO" --pattern "$asset" --pattern checksums.txt --dir "$tmp" >/dev/null ||
  die "could not download $asset from $tag — $REPO is private, so \`gh auth status\` is the first thing to check"

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
# must leave whatever yad is already installed working.
want=$(awk -v a="$asset" '{ n = $2; sub(/^\*/, "", n); if (n == a) print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "checksums.txt in $tag does not list $asset — nothing was installed"
got=$(sha256 "$tmp/$asset")
[ -n "$got" ] || die "could not compute a SHA-256 for $asset — nothing was installed"
[ "$want" = "$got" ] ||
  die "$asset from $tag does not match its published checksum (got $got, the release says $want) — nothing was installed"

mkdir -p "$DIR"
had_one=no
# `[ -e ... ] && had_one=yes` would end the script under `set -e` when the
# test fails, which is the common case: a first install.
if [ -e "$DIR/yad" ]; then had_one=yes; fi
# Staged inside $DIR and renamed, so the last step is one atomic mv within one
# filesystem: a yad that is running is never a half-written file.
cp "$tmp/$asset" "$staged"
chmod 0755 "$staged"
mv -f "$staged" "$DIR/yad"

echo "installed $tag as $DIR/yad"
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) echo "note: $DIR is not on PATH — add it with: export PATH=\"$DIR:\$PATH\"" ;;
esac
if [ "$had_one" = yes ]; then
  echo "note: a runner already running keeps the old binary until it is restarted (\`yad daemon restart\`)"
fi
echo "next: yad doctor"
