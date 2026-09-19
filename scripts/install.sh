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
# or, if you would rather curl it:
#
#   curl -fsSL -H "Authorization: Bearer $(gh auth token)" \
#     https://raw.githubusercontent.com/skkap/yad/master/scripts/install.sh | sh
#
# YAD_VERSION pins a release, YAD_INSTALL_DIR moves where it lands, and
# YAD_REPO points at a fork. Nothing here restarts anything: a runner already
# running keeps the binary it started from until someone restarts it.
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
gh auth status >/dev/null 2>&1 ||
  die "\`gh\` is not logged in, and $REPO is private — run \`gh auth login\`"

# One of these two exists on every Linux distribution and every macOS; without
# one there is no way to check what was downloaded, and an unchecked binary is
# not worth installing.
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "neither sha256sum nor shasum is on this machine, so the download cannot be verified"
fi

tag="${YAD_VERSION:-}"
if [ -z "$tag" ]; then
  tag=$(gh release view --repo "$REPO" --json tagName --jq .tagName) ||
    die "could not ask $REPO for its newest release — check \`gh auth status\`"
fi
[ -n "$tag" ] || die "$REPO has published no release yet — build yad from source with \`make install\`"

tmp=$(mktemp -d)
staged="$DIR/.yad.install.$$"
trap 'rm -rf "$tmp" "$staged"' EXIT INT TERM

gh release download "$tag" --repo "$REPO" --pattern "$asset" --pattern checksums.txt --dir "$tmp" >/dev/null ||
  die "could not download $asset from $tag"

# The checksum is checked before anything is written to $DIR: a bad download
# must leave whatever yad is already installed working.
want=$(awk -v a="$asset" '{ n = $2; sub(/^\*/, "", n); if (n == a) print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "checksums.txt in $tag does not list $asset — nothing was installed"
got=$(sha256 "$tmp/$asset")
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
