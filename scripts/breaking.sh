#!/usr/bin/env bash
# Both OpenAPI documents in this repository are generated, committed, and read
# by code nobody here controls: a TypeScript hub generates its client from
# protocol/v1/openapi.yaml (0017), a service from protocol/hubapi/openapi.yaml
# (0022), and each carries its own major version that moves on its own. A field
# renamed or removed in either one breaks software already running on other
# people's machines — the one class of mistake in this repo whose cost lands
# somewhere the author cannot see, and the one least likely to be caught by eye
# in a review of a 4000-line generated file.
#
#   scripts/breaking.sh        make check-breaking
#
# Each document is compared against itself at the release before this commit
# (below, for what "before" means). Exit 1 with oasdiff's report on a breaking change; exit 0
# otherwise, and see below for what "otherwise" is allowed to mean.
#
# When oasdiff cannot do the comparison at all it exits neither 0 nor 1 — 102
# for a document it could not load, 100 for a bad flag — and that status is
# carried out of here unchanged rather than folded into 1. A spec that will not
# load is the input a check most wants to be loud about, and calling it a
# breaking change would send the reader hunting a rename that does not exist.
#
# A GROWN ENUM, before you switch this off. An enum the hub sends to a runner,
# such as a new control kind, may grow within v1 because a hub sends a new value
# only to a runner that advertised it (ARCHITECTURE.md §2 Versioning). oasdiff
# cannot see feature advertisement, so the generator writes those enums as
# x-extensible-enum, and they pass here by construction
# (docs/decisions/0058). Any other enum that fails here has a real victim. The
# usual one is protocol/hubapi's, where no feature exists to protect a service.
# An ignore rule for the oasdiff check would hide that failure too.
set -euo pipefail

# The documents people generate clients from. Both, deliberately: hubapi is
# outside the runner protocol but not outside the promise — 0022 gives it its
# own path major precisely because breaking it costs a caller a rewrite.
docs=(protocol/v1/openapi.yaml protocol/hubapi/openapi.yaml)

# The same glob release.yml publishes on, so the baseline is a spec someone
# could have taken rather than merely a tag someone wrote. Version sort,
# because refname sort puts v0.9.0 above v0.10.0.
#
# Which release is "the last one" depends on whether this commit is one.
#
# A tagged HEAD is a release being cut. release.yml runs `make check` on a tag
# push, detached at the tag. The baseline is the release just below it in
# version order, on another commit. The release job's run of this gate is the
# one that must not be vacuous: a tag can sit on a commit CI never saw. If the
# commit's own tag were the baseline, oasdiff would compare a file with itself
# and every release would pass while printing a reassuring "against v0.2.0".
#
# Version order, not ancestry, because the two disagree exactly where a
# release is unusual:
#   - Tag v0.3.0 on a strict ancestor of v0.2.0, and every tag contains HEAD.
#     An ancestry rule then finds nothing, and the gate goes inert on the one
#     run that must not be vacuous (DEV-90).
#   - "Newest tag not on HEAD" would hold a v0.1.1 patch on a side branch
#     against v0.2.0, and fail it on everything 0.2.0 added.
#   - The same rule would hold a bisect that lands on the v0.1.0 commit against
#     v0.2.0.
# By version order, v0.1.1 is compared with v0.1.0, v0.1.0 with whatever came
# before it (as its own release run did), and v0.3.0 with v0.2.0: the specs
# the people downstream actually hold. scripts/release-guard.sh refuses to
# publish that last one at all.
#
# An untagged HEAD, a pull request or an old commit, is compared with the
# newest release that does not contain it: the newest one it builds on. For a
# commit older than every release that leaves nothing, which is right,
# because there is no earlier spec. scripts/breaking_test.go runs every one of
# these topologies.
own=$(git tag --list 'v[0-9]*' --points-at HEAD --sort=-v:refname | head -n1)
tag=
if [ -n "$own" ]; then
	head=$(git rev-parse HEAD)
	below=
	while IFS= read -r t; do
		if [ -z "$below" ]; then
			[ "$t" = "$own" ] && below=1
			continue
		fi
		# A second name for this same commit is not an earlier release.
		[ "$(git rev-parse "$t^{commit}")" = "$head" ] && continue
		tag=$t
		break
	done < <(git tag --list 'v[0-9]*' --sort=-v:refname)
else
	tag=$(git tag --list 'v[0-9]*' --sort=-v:refname --no-contains HEAD | head -n1)
fi

if [ -z "$tag" ]; then
	# Absence is data, and this is the absence that matters most: a check with
	# no baseline passes every time, and a silent pass is indistinguishable
	# from a real one. So it says what it did not do.
	#
	# Three absences reach this branch, one per way of choosing, and each gets
	# its own sentence. A sentence written for one topology and printed in
	# another is how earlier defects in this file got here. With nothing
	# tagged, "nothing has been released" is true. The other two have tags
	# sitting right there, and that sentence would be flatly untrue for them.
	newest=$(git tag --list 'v[0-9]*' --sort=-v:refname | head -n1)
	if [ -z "$newest" ]; then
		echo "check-breaking: INERT — no v[0-9]* tag in this repository."
		echo "check-breaking: nothing has been released, so there is no baseline to compare against and this check proves nothing about ${docs[*]}."
		echo "check-breaking: it starts guarding the moment the owner pushes the first release tag; nothing else needs to change."
	elif [ -n "$own" ]; then
		echo "check-breaking: INERT — HEAD is release $own, and there is no earlier release on another commit to compare it against."
		echo "check-breaking: nothing was compared, and this is not evidence that ${docs[*]} are compatible with anything."
	else
		echo "check-breaking: INERT — every v[0-9]* tag contains HEAD: this commit is older than every release (newest is $newest), so it has no earlier spec."
		echo "check-breaking: nothing was compared, and this is not evidence that ${docs[*]} are compatible with anything."
	fi
	exit 0
fi

base=$(mktemp -d)
trap 'rm -rf "$base"' EXIT

failed=0
toolstatus=0
for doc in "${docs[@]}"; do
	if ! git cat-file -e "$tag:$doc" 2>/dev/null; then
		# A document that did not exist at the last release cannot have broken
		# anyone's client: there was no client. Said out loud rather than
		# skipped, because "no baseline for this file" and "this file is
		# compatible" are the same exit code and must not read the same.
		echo "check-breaking: $doc — INERT, the document does not exist at $tag; it is new since that release and has no baseline."
		continue
	fi

	# The committed document rather than a freshly generated one, on purpose:
	# this is the artefact a hub author actually downloads. `make
	# check-openapi`, which runs before this in `make check` and in CI, is
	# what proves it still matches the Go types.
	was=$base/$(echo "$doc" | tr / _)
	git show "$tag:$doc" >"$was"

	echo "check-breaking: $doc against $tag"
	# --fail-on is required at all: without it oasdiff prints the breaking
	# changes and still exits 0, and a report nobody is stopped by is not a
	# gate.
	#
	# WARN rather than ERR, which is not the obvious choice and was measured
	# rather than reasoned. oasdiff rates a removed *request* property a
	# warning, because its viewpoint is the ordinary one — a provider dropping
	# a field it accepts does not hurt the callers. This protocol inverts that
	# viewpoint: the hub is the server and generates its types from this
	# document, while the runner is the caller, so a field the runner stops
	# sending is a field a generated hub still requires.
	#
	# Concretely: delete SyncRequest's required `fingerprint`, which lives in
	# this document and in no other, and ERR exits 0 while WARN exits 1. ERR
	# catches a rename, because the added half is an error — so the acceptance
	# criterion would have passed while a plain deletion walked through.
	#
	# Adding a property stays clean at either level, so the stricter one is
	# free: a new optional field was measured on both documents and reported
	# no breaking change. Not every additive change is clean, though. A grown
	# response enum is additive and fails at *both* levels, because it is an
	# ERR-level rule. So WARN costs nothing there either, and relaxing this to
	# ERR would not let it through. 0058 handles it in the document instead.
	set +e
	go tool oasdiff breaking "$was" "$doc" --fail-on WARN
	status=$?
	set -e
	case $status in
	0) ;;
	# 1 is the only status that means "compared, and found something
	# breaking". Everything else is oasdiff failing to do the comparison at
	# all — 102 for a document it cannot load, 100 for a bad flag — and
	# reporting that as a breaking change would send the reader to invent a
	# rename that is not there. Kept apart so the message can carry the right
	# next action.
	1) failed=1 ;;
	*)
		echo "check-breaking: $doc — oasdiff exited $status without comparing anything. This is oasdiff failing, not a breaking change: 102 is a document it could not load, 100 a bad flag. Fix the document or the invocation and run it again."
		toolstatus=$status
		;;
	esac
done

# A document that could not be compared outranks one that compared badly: the
# run has not answered the question, so saying "nothing broke" would be a
# silent pass and saying "something broke" would be a fabricated one. oasdiff's
# own status is carried out rather than flattened, which is what makes the
# contract at the top of this file true.
if [ "$toolstatus" -ne 0 ]; then
	echo
	echo "check-breaking: oasdiff could not compare every document, so this run proves nothing about the ones it did not reach."
	exit "$toolstatus"
fi

if [ "$failed" -ne 0 ]; then
	echo
	echo "check-breaking: the changes above break a client generated from $tag."
	echo "check-breaking: a rename or a removal in a released document is a new major version, not an edit — see AGENTS.md and ARCHITECTURE.md §2 Versioning. If it is genuinely additive and oasdiff is wrong, say so in the pull request rather than here."
	exit 1
fi
