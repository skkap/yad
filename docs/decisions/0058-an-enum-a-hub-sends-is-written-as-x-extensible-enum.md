---
date: 2026-09-29
---

# An enum a hub sends is written as x-extensible-enum, so the breaking check lets it grow

`make check-breaking` (DEV-34) compares both OpenAPI documents with the last
release, and since v0.1.0 (2026-09-28) it has something to compare with. It
would fail on the way this protocol is meant to grow. A new control kind is a
value added to an enum in the sync *response*. oasdiff rates that
`response-property-enum-value-added`, an error at any `--fail-on` level,
because it cannot see that a hub sends the value only to a runner that
advertised it in `protocol_features`
([0047](0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md),
ARCHITECTURE.md §2 Versioning). Four login kinds and `remove_account` have
come in that way since v1 shipped. Each one would have failed the gate
(DEV-87).

## Decision

**In `protocol/v1/openapi.yaml`, an enum a hub sends to a runner is written as
`x-extensible-enum:` in place of `enum:`.** It lists the same values. Today
that is three fields: `Control.kind`, `Grant.as` and `SessionRef.mode`. These
are the three `TestTheV1EnumsAreClosed` marks as sent towards the runner. The
generator decides this (`internal/hub/extensibleenum.go`, called from
`hub.OpenAPIYAML`) by direction, not from a list of names. It marks a schema
reachable from a response body and from no request body, and only in the
protocol's document. `TestOnlyWhatAHubSendsIsExtensible` pins the result, so a
fourth one shows up in review.

**`protocol/hubapi/openapi.yaml` keeps every `enum`.** The service API has no
feature advertisement. A state or login phase it gains reaches a service that
never asked for it, and that is the enum growth the check must keep catching.
`TestTheServiceAPIHasNoExtensibleEnum` holds it.

**Enums a runner sends keep `enum`.** oasdiff rates a request enum that gained
a value as information only, never breaking, so there is nothing to fix there. A hub generated
from the document validates those fields strictly, and 0047 relies on that.
`TestExtendingEnumsLeavesRequestsAlone` holds it.

## Measured, with oasdiff v1.32.1 as pinned in go.mod

Every row below compares a document with the real `v0.1.0` tag at `--fail-on WARN`.

| Change | Result |
|---|---|
| `proof_new_kind` added to `Control.kind` as `enum` (before this) | exit 1, `response-property-enum-value-added` |
| the same, with `enum` kept and `x-extensible-enum` added beside it | exit 1: oasdiff still checks the `enum` |
| this change: the three rewritten, nothing added | exit 0; 16 info-level `response-property-enum-value-removed`, one per value moved, nothing renamed or removed |
| this change, plus `proof_new_kind` in the Go tag, regenerated | exit 0 |
| `proof_new_state` added to hubapi's run `state` | exit 1, six `response-property-enum-value-added`, one per operation returning a run |
| `proof_new_state` added to v1's `HeldRun.state` (a runner sends it) | exit 0 from oasdiff; `TestTheV1EnumsAreClosed` fails |

What lets these fields pass is that `enum` is gone. oasdiff's response rules
read only that keyword, and no response rule reads `x-extensible-enum`
(v1.32.1 reads the extension only in its two request-side removal checks). The
values still go under the extension, for three reasons:

- oasdiff's own message recommends it for a set meant to grow;
- it is Zalando's convention, and it keeps the list machine-readable;
- it would catch a removed value if the field ever moved into a request.

In a response, then, oasdiff guards neither growth nor removal of these
values. Removal is caught by `TestTheV1EnumsAreClosed`, which pins each set
exactly.

The last row
shows that the request side was never oasdiff's to guard. Growth there is
caught by the Go pin test, which names 0047's rule. A feature-gated control
kind still fails that pin test too. That is intended: the failure is where the
author reads that the kind must be gated before it is added.

The ticket feared a dangerous `RunState` growth that "an old runner cannot
read". In v1 a runner sends run states and never reads them, so that half is
the pin test's. The victim oasdiff protects is a service reading hubapi's run
`state`, and that still fails the gate.

## What it costs

A client generator that does not read the extension types the three fields as
plain strings. It no longer produces a union of their values. For a hub, that
means the controls, grant deliveries and session modes it *writes* lose a
compile-time check on the spelling. The values stay listed in the document
under the extension, and each field's description still names them and the
features that gate them. Hubs generated from v0.1.0 are unaffected until they
regenerate, and after that they are looser, never stricter.

## Considered options

**An oasdiff ignore file** (`--err-ignore`) downgrading
`response-property-enum-value-added`. It works per rule, not per field, so it
would also let hubapi's run state grow unchecked, and nobody revisits an ignore
file. DEV-34 rejected it and DEV-87's acceptance rules it out.

**Arguing it in each pull request.** This is free today, but it depends on
every future author reaching the right conclusion while a check is red and
they are in a hurry. The likely outcome is the check switched off.

**Relaxing `--fail-on` to ERR.** The rule is error-level, so this would not
help. It would also stop the check catching a deleted request field, which is
why `scripts/breaking.sh` uses WARN.

**Keeping `enum` and adding `x-extensible-enum` beside it**, so generated hubs
keep their unions. Measured above: oasdiff still checks the enum.

**Marking every v1 enum**, since 0047 feature-gates them all. That would buy
nothing on the request side, where oasdiff flags no growth. It would also
loosen what a hub generated from the document validates, where 0047 relies on
strictness.

**Only `Control.kind`**, the one that actually grows. `Grant.as` and
`SessionRef.mode` are gated on the same terms. A rule by name would fail the
first gated grant delivery, the same way DEV-87 found the control kind.
