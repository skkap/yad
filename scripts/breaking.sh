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
# Each document is compared against itself at the last release tag that is not
# this commit's own. Exit 1 with oasdiff's report on a breaking change; exit 0
# otherwise, and see below for what "otherwise" is allowed to mean.
#
# When oasdiff cannot do the comparison at all it exits neither 0 nor 1 — 102
# for a document it could not load, 100 for a bad flag — and that status is
# carried out of here unchanged rather than folded into 1. A spec that will not
# load is the input a check most wants to be loud about, and calling it a
# breaking change would send the reader hunting a rename that does not exist.
#
# ONE KNOWN FALSE ALARM, before you switch this off: adding a value to an enum
# the hub *returns* — a new control kind is the case that will come up — fails
# here, and ARCHITECTURE.md §2 Versioning says growing that one is safe,
# because a hub sends a control only to a runner that advertised it. oasdiff
# cannot see feature advertisement. That collision is DEV-87, with the
# measurement and three options in it — read it before reaching for an ignore
# rule, because the obvious rule also hides the enum growth that really would
# break somebody.
set -euo pipefail

# The documents people generate clients from. Both, deliberately: hubapi is
# outside the runner protocol but not outside the promise — 0022 gives it its
# own path major precisely because breaking it costs a caller a rewrite.
docs=(protocol/v1/openapi.yaml protocol/hubapi/openapi.yaml)

# The same glob release.yml publishes on, so the baseline is a spec someone
# could have taken rather than merely a tag someone wrote. Version sort,
# because refname sort puts v0.9.0 above v0.10.0.
#
# --no-contains HEAD is what keeps that true during a release. release.yml runs
# `make check` on a tag push, with HEAD detached at the tag being released, so
# the newest tag is this commit's own: without this the baseline would be the
# working tree, oasdiff would compare a file to itself, and the release job's
# copy of the gate would pass on every release while printing a reassuring
# "against v0.2.0". That is the run release.yml's own comment exists for — a
# tag can sit on a commit CI never saw — and it is the one run where this check
# must not be vacuous. Excluding tags that contain HEAD leaves the previous
# release, which is the spec the people downstream actually hold.
tag=$(git tag --list 'v[0-9]*' --sort=-v:refname --no-contains HEAD | head -n1)

if [ -z "$tag" ]; then
	# Absence is data, and this is the absence that matters most: a check with
	# no baseline passes every time, and a silent pass is indistinguishable
	# from a real one. So it says what it did not do.
	#
	# Two different absences reach this branch and they must not print the same
	# sentence. Nothing tagged at all is the ordinary one. The other is every
	# tag being excluded by --no-contains, where "nothing has been released"
	# would be flatly untrue with the tags sitting right there.
	#
	# That second one has more than one cause, which is the trap: HEAD older
	# than every release (a bisect, an old commit checked out) has genuinely no
	# earlier spec, but a release published from a strict ancestor of an
	# earlier release has one and is not comparing against it — DEV-90, left
	# open because that topology is already shipping older code under a higher
	# version and wants guarding above this script. So this message says what
	# it did not do and stops, rather than explaining why: a sentence written
	# for one topology is how the last two defects in this file got here, and
	# it must be true in every topology that reaches it.
	if [ -z "$(git tag --list 'v[0-9]*' | head -n1)" ]; then
		echo "check-breaking: INERT — no v[0-9]* tag in this repository."
		echo "check-breaking: nothing has been released, so there is no baseline to compare against and this check proves nothing about ${docs[*]}."
		echo "check-breaking: it starts guarding the moment the owner pushes the first release tag; nothing else needs to change."
	else
		echo "check-breaking: INERT — every v[0-9]* tag contains HEAD, so none was usable as a baseline (newest is $(git tag --list 'v[0-9]*' --sort=-v:refname | head -n1))."
		echo "check-breaking: nothing was compared, and this is not evidence that ${docs[*]} are compatible with anything. If this ran while publishing a release, see DEV-90."
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
	# no breaking change. Not every additive change is clean, though — the
	# grown enum at the top of this file is additive and fails at *both*
	# levels, being an ERR-level rule. So WARN costs nothing there either, and
	# relaxing this to ERR would not buy DEV-87 back.
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
