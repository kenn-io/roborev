# Zero-Output GitHub Comment Gate Design

## Problem

An agent command can fail before it emits review protocol output. The Codex
adapter currently returns a plain error when the process exits without valid
JSON events. The worker treats that error as a genuine review failure, retries
it as ordinary agent work, and synthesis can produce an all-failed review body.

The daemon-free `ci review --comment` path posts that body even though no agent
produced review output. Other panel paths can eventually post an unavailable
note containing a raw error excerpt. Local installation, authentication, and
provider failures therefore risk becoming pull-request comments.

## Publication Invariant

A GitHub review comment requires at least one substantive agent result. A result
is substantive when the review job completed with nonempty review output. If no
agent produced output, roborev must not call the forge comment API.

The invariant applies regardless of whether the underlying failure is local,
provider-side, quota-related, timed out, or not yet classified. Publication
must not depend on matching mutable command-line error strings.

Partial-success panels remain publishable. Their synthesized review may report
which reviewers failed, but it must not include raw command stderr. Fully
successful reviews are unchanged.

## Error Classification

Agent execution gains an unavailable category distinct from quota limits and
genuine review results. Codex returns this category for failures before the JSON
event protocol starts, including command startup failure and a nonzero process
exit with no valid events.

The unavailable category uses a typed wrapper while the error remains in
memory. Storage keeps the existing string schema and persists a canonical
`unavailable:` category prefix followed by the diagnostic. Classification uses
the stable prefix, while local logs retain the complete wrapped cause.
Deterministic pre-protocol failures do not consume the ordinary in-job retry
loop. The daemon's higher-level CI scheduler remains responsible for bounded
recovery attempts after the environment changes; daemon-free CI returns
nonzero to its caller.

This classification improves retry behavior and local status, but it is not the
security boundary. The publication invariant remains the final gate even for
unknown errors.

## Posting Paths

The daemon-free CI flow checks the completed batch before `postForgeComment`.
An all-failed batch logs its diagnostic summary and returns its existing
nonzero result without making a forge request.

The daemon panel poster checks member results before every review, soft-note, or
give-up comment. A zero-output panel remains visible in local state and logs but
never posts a GitHub comment. This also prevents raw stderr from leaving through
the unavailable-note formatter.

The substantive-output check will live in shared review logic and will require
a completed result whose trimmed review text is nonempty. Both existing posting
entry points must call this helper immediately before their forge request, so
neither path reimplements error classification.

## Tests

Behavior tests will prove:

- `ci review --comment` makes zero forge calls and returns nonzero when every
  agent fails before producing output;
- quota, timeout, unavailable, and unknown all-failed batches make zero forge
  calls;
- one substantive result plus failed reviewers posts exactly one synthesized
  comment;
- fully successful review posting is unchanged;
- daemon panels make no comment before or after bounded give-up when every
  member has zero output;
- daemon panels with a substantive member remain publishable;
- a synthetic Codex pre-protocol launcher failure is classified unavailable;
- unavailable worker failures avoid the generic immediate retry loop while
  retaining their complete diagnostic locally; and
- no GitHub comment formatter receives raw stderr from a zero-output run.

Tests use synthetic repositories, identifiers, and provider messages. They do
not include incident logs, real infrastructure names, or external API calls.

## Compatibility And Non-Goals

No storage schema migration is required. Existing stored plain errors continue
to be protected by the classification-independent publication gate.

This change does not hide agent-authored review findings, convert failed agent
execution into a successful verdict, or suppress local diagnostics. It does not
guarantee that an agent's substantive prose is free of sensitive content; that
is a separate review-output policy concern.
