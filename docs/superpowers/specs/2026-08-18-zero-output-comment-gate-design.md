# Zero-Output GitHub Comment Gate Design

## Problem

An agent command can fail before it emits review protocol output. The Codex
adapter currently returns a plain error when the process exits without valid
JSON events. The worker treats that error as a genuine review failure, retries
it as ordinary agent work, and synthesis can produce an all-failed review body.

The daemon-free `ci review --comment` path posts that body even though no agent
produced review output. The daemon panel path already distinguishes substantive
review output, but its eventual unavailable notes include an excerpt from the
stored job error. Codex job errors can include command stderr. Local
installation, authentication, and provider diagnostics therefore risk becoming
pull-request comments through those operational notes.

## Publication Policy

A GitHub review comment requires at least one substantive agent result. A result
is substantive when the review job completed with nonempty review output. The
daemon-free `ci review --comment` path must not call the forge comment API when
no agent produced output.

The daemon may still post its existing fixed operational notices without a
substantive result: the all-skipped summary, the bounded genuine-failure
give-up note, and the transient three-day give-up note. These notices explain
terminal CI state and are not review findings. They must contain only fixed,
category-level language and must never interpolate a stored job error or raw
stderr. Their existing commit statuses and terminal panel outcomes remain
unchanged.

The daemon-free gate applies regardless of whether the underlying failure is
local, provider-side, quota-related, timed out, or not yet classified.
Publication eligibility must not depend on matching mutable command-line error
strings.

Partial-success panels remain publishable. Their synthesized review may report
which reviewers failed, but it must not include raw command stderr. Fully
successful reviews are unchanged.

## Error Classification

Agent execution gains an unavailable category distinct from quota limits and
genuine review results. Codex attaches a typed pre-protocol wrapper to command
startup failures, propagated `exec --help` capability-probe errors, and a
nonzero process exit with no valid events. The intentional compatibility
fallback in `codexSupportsIgnoreUserConfig` remains unchanged: a probe command
that merely rejects the optional flag reports the capability as unsupported and
does not become an unavailable error.

The worker applies the existing quota, session, and transient classifiers to
the complete wrapped error before considering the unavailable category. A
pre-protocol error that still contains a recognized provider or session signal
keeps its existing cooldown, retry, prefix, and terminal behavior. Only a typed
pre-protocol error whose limit classification is `LimitKindNone` enters the
unavailable path.

The unavailable category uses its typed wrapper while the error remains in
memory. Storage keeps the existing string schema and persists a canonical
`unavailable:` category prefix followed by the diagnostic. Classification uses
the stable prefix, while local logs retain the complete wrapped cause.
Deterministic pre-protocol failures do not consume the ordinary in-job retry
loop. They use the existing non-retryable-agent path: attempt a configured,
distinct backup agent that is not cooling down, otherwise fail the job
immediately. Explicit stored backups retain their existing semantics and are
not preflighted against `PATH`; if that backup attempt also fails, its error is
classified independently. Workflow-derived backups retain their existing
configuration-aware availability check. After persistence, `unavailable:`
remains a genuine failure for panel classification, so the daemon's
higher-level CI scheduler continues to provide bounded recovery attempts after
the environment changes. Daemon-free CI returns nonzero to its caller.

This classification improves retry behavior and local status, but it is not the
security boundary. The publication policy remains the final gate even for
unknown errors.

## Posting Paths

The daemon-free CI flow checks the completed batch before `postForgeComment`.
An all-failed batch logs its diagnostic summary and returns its existing
nonzero result without making a forge request.

The daemon keeps its existing outcome classifier. A review post still requires
substantive output. All-skipped and bounded give-up outcomes continue through
their existing posting and finalization paths, including their current commit
status and `giveup_posted` or `no_review_posted` terminal outcome. The two
give-up formatters no longer accept or render a last-error excerpt. Complete
diagnostics remain available in local state and logs.

The daemon's existing `hasReviewOutput` check will move to shared review logic
and require a completed result whose trimmed review text is nonempty. The
daemon-free flow will reuse it immediately before its review forge request, and
the daemon outcome classifier will reuse it for review-post eligibility. The
fixed operational-note paths remain explicit exceptions rather than pretending
to contain substantive review output.

## Tests

Behavior tests will prove:

- `ci review --comment` makes zero forge calls and returns nonzero when every
  agent fails before producing output;
- quota, timeout, unavailable, and unknown all-failed batches make zero forge
  calls;
- one substantive result plus failed reviewers posts exactly one synthesized
  comment whose failure summaries contain no raw stderr;
- fully successful review posting is unchanged;
- daemon panels post no review comment while every member has zero output;
- all-skipped daemon panels retain their fixed `Review Skipped` notice and
  `no_review_posted` terminal outcome;
- bounded genuine and transient give-up paths retain their fixed unavailable
  notices, commit statuses, and `giveup_posted` terminal outcome without a last
  error excerpt;
- daemon panels with a substantive member remain publishable;
- the shared substantive-output helper accepts only completed, trimmed nonempty
  review output and is used by both posting flows;
- propagated Codex `exec --help` probe errors, command startup failure, and
  nonzero exit without valid protocol events are classified unavailable when
  no quota, session, or transient signal takes precedence;
- the `codexSupportsIgnoreUserConfig` compatibility rejection remains a
  successful unsupported-capability fallback rather than an unavailable error;
- no-JSON exits containing recognized quota, session, or transient signals keep
  their existing classifications and retry or cooldown behavior;
- unavailable worker failures attempt a configured, distinct, non-cooling
  backup agent and otherwise avoid the generic immediate retry loop while
  retaining their complete diagnostic locally; an explicit backup remains
  un-preflighted and any failure from its attempt is classified independently;
- a persisted `unavailable:` failure remains genuine for bounded CI scheduler
  recovery; and
- no fixed operational-note formatter receives or renders raw stderr.

Tests use synthetic repositories, identifiers, and provider messages. They do
not include incident logs, real infrastructure names, or external API calls.

## Compatibility And Non-Goals

No storage schema migration or new panel outcome is required. Existing stored
plain errors remain local, and existing terminal outcome backfill semantics are
unchanged.

This change does not hide agent-authored review findings, convert failed agent
execution into a successful verdict, or suppress local diagnostics. It does not
guarantee that an agent's substantive prose is free of sensitive content; that
is a separate review-output policy concern.
