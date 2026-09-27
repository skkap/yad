#!/bin/sh
# Prints the licence of everything compiled into the yad binary: the Go
# standard library and runtime, and every module cmd/yad links. MIT and BSD
# both require these notices to travel with a binary distribution, so a
# release carries this output as THIRD_PARTY_LICENSES.txt.
#
# A module with no licence file fails the script rather than being skipped:
# a release with a gap in its notices is the one thing this exists to prevent.
set -eu

notice() {
  printf '%s\n%s\n%s\n\n' "================================================================" "$1" "================================================================"
}

# Homebrew moves Go's LICENSE out of GOROOT to the directory above it.
goroot=$(go env GOROOT)
golicense=
for f in "$goroot/LICENSE" "$goroot/../LICENSE"; do
  if [ -f "$f" ]; then golicense=$f; break; fi
done
[ -n "$golicense" ] || { echo "notices: no LICENSE in or above $goroot" >&2; exit 1; }
notice "Go standard library and runtime ($(go env GOVERSION))"
cat "$golicense"
echo

go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/yad |
  sort -u |
  while read -r path version dir; do
    found=no
    for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE*; do
      [ -f "$f" ] || continue
      if [ "$found" = no ]; then notice "$path $version"; fi
      found=yes
      if [ "$(basename "$f")" != LICENSE ]; then echo "--- $(basename "$f")"; fi
      cat "$f"
      echo
    done
    [ "$found" = yes ] || { echo "notices: $path $version has no licence file in $dir" >&2; exit 1; }
  done
