#!/usr/bin/env bash
# Refuses to publish a release whose commit an earlier release already
# contains.
#
#   scripts/release-guard.sh <tag>        release.yml, before anything is built
#
# A tag on a strict ancestor of a released commit ships older code under a new
# version. Everyone who takes it goes back in time, and `yad upgrade` offers it
# as an upgrade because it compares version numbers, not commits. Nothing
# downstream reliably catches it: `make check` passes on old code as readily as
# on new. check-breaking compares the new release with the one below it in
# version order, which here is the newer code (DEV-90). It fails only when that
# newer code changed a protocol document.
#
# A rollback is still possible. Revert on top of the newest release and tag
# that commit: its history then holds both the release and the revert.
#
# A second tag on an already-released commit is not refused. It ships the same
# code under another name, which misleads nobody about what is inside.
set -euo pipefail

if [ $# -ne 1 ] || ! git rev-parse -q --verify "refs/tags/$1" >/dev/null; then
	echo "release-guard: usage: scripts/release-guard.sh <tag>, naming a tag in this repository." >&2
	exit 2
fi
new=$1
at=$(git rev-parse "$new^{commit}")

# Quoted only when it has to be. A tag name that git accepts can still hold a
# quote or a dollar sign, and the printed command must run as printed.
sq() {
	case $1 in
	*[!A-Za-z0-9._/-]*) printf "'%s'" "${1//\'/\'\\\'\'}" ;;
	*) printf '%s' "$1" ;;
	esac
}

refused=
while IFS= read -r t; do
	c=$(git rev-parse "$t^{commit}")
	if [ "$c" != "$at" ] && git merge-base --is-ancestor "$at" "$c"; then
		echo "release-guard: $new is on ${at:0:12}, which $t (${c:0:12}) already contains. Publishing it would ship older code than $t under a new version."
		refused=$t
	fi
done < <(git tag --list 'v[0-9]*' --sort=-v:refname)

if [ -n "$refused" ]; then
	echo "release-guard: nothing was published. Delete the tag with \`git push origin --delete $(sq "$new")\` and \`git tag -d $(sq "$new")\`. Then tag a commit that contains $refused. To roll back, first revert on top of it."
	exit 1
fi
echo "release-guard: $new is not older than any release."
