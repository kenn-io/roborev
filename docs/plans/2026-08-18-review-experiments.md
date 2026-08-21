# Review configuration experiments

Status: Confirmed design

Date: 2026-08-18

## Summary

Add deterministic, branch-scoped A/B experiments for daemon-backed reviews. An
enabled experiment compares the normal review configuration with one
configuration overlay. Roborev assigns a branch to one arm, applies the overlay
before resolving the review or panel, and stores the assignment with the review
unit.

The first use is comparing normal reviews with reviews that enable
`reuse_review_session`. The mechanism is generic: agent, model, reasoning,
panel, and other review-time configuration may be varied without adding another
experiment-specific feature flag.

Experiments apply to ordinary daemon reviews and GitHub CI-poller reviews. The
standalone, daemon-free `roborev ci review` command is not part of this design.

## Goals

- Assign branches deterministically to a default or experimental arm.
- Keep the same branch in the same arm across commits and review types.
- Apply an experimental configuration as a layer above global and repository
    configuration and below explicit request or CLI values.
- Treat a panel as one experimental unit, even when its overlay changes the
    member or synthesis configuration.
- Store immutable experiment definitions, assignments, and session-resumption
    lineage in SQLite and PostgreSQL.
- Make assignment metadata available through structured projections and exports
    without revealing it to reviewing agents or PR readers.
- Extend review-session reuse to ordinary panels and CI-poller panels so the
    first experiment exercises real treatment behavior.
- Leave room for several assignments per review unit later while enforcing at
    most one applicable experiment now.

## Non-goals

- Calculating statistical significance or experiment success.
- Defining a quality metric, evaluator, or experiment dashboard.
- Multi-arm experiments, adaptive allocation, or bandits.
- Enrolling one review in several experiments in this version.
- Experimenting on fixes, refine, analyze, compact, synthesis-only tasks, or
    other non-review workflows.
- Enrolling the local auto-design classifier or its separate follow-up job. A
    CI auto-design member remains part of its assigned CI panel.
- Making daemon lifecycle, database, credentials, sync, or web-server settings
    vary per review.
- Adding experiments to the daemon-free `roborev ci review` command.

## Current behavior that shapes the design

The existing `reuse_review_session` implementation only runs from the
single-agent daemon enqueue path in `internal/daemon/server.go`. Its storage
query deliberately excludes panel members. CI jobs already have a source marker
that can keep them out of local fix and refine discovery when their head branch
is stored on the job.

The CI poller currently knows the PR head SHA and base branch but does not carry
the head branch name through `ghPR`. That field must be added before the poller
has the branch identity required for assignment and session reuse.

`review_jobs.session_id` does not say whether a job resumed. A fresh job stores
the session ID captured from the agent, while a resumed job begins with an
existing session ID. Both have the same stored shape after completion.

These are implementation constraints, not alternative experiment semantics.

## Terminology and invariants

**Experiment definition**
:   A versioned, immutable experiment ID, its allocation ratio, workflow scope,
    and experimental configuration overlay. `enabled` is enrollment state and is
    not part of the immutable definition.

**Review unit**
:   A single review job or one complete panel run. A panel has one assignment;
    member and synthesis jobs derive it through the panel-run UUID.

**Subject**
:   The namespaced repository and branch identity used for deterministic
    assignment. The database stores only its SHA-256 hash.

**Default arm**
:   The base configuration with no experiment overlay.

**Experimental arm**
:   The base configuration with the experiment's configuration recursively
    merged on top.

The following invariants are mandatory:

1. A review without a branch subject does not participate.
1. A review unit has at most one assignment in this version.
1. Every job in a panel derives the panel's assignment.
1. An experiment ID identifies exactly one definition for the lifetime of a
    database.
1. Explicit request and CLI values retain precedence over the experiment.
1. Experiment metadata never enters an agent prompt, agent environment, PR
    comment, or review prose.
1. Synthesis jobs carry panel attribution but never resume review sessions.

## Configuration contract

Experiments are named TOML tables in the global or repository config:

```toml
[experiments.review-session-resumption-v1]
enabled = true
ratio = 0.5
workflows = ["review", "ci"]

[experiments.review-session-resumption-v1.config]
reuse_review_session = true
```

`ratio` is the fraction assigned to the experimental arm. It must be between
`0.0` and `1.0`, inclusive. An enabled experiment with `ratio = 0.0` records an
all-default baseline. A disabled experiment records no assignments.

The allowed workflow names are:

- `review`: ordinary daemon review jobs, including post-commit reviews and
    ordinary panels.
- `ci`: GitHub CI-poller panel reviews.

Several definitions may be enabled when their workflow scopes do not overlap.
Configuration validation rejects two enabled definitions that can apply to the
same review.

### Overlay schema

`config` uses the normal repository review-configuration vocabulary. It is
decoded and validated as a partial `RepoConfig`, then applied as a synthetic
highest-precedence repository layer. This reuses existing config-taking
resolvers and avoids creating parallel agent, model, reasoning, and panel
semantics.

The initial allowlist includes configuration frozen while constructing a review
plan:

- review agents, models, backup agents, providers, reasoning, and severity;
- `reuse_review_session` and its lookback;
- `[review]` subagents and panels;
- `[ci]` review matrix, named panel, reasoning, and severity settings;
- named-panel synthesis settings under `[review.panels]`.

The overlay rejects `experiments` and settings whose effects occur outside a
review unit, including daemon listeners, worker-pool sizing, credentials,
database and sync configuration, hooks, and browser configuration.
It also rejects prompt-building and worker-runtime settings such as review
guidelines, context count, exclusion patterns, prompt limits, and job timeout;
those are resolved after enqueue today and cannot yet be frozen faithfully as
part of an experiment assignment.

Within the overlay, nested tables merge recursively. Scalars and arrays replace
base values. Global and repository configuration first compose with the normal
typed resolver boundaries: a same-named repository subagent or panel replaces
the complete global entry. The experiment overlay then recursively merges into
that resolved entry. TOML key order has no effect.

### Global and repository definitions

A repository may enable or disable a definition supplied by global config:

```toml
[experiments.review-session-resumption-v1]
enabled = true
```

When the same ID exists globally, the repository may override only `enabled`.
Changing `ratio`, `workflows`, or `config` requires a new experiment ID. A
repository may define a complete experiment under a new ID.

The effective precedence is:

```text
defaults -> global config -> repo config -> experiment overlay -> explicit request
```

## Experiment selection module

Experiment behavior belongs in one deep module under
`internal/config/experiments.go`. Callers should not implement hashing, ratio
thresholds, raw-map merging, definition validation, or fingerprints.

Its conceptual interface is:

```go
type ExperimentWorkflow string

const (
	ExperimentWorkflowReview ExperimentWorkflow = "review"
	ExperimentWorkflowCI     ExperimentWorkflow = "ci"
)

type ExperimentSubject struct {
	Repository string
	Branch     string
}

type ExperimentSelectionInput struct {
    Workflow ExperimentWorkflow
    Subject  ExperimentSubject
    Global   *Config
    Repo     *RepoConfig
    RawRepo  map[string]any
}

type ExperimentSelection struct {
    RepoConfig    *RepoConfig
    RawRepoConfig map[string]any
    SubjectHash   string
    Assignment    *ExperimentAssignment
}

func SelectReviewExperiment(
	in ExperimentSelectionInput,
) (ExperimentSelection, error)
```

The real types may group the raw and typed repo configuration differently, but
the interface must preserve these properties:

- one call selects and applies an experiment;
- an absent assignment is a normal, non-experimental result;
- callers receive a typed effective repository config;
- all validation errors are returned before any job is enqueued;
- the module owns canonical definition and subject hashes.

The enqueue boundary owns `effective_config_hash` because only it has the final
standalone or panel plan after explicit request values and agent availability
resolution.

The deletion test for this module is strong: without it, deterministic
assignment, merge rules, validation, and fingerprints would reappear in the
single-review, local-panel, and CI-panel enqueue paths.

### Raw repository configuration

Applying a partial overlay requires knowing whether zero values were explicitly
set. The local and ref-aware repository config loaders must therefore make the
raw TOML map available alongside the typed `RepoConfig`. Re-encoding a typed
config is not equivalent because it loses omission information. The two forms
must come from the same file snapshot. If an experiment is selected and a typed
repository config has no paired raw map, selection fails rather than fabricating
raw configuration from typed values or silently dropping repository overrides.

Existing config-taking resolvers should consume the returned effective
`RepoConfig`. An enqueue path must not reload `.roborev.toml` after selection,
because doing so would discard the overlay.

## Subject and assignment algorithm

The canonical subject tuple is the canonical repository identity and branch
name. Ordinary reviews use the local branch, and CI-poller reviews use the PR
head branch.

The local repository identity uses the stored canonical remote identity when
available and the canonical repository root as a local fallback. Only the hash
is persisted, so a fallback path is not exported as machine-specific data.

Tuple fields use a length-prefixed encoding rather than string concatenation.
The subject hash is:

```text
SHA-256("roborev-review-subject-v1" || encoded subject tuple)
```

Assignment then hashes the immutable experiment ID with the subject hash:

```text
SHA-256("roborev-review-experiment-v1" || experiment ID || subject hash)
```

Interpret the first 64 bits as an unsigned big-endian integer. The experimental
arm is selected when that value falls below the exact integer threshold derived
from `ratio`; otherwise the default arm is selected. `ratio = 0.0` and
`ratio = 1.0` are handled without floating-point boundary ambiguity.

Changing a commit, PR number, review type, or panel does not change assignment.
Renaming a branch does change its subject and therefore may change assignment.

## Enqueue flows

```text
global + repo config       branch subject
          |                      |
          +--------+-------------+
                   v
       SelectReviewExperiment
                   |
          effective RepoConfig
          + assignment or nil
                   |
          explicit request values
                   |
          resolve review or panel
                   |
         build immutable job plan
                   |
     persist jobs + assignment atomically
```

### Ordinary single review

The daemon computes the subject from the resolved repository and request branch
before resolving the agent or default panel. Selection can therefore change the
agent, model, reasoning, reuse setting, or whether a panel is chosen.

Explicit request fields are applied after the selection result. The final
immutable job plan supplies the assignment's effective-config hash. The job and
assignment are inserted in one transaction.

### Ordinary panel review

Selection occurs before `ResolvePanel`. The overlay may change the selected
panel, its members, or synthesis settings. The resulting run has one assignment
keyed by its panel-run UUID. Member and synthesis projections derive that
assignment at read time.

Session reuse is evaluated independently for each member after the panel plan is
resolved. Synthesis is always fresh.

### CI-poller panel review

`internal/github.OpenPullRequest` and the daemon's `ghPR` gain the PR head ref
name and head repository full name. These values come from the trusted GitHub
response, not from the checked-out PR branch.

The CI poller continues loading `.roborev.toml` from the protected default
branch. It retains both the typed and raw repo config, selects the experiment,
then resolves the CI matrix or named panel from the effective config.

The assignment, CI panel mapping, retry-attempt reservation, member jobs, and
synthesis job are committed in the existing `CreateCIPanelRun` transaction. A
concurrent loser creates none of them.

CI jobs store the PR head branch in `review_jobs.branch` and the target branch
in `review_jobs.ci_base_branch`. Local fix and refine discovery excludes CI jobs
using their existing source marker, while event and hook matching uses the CI
base branch.

## Persistence

Use one SQLite migration and the corresponding next PostgreSQL schema version.
The migration creates two tables and adds review-job lineage fields.

### Experiment definitions

```sql
CREATE TABLE experiment_definitions (
  experiment_id TEXT PRIMARY KEY,
  definition_hash TEXT NOT NULL,
  definition_json TEXT NOT NULL,
  first_seen_at TEXT NOT NULL,
  source_machine_id TEXT NOT NULL,
  synced_at TEXT
);
```

`definition_json` is the canonical form of the ratio, workflow scope, and
overlay. It excludes `enabled`. Inserting an already-known ID with another hash
returns an experiment-definition conflict and aborts enqueue.

Keeping the definition separate makes immutability enforceable and preserves the
historical meaning after the config file changes or disappears.

### Experiment assignments

```sql
CREATE TABLE experiment_assignments (
  review_unit_kind TEXT NOT NULL,
  review_unit_uuid TEXT NOT NULL,
  experiment_id TEXT NOT NULL REFERENCES experiment_definitions(experiment_id),
  arm TEXT NOT NULL,
  subject_hash TEXT NOT NULL,
  effective_config_hash TEXT NOT NULL,
  effective_config_json TEXT NOT NULL,
  assigned_at TEXT NOT NULL,
  source_machine_id TEXT NOT NULL,
  synced_at TEXT,
  PRIMARY KEY (review_unit_kind, review_unit_uuid, experiment_id),
);
```

The application validates review-unit kinds and arm names. The database does
not duplicate those closed sets as `CHECK` constraints, so adding a future unit
kind or arm does not require a schema migration solely to relax validation.

The application enforces one row per review unit today. The primary key already
permits a different experiment ID to be attached later, so future concurrent or
active/passive designs do not require replacing the storage model.

`effective_config_hash` fingerprints only the immutable standalone job plan or
complete panel plan after explicit request values and availability resolution.
It does not include unrelated raw repository settings that are resolved later
by prompt or worker code.

`effective_config_json` stores that same canonical frozen plan privately with
the assignment. It is not included in review projections or exports. A manual
standalone rerun uses it to restore the attributed primary and backup plan after
an execution-time failover has changed the mutable job row.

There is no polymorphic foreign key from `review_unit_uuid`: `job` refers to a
`review_jobs.uuid`, while `panel` refers to `panel_run_uuid`. Storage methods
create assignments in the same transaction as their review unit, and read
methods resolve the appropriate identifier.

### Review-job fields

Add these nullable fields to `review_jobs`:

```sql
resume_source_job_uuid TEXT
```

`resume_source_job_uuid` identifies the prior job whose session ID was selected
for this attempt. It is null for a fresh attempt. The existing `session_id`
continues to hold the actual session identifier.

Retries that discard a failed resume and run fresh clear both `session_id` and
`resume_source_job_uuid`. Re-enqueuing an experimental standalone job preserves
both its assignment and its complete frozen execution plan on the same row;
configuration changes made after the original enqueue do not affect that rerun.
Non-experimental standalone reruns retain their existing behavior of
re-resolving implicit execution settings. A manual panel rerun creates a new
panel run UUID but deliberately clones the original assignment and frozen member
and synthesis plans; it is a continuation, not fresh enrollment. A newly
requested review or panel performs deterministic selection from the currently
enabled definition.

Panel reruns verify the stored plan hash and match every member by its stable
name and index before creating the replacement run. A malformed plan, hash
mismatch, duplicate member, missing member, extra member, or synthesis identity
mismatch rejects the rerun before any new job is inserted.

Experiment definitions and assignments are retained as immutable audit records
when repository deletion removes their review jobs. They no longer appear in
job-based projections or exports, but remain available to synchronization until
the data store itself is retired.

## Session reuse

Replace the single-agent-only lookup with one reusable-session module used by
single reviews and panel members. Its input is a fully resolved job plan, not a
partially resolved config.

A candidate must match:

- repository;
- source machine ID, because supported agent session state is local;
- branch name and workflow source;
- experiment assignment state: no assignment, or the same experiment ID, arm,
    and definition hash;
- agent, model, provider, reasoning, and review type;
- worktree path for local worktree reviews;
- panel identity for panel members, including member name and resolved member
    configuration;
- a reusable session ID and successful terminal review status.

Panel members may resume only from prior panel members. A single review may
resume only from a prior single review. Synthesis jobs are never candidates or
consumers.

The existing safety checks remain:

1. The candidate target must be an ancestor of the new target.
1. The new target must be no more than 50 commits ahead.
1. Candidates are checked newest first, skipping invalid or unreachable ones.

If reuse is enabled but no candidate qualifies, the job runs fresh and captures
a session for the next review. This is the normal first treatment exposure on a
branch.

Enqueue stores `resume_source_job_uuid` only after resolving a session-capable
agent and a valid candidate created by the same machine. The worker clears the
lineage if failover or retry turns the attempt into a fresh run. A
non-session-capable agent remains a valid experiment parameter; it simply has no
reusable-session candidate.

## Structured projections and exports

Represent assignments as an array even though its maximum length is one in this
version:

```json
{
  "experiments": [
    {
      "id": "review-session-resumption-v1",
      "arm": "experiment",
      "subject_hash": "...",
      "definition_hash": "...",
      "effective_config_hash": "..."
    }
  ]
}
```

Add this field to review/job projections, `export reviews`, CI metrics, and CI
cost exports. Panel members and synthesis derive the array from the panel-run
assignment. Add `resume_source_job_uuid` to job-level and subagent projections
where session lineage is meaningful.

Do not render these fields in normal terminal review output, TUI review prose,
browser review prose, GitHub comments, or synthesis prompts. Diagnostic logs may
include experiment ID and arm but must not include raw private branch identity.

## PostgreSQL sync

Sync definitions before assignments on push. Pull definitions before assignments
and jobs before any projection that resolves a job assignment.

Definitions are insert-only. A remote row with the same ID and another hash is a
hard sync conflict; neither side overwrites the other. Assignments are also
immutable and use their composite primary key for idempotent insertion.

The two new `review_jobs` fields participate in the existing job upsert and
attempt-reset semantics. A retry reset clears `resume_source_job_uuid` together
with `session_id` and token usage.

## Validation and errors

Global and repository config normalization validates:

- IDs are nonempty TOML table keys;
- ratios are finite and within `[0, 1]`;
- workflows are known and nonempty;
- an enabled definition has a valid overlay;
- overlays contain only allowed review-time paths;
- overlays do not contain `experiments`;
- enabled definitions do not overlap on a workflow;
- repository entries for global IDs override only `enabled`;
- a repo-defined ID supplies a complete definition.

Selection or persistence errors abort enqueue. In particular, the CI poller must
not treat an invalid enabled experiment as the ordinary nonfatal `.roborev.toml`
fallback case. It should log the configuration error and publish an error status
for that PR head rather than review under an unrecorded default.

## Verification strategy

Tests should exercise owned behavior through the experiment-selection and
enqueue interfaces.

### Configuration and selection

- The same experiment and branch subject produce the same arm.
- Repository namespacing separates equal branch names.
- Ratios zero and one select the expected arms.
- Missing branch identity produces no assignment.
- Recursive table merge, scalar replacement, and array replacement produce the
    effective typed repo config.
- Invalid paths, recursive experiments, overlapping enabled scopes, and illegal
    repo overrides return focused validation errors.
- Reusing an ID with a changed definition hash is rejected.

### Storage and enqueue integration

- A single job and its assignment commit or roll back together.
- A panel stores one assignment and all projected jobs derive it.
- A concurrent CI-panel loser stores no assignment.
- Re-enqueuing an experimental standalone job preserves its assignment and
    frozen agent, model, provider, reasoning, severity, and backup plan even
    after configuration changes. A manual panel rerun clones the original
    assignment and plan, while a newly requested panel receives the
    deterministic current assignment.
- A fresh retry clears resume lineage.

### Session reuse

- Two successive targets on the same subject and compatible member plan resume
    from the earlier job.
- A different arm, member, agent, model, provider, reasoning, review type, or
    non-ancestor target does not reuse the session.
- A synchronized job created on another machine does not supply a session.
- Synthesis never receives a session ID.
- A first eligible treatment job runs fresh and a later job can resume it.

### Sync and exports

- Definition and assignment rows round-trip through sync without changing hashes
    or arms.
- Conflicting definition hashes surface as a sync conflict.
- Review and CI exports attribute single jobs and whole panels correctly.

Do not add tests for SHA-256, TOML-library round trips, SQL schema text, or the
absence of experiment metadata in prose. Those are dependency behavior or
implementation-text assertions. Verify prose exclusion by reviewing the data
flow and rendered projections that actually consume structured fields.

## Implementation sequence

1. Add experiment config types, raw repo loading, validation, deterministic
    selection, overlay merge, and canonical fingerprints.
1. Add the single SQLite migration and next PostgreSQL schema, storage methods,
    job fields, and sync support.
1. Integrate selection and atomic assignment persistence with ordinary single
    and panel enqueue paths.
1. Carry GitHub head branch identity into the CI poller, apply
    selection before panel resolution, and persist it in `CreateCIPanelRun`.
1. Generalize reusable-session lookup for single reviews and panel members,
    including assignment isolation and resume lineage.
1. Add structured projections, exports, configuration docs, and user-facing
    examples.

Each step should leave non-experimental reviews behaviorally unchanged. The
schema work remains one migration for the implementation PR, and no shipped
migration is edited.

## Rejected alternatives

### Hash the head SHA

The same branch would switch arms after pushes. This is especially invalid for a
stateful session-resumption treatment.

### Use PR number as the subject

It would not cover ordinary user reviews and would make the assignment unit a PR
rather than the requested branch.

### Put experiment logic in the GitHub workflow

The generated workflow is daemon-free and has neither persistent job history nor
session continuity. It would also omit ordinary daemon reviews.

### Store one experiment column on every job

It duplicates panel-level state and forces another schema replacement when a
review may participate in several experiments later.

### Let several enabled overlays stack

The result would be an implicit factorial experiment with ambiguous attribution.
The persistence model permits that future, but selection rejects it now.

### Add experiment metadata to prompts or PR comments

It could alter reviewer behavior and exposes operational rollout information to
contributors without helping review quality.
