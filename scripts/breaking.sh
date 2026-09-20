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
# Each document is compared against itself at the last release tag. Exit 1 with
# oasdiff's report on a breaking change; exit 0 otherwise, and see below for
# what "otherwise" is allowed to mean. An unparseable document exits 102, which
# is a failure here and deliberately not a silence: a spec that will not load
# is the one input a check most wants to be loud about.
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

# The same glob release.yml publishes on, so the baseline is by construction a
# spec someone could have taken, not merely a tag someone wrote. Version sort,
# because refname sort puts v0.9.0 above v0.10.0.
tag=$(git tag --list 'v[0-9]*' --sort=-v:refname | head -n1)

if [ -z "$tag" ]; then
	# Absence is data, and this is the absence that matters most: a check with
	# no baseline passes every time, and a silent pass is indistinguishable
	# from a real one. So it says what it did not do.
	echo "check-breaking: INERT — no v[0-9]* tag in this repository."
	echo "check-breaking: nothing has been released, so there is no baseline to compare against and this check proves nothing about ${docs[*]}."
	echo "check-breaking: it starts guarding the moment the owner pushes the first release tag; nothing else needs to change."
	exit 0
fi

base=$(mktemp -d)
trap 'rm -rf "$base"' EXIT

failed=0
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
	# this is the artefact a hub author actually downloads. `make check`'s
	# check-generated is what proves it still matches the Go types.
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
	# An additive change stays clean at either level, so the stricter one is
	# free: a new optional field was measured on both documents and reported
	# no breaking change.
	if ! go tool oasdiff breaking "$was" "$doc" --fail-on WARN; then
		failed=1
	fi
done

if [ "$failed" -ne 0 ]; then
	echo
	echo "check-breaking: the changes above break a client generated from $tag."
	echo "check-breaking: a rename or a removal in a released document is a new major version, not an edit — see CLAUDE.md and ARCHITECTURE.md §2 Versioning. If it is genuinely additive and oasdiff is wrong, say so in the pull request rather than here."
	exit 1
fi
