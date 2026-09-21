---
date: 2026-09-21
---

# A grant may not move a run off its account

Amends [0038](0038-the-owner-trusts-the-hubs-it-connects.md), whose "what
stays" list reserves accounts to the owner. It turned out one grant name could
reach an account anyway.

0038 accepts any valid variable name as a grant, `ANTHROPIC_API_KEY` among
them. The runner scrubs the owner's own `ANTHROPIC_API_KEY` from every child
(`supervise.Scrub`), because it moves billing from the account's subscription to
the API, and then appends the run's grants after the scrub. A hub's grant by
that name therefore lands exactly where the owner's was removed. A run the
runner put on account `work` would spend the hub's key while its events, health
and `yad account list` all said `work`.

The harm is not access. 0038 already accepts that a hub can ask the harness for
anything. The harm is that **the account layer starts lying**:

- **Limits never fire.** A turn on an API key reports none of the account's
  usage windows. Claude reached with an API key emits no `rate_limit_event` at
  all (`internal/adapter/claude/testdata/README.md`).
- **Failover has nothing to fail over.** No limit arrives, so the run never
  moves, and the accounts the owner configured for exactly that sit unused.
- **Health reports the account free for ever**, to every hub, whatever the
  account itself has left.
- **Events name an account the tokens did not come from.** An owner reading
  which subscription did which work is reading fiction.

## Decision

A grant may not name a variable that selects whose credential a harness uses or
which home it reads its login from. The runner refuses a run carrying one,
whole, and never runs it with the grant stripped (the rule 0024 and 0038 keep
for every grant).

- **One list, in `protocol/v1/grant.go`** (`accountGrantNames`), with the reason
  beside each name. `Grant.Validate` checks it, so the hub refuses such a run
  before queueing it and the runner refuses it at the claim, with the `refused`
  result of [0019](0019-a-run-starts-once-its-claim-is-acknowledged.md), before
  any workdir is built or harness started. The executor checks again where the
  grant becomes a variable. Nothing about a hub is trusted for this: a hub on an
  older build, or any hub that did not check, is refused by the runner all the
  same.
- **The names.** For Claude, taken from its authentication precedence:
  `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `ANTHROPIC_PROFILE`, `ANTHROPIC_FEDERATION_RULE_ID`,
  `ANTHROPIC_ORGANIZATION_ID`, `ANTHROPIC_CONFIG_DIR`, `CLAUDE_CONFIG_DIR`,
  `ANTHROPIC_BASE_URL`, `ANTHROPIC_CUSTOM_HEADERS`, and every provider switch
  by its prefix, `CLAUDE_CODE_USE_*`. For Codex, taken from its login code:
  `CODEX_HOME`, `OPENAI_API_KEY`, `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN`,
  `OPENAI_BASE_URL`, `CODEX_REFRESH_TOKEN_URL_OVERRIDE` and
  `AWS_BEARER_TOKEN_BEDROCK`, which Codex on its Bedrock provider (chosen in
  the account home's own config, with no switch to refuse) reads ahead of the
  AWS credential chain. Matched whole (the
  prefix as a prefix) and in any case, as the 0038 deny list is. `HOME`, which
  also moves both harnesses' default homes, was already refused.
- **The switches go by prefix.** Claude names every cloud provider switch
  `CLAUDE_CODE_USE_<provider>`: Bedrock, Vertex, Foundry and Claude Platform
  on AWS so far. The first draft of this list named three of them and missed
  the fourth, so the family is refused whole. That way a provider Claude has
  not shipped yet is refused before anyone thinks to add it.
- **Endpoints and headers count.** The server that answers a turn decides whose
  account it runs on, and a turn answered through someone else's endpoint
  reports none of the account's windows. A header variable can carry an
  `x-api-key` or `Authorization` header, which is a credential by another name.
- **What is left off, and why.** A provider's own keys, endpoints and
  workspace ids (`ANTHROPIC_BEDROCK_BASE_URL`, `ANTHROPIC_FOUNDRY_API_KEY`,
  `ANTHROPIC_AWS_API_KEY`, `ANTHROPIC_AWS_WORKSPACE_ID`,
  `ANTHROPIC_VERTEX_BASE_URL` and the like) take effect only once a
  `CLAUDE_CODE_USE_*` switch is on. The switches are refused, and Scrub
  removes every `CLAUDE_CODE_*` from the owner's environment, so the keys alone
  move nothing and a project that deploys to AWS keeps its credentials. The
  Workload Identity Federation inputs (`ANTHROPIC_IDENTITY_TOKEN[_FILE]`,
  `ANTHROPIC_SERVICE_ACCOUNT_ID`, `ANTHROPIC_WORKSPACE_ID`) are inert the same
  way. Claude federates only when `ANTHROPIC_FEDERATION_RULE_ID` and
  `ANTHROPIC_ORGANIZATION_ID` are both set, and each of those is refused on its
  own, because the owner's environment may already hold the other.
  `CODEX_REVOKE_TOKEN_URL_OVERRIDE` and `CODEX_APP_SERVER_LOGIN_CLIENT_ID` are
  read only when someone logs in or out, and a run does neither.
- **The same list for every harness.** A Codex run is refused
  `ANTHROPIC_API_KEY` too. A hub checks the list before queueing without knowing
  which adapter reads what, one harness can start the other as a tool, and the
  cost of refusing is a rename.
- **The refusal carries the way round it.** A project that needs its own
  Anthropic or OpenAI key, for its tests say, gets it under another name that
  the brief tells the harness about. A run that should use another login needs
  the owner to add that login as an account with `yad account add`.
- **The owner is not bound by this.** It limits what a hub can do. Whether an
  owner may configure API-key billing on the machine is DEV-62's question.

`internal/account`'s tests hold every harness home variable to the list, so a
harness that gains account homes cannot be missed.

0038's "what stays" list now reads: owner configuration is never a protocol
field, **and a grant may not move a run off its account**.

## What was not measured, and why it does not matter

How the installed Claude CLI ranks an `ANTHROPIC_API_KEY` against the OAuth
login in `CLAUDE_CONFIG_DIR` was not measured. DEV-73 asked for it, since it
decides whether the conflict is real in practice. The refusal is correct either
way. If the key wins, the run leaves its account. If the login wins, the grant
does nothing a run needs, and refusing it costs a rename. The same holds for
every other name on the list whose precedence has not been measured.

## Considered options

**Strip the account's variables before grants are delivered** — the ticket's
first alternative. Silent. The hub believes it delivered a grant that never
arrived, and the run fails for a reason nobody sees, which is why 0024 and 0038
refuse rather than strip. **Allow it, and write a sentence saying so** — the
ticket's second. The account label, limits, failover and health would then be
true only when no hub had sent one of these names, which no one reading them
could check. **Reserve whole namespaces** (`ANTHROPIC_*`, `OPENAI_*`,
`CODEX_*`), as 0024 did. More future-proof, but it refuses `ANTHROPIC_MODEL`
and every other variable in those namespaces that moves no account, and 0038
dropped the namespaces for exactly that reason. A name missing from this list
is a bug to add a line for, not a reason to go back to them.
