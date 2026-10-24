---
title: Custom Review Types
description: Define reusable schema-constrained reviews with Go templates
---

Custom review types let you give a specialized review a name and run it through
the normal roborev workflows:

```bash
roborev review --type api-contract
roborev review --branch --type api-contract
```

Define a type in `.roborev.toml` for one repository or in
`~/.roborev/config.toml` to make it available globally:

```toml
[review.types.api-contract]
template = ".roborev/reviews/api-contract.md"
agent = "codex"
reasoning = "thorough"
```

The template supplies instructions for what the reviewer should check. Roborev
still constructs the commit or range context, project guidelines, diff, and
complete-prompt snapshot instructions. It also owns the output schema, severity
filtering, pass/fail verdict, and final Markdown rendering.

## Reviews of drafts and other material

A custom rubric can review committed drafts and other versioned material, not
only software. Roborev uses neutral wording in the generated task context,
history, metadata, and response guidance. It preserves your template,
guidelines, quoted findings, and other supplied text. Built-in review types keep
their existing prompts.

Use the same type name in a panel member's `review_type`:

```toml
[review.types.citations]
template = ".roborev/reviews/citations.md"
agent = "claude-code"

[review.subagents.sources]
review_type = "citations"

[review.panels.drafts]
members = ["sources"]
```

A global subagent may reference a custom type defined in each repository. Global
loading and global-only validation, including experiment overlays, check the
reference's syntax. Review resolution requires the type to exist in the
effective global/repository configuration; an unresolved global-only invocation
fails.

If any voting panel member uses a custom type, synthesis uses neutral wording
and labels its inputs with their rubrics. This includes failed or skipped custom
voters; non-voting observers do not affect synthesis. Findings merge only when
they describe the same problem. Silence from another rubric does not count as
disagreement. See [Subagent Review Panels](subagent-review-panels.md).

`compact` supports built-in review sources only. It rejects direct custom
reviews and panel parents with custom voting members, rather than substituting a
built-in rubric. `fix` and `refine` remain writable, code-framed workflows; they
do not promise to preserve a custom rubric during re-review.

## Committed evidence

The repo-local snapshot directory must contain no tracked files and stay
separate from committed rubrics and manifests. These examples set `snapshot_dir`
to `.roborev-snapshots/` so rubrics can remain under `.roborev/reviews/`.

Enable binary evidence on a custom type:

```toml
snapshot_dir = ".roborev-snapshots"

[review.types.citations]
template = ".roborev/reviews/citations.md"
binary_evidence = true
```

Roborev reads the reviewed commit's Git objects, rather than the current
working-tree files. Single commits, roots, ranges and merges use the same
selection as the review diff, including exclusions and rename behavior. A range
that includes the first commit also includes binary files introduced in that
commit. Committed endpoint attributes determine binary classification; a
different checkout's attributes do not change the evidence set. Evidence-enabled
preparation ignores Git replacement refs and disables text conversion and
external diff commands for the rendered comparison and prebuilt diff snapshots.

Each changed binary has an index entry with its escaped original path, change
kind, old or new side, Git blob ID, raw-byte SHA-256, byte count and media hint.
Deletions retain old bytes; modifications offer both sides. Equal content shares
one file within an attempt. Symlinks, submodules and mode-only changes have
explicit omissions and are not copied as ordinary binary files. Git LFS pointers
remain text pointers; Roborev does not fetch their remote payloads.

The files live in private, ignored attempt directories under `snapshot_dir` in
the actual agent checkout. Their names come from content hashes. Sniffed PDF and
supported image types get safe extensions for native readers. Files use mode
0600 and remain available even when the prompt fits inline. Unsupported media is
labelled as needing a supplied text rendition. Hashes identify the bytes
offered; they do not prove that the agent read or understood a file.

Stored and prebuilt prompts carry a versioned portable evidence footer. At
execution, Roborev validates it against the resolved settings and reviewed Git
objects, then binds fresh file paths. The supplied rubric, PR discussion and
member instructions survive retries. Prompt text does not grant tools or select
operator configuration. Cleanup covers successful and failed attempts; retries
reconstruct the pinned bytes and fail if required Git objects are missing.

The complete evidence index participates in the usual prompt preparation after
all attachments. Oversized prompts use the same complete-file transport as
ordinary reviews. Evidence files have no separate guessed size or page cap.
There is no machine-wide evidence cache or runtime network lookup.

Evidence applies only to the custom rubric that enables it. Other members in a
mixed panel keep their normal prompts. Dirty reviews reject enabled binary
evidence or a nonempty `evidence_manifest` before launching an agent; commit the
material first. Evidence-disabled dirty reviews retain their normal behavior.

## Required pinned text renditions

Use a manifest when a rubric needs exported text, including unchanged sources
that support a changed draft:

```toml
snapshot_dir = ".roborev-snapshots"

[review.types.citations]
template = ".roborev/reviews/citations.md"
evidence_manifest = "evidence/manifest.toml"
```

A nonempty `evidence_manifest` requires every listed rendition. It works with
`binary_evidence = false`, the default. Enable binary evidence as well if the
reviewer should receive changed source PDFs or images alongside exported text.

The manifest uses version 1:

```toml
version = 1

[[documents]]
id = "source-a"
text_path = "evidence/source-a.txt"
text_sha256 = "<64 lowercase hexadecimal characters>"
provenance = "docbank://vaults/V/documents/N/versions/C/renditions/A"
```

Replace the hash placeholder with the SHA-256 of the committed text bytes.

| Field | Required | Meaning |
|-------|----------|---------|
| `version` | Yes | Manifest format version; must be `1` |
| `documents[].id` | Yes | Nonempty unique document identity |
| `documents[].text_path` | Yes | Repository-root-relative path to committed UTF-8 text |
| `documents[].text_sha256` | Yes | SHA-256 of raw text bytes, as 64 lowercase hexadecimal characters |
| `documents[].source_sha256` | No | Declared source hash, in the same hexadecimal format; not locally verified |
| `documents[].provenance` | No | Opaque declared provenance; never fetched or verified |

The manifest and text files come from the reviewed head, including unchanged
entries. Paths are relative to the repository root, even when the manifest lives
in a subdirectory. They must name regular committed Git files. Absolute paths,
URLs, traversal, symlinks and submodules are rejected. Missing or excluded
required files fail preparation before the agent runs. Malformed manifests,
unknown fields, duplicate IDs, invalid hashes, hash mismatches and invalid UTF-8
also fail preparation. Roborev hashes raw bytes before validating UTF-8; line
endings are part of the identity.

Verified renditions use the same private snapshot lifecycle as binary files,
with content-named `.txt` files. The portable index records document identity,
text path, blob ID, verified text hash and declared metadata. Source hashes and
provenance are explicitly labelled as declared and unverified. A text hash
proves the offered exported bytes; it does not prove OCR fidelity, source
identity, or that the reviewer read them.

To prepare a pinned Docbank export outside Roborev:

1. Select a specific document version and rendition in Docbank. Export that
    rendition as UTF-8 text to `evidence/source-a.txt` using your source tool.
    Keep the pinned version and rendition identity for the provenance field.

1. Stage the exported text, then hash the staged Git blob. Git may normalize
    line endings during staging; Roborev verifies the committed bytes:

    ```bash
    git add evidence/source-a.txt
    git cat-file blob :evidence/source-a.txt | shasum -a 256
    ```

1. Put the resulting hash and pinned provenance in `evidence/manifest.toml`. If
    you have a source hash, record it as optional `source_sha256`; Roborev does
    not use it to claim source verification.

1. Commit the text and manifest with the draft, then review the committed head:

    ```bash
    git add evidence/manifest.toml draft.md
    git commit -m "Pin source rendition for draft review"
    roborev review --type citations --wait
    ```

Roborev does not contact Docbank, resolve the URI, or search for source bytes.
New exported bytes require an updated text hash and a new commit.

## Configuration reference

Each `[review.types.<name>]` table supports these fields:

| Field | Required | Description |
|-------|----------|-------------|
| `template` | Yes | Path to the Go template containing the review rubric |
| `includes` | No | Map of names to files made available through `.Includes` |
| `agent` | No | Agent override for this type |
| `model` | No | Model override for this type |
| `reasoning` | No | Reasoning override for this type |
| `binary_evidence` | No | Offer changed committed binary files; defaults to false |
| `evidence_manifest` | No | Repository-relative committed manifest of required text renditions; defaults to empty |

Names must start with a lowercase letter and contain only lowercase letters,
digits, and hyphens. Names may not replace built-in types or their aliases:
`default`, `review`, `general`, `security`, `design`, `lookahead`, and
`test-value`. Names used by other built-in workflows are also reserved: `fix`,
`refine`, and `classify`.

A repository definition replaces a global definition with the same name. Its
fields are not merged individually.

## Paths and includes

Template and include paths in repository and global configuration accept:

- Repository-relative paths, resolved separately for the repository being
    reviewed.
- Absolute paths.
- Home-relative paths beginning with `~/`.

Template and include paths containing `..` and symlinks may also point outside
the repository. Roborev treats these paths as user-selected files, not as a
security boundary. Evidence manifest and rendition paths follow the stricter
committed-file rules above. In CI, repository-relative files that remain inside
the repository are read from the configured base ref. External paths are read
from the filesystem.

Roborev does not fetch URLs while running a review. Download or vendor remote
rubrics first so reviews remain reproducible and do not depend on the network.

Named includes are useful when a small wrapper template needs to combine a
third-party rubric with local instructions:

```toml
[review.types.example]
template = ".roborev/reviews/example.tmpl"

[review.types.example.includes]
rubric = ".roborev/reviews/upstream-rubric.md"
policy = ".roborev/reviews/local-policy.md"
```

The wrapper can render them with Go's `index` function:

```gotemplate
# Example review

{{ index .Includes "rubric" }}

## Repository policy

{{ index .Includes "policy" }}
```

## Template values

Templates use Go's `text/template` syntax and receive these values:

| Value | Type | Description |
|-------|------|-------------|
| `.ReviewType` | string | Configured type name, such as `api-contract` |
| `.Includes` | map | Contents of every configured named include |

Invalid template syntax and execution errors fail prompt construction with a
configuration error. Template and include contents use the same configured
`max_prompt_size` budget as the rest of the review prompt.

Do not put diff placeholders in the custom template. Roborev appends the review
target and diff context after rendering it.

Queued reviews resolve their custom type when a worker starts them. If the type
has been removed or renamed since the job was queued, the job fails with a
configuration error; Roborev never substitutes the generic review prompt.

## Structured results and compatible agents

Custom types require an agent with native JSON Schema output support. Roborev
currently supports custom reviews with `codex`, `claude-code`, `pi`, and `grok`.
If the resolved agent lacks that capability, Roborev rejects the review before
enqueue instead of storing a job that can only fail or falling back to
unconstrained prose.

Roborev does not impose one shared minimum version across those independent
CLIs. The installed command must support the schema mechanism used by its
adapter: `--output-schema` for Codex, `--json-schema` for Claude Code and Grok,
and the `pi-json-schema` extension for Pi. An unsupported flag or missing Pi
extension is reported as an agent execution error; update the CLI or install the
extension before retrying the review.

The schema is fixed by roborev. The agent must return:

```json
{
  "schema_version": 2,
  "summary": "Overall assessment",
  "verdict": "fail",
  "findings": [
    {
      "severity": "high",
      "problem": "What is wrong and why it matters",
      "fix": "A concrete correction",
      "location": "optional/file.go:42"
    }
  ]
}
```

`schema_version`, `summary`, `verdict`, and `findings` are required. The current
schema version is `2`; stored version `1` documents (without a verdict) still
load, and Roborev rejects missing or unsupported versions. `verdict` is `pass`,
`fail`, or `unable_to_review`. Every finding requires `severity`, `problem`,
`fix`, and `location`; use `null` when a finding has no location. Severity must
be one of `critical`, `high`, `medium`, or `low`.

The agent's verdict is shown in the review as an assessment but does not change
the outcome: the findings and the severity threshold decide pass or fail. The
exception is `unable_to_review`, which means the agent could not assess the
change at all. Roborev treats that as an agent failure, so normal retry and
backup-agent failover apply instead of recording a clean pass. A `fail` verdict
with no findings is rejected the same way, because the agent has not said what
is wrong.

| Agent verdict | Findings | Review outcome |
|---------------|----------|----------------|
| `pass` or `fail` | Any finding at or above the threshold | Fail |
| `pass` or `fail` | Findings only below the threshold | Pass; findings stay in the output |
| `pass` | None | Pass |
| `fail` | None | Agent error; retried or failed over |
| `unable_to_review` | Any | Agent error; retried or failed over |

Panels apply the panel threshold to each member the same way. When every member
passes but some still carry lower findings, the synthesis agent combines them so
those findings reach the combined output.

Roborev keeps every finding in the review output. `review_min_severity` or
`--min-severity` only decides the verdict: the review fails when any finding is
at or above the threshold and passes otherwise. Built-in review types use the
same schema when the agent supports schema-constrained output, and fall back to
prose for other agents.

## Strict tools

Custom review types can request the Claude-only strict tool boundary:

```toml
[review.types.citations]
template = "reviews/citations.tmpl"

[review.types.citations.tools]
mode = "read-only"
mcp = {}
```

An empty MCP map explicitly selects strict execution without MCP access.
Nonempty grants launch only operator-authorized servers and grant exact tool
names. See [MCP authority](../configuration.md#strict-review-tool-policies) for
global server definitions and the operator ceiling. Unsupported adapters fail
before review invocation. If any voting member requests strict tools, synthesis
uses strict tools with no MCP access. Non-voting observers do not change
synthesis policy.

Strict reviews always start fresh sessions, including local and CI calls. See
[Claude Code review tools](../agents/index.md#strict-reviews) for startup
checks, session isolation, and the native CLI trust boundary. Fix and refine
retain their existing writable behavior.

## Writing useful severity guidance

The schema guarantees that each finding has a valid severity label, but it
cannot decide what severity means for your domain. Put a short, concrete rubric
in the prompt. Explain impact rather than relying on adjectives. For example:

```markdown
Classify findings by impact:

- critical: permits data loss, credential compromise, or remote code execution
- high: breaks a primary workflow or creates a likely production incident
- medium: causes incorrect behavior in a narrower or recoverable case
- low: creates a concrete maintainability or usability cost without incorrect behavior
```

Tell the reviewer to report all real findings. Roborev applies the configured
minimum after structured output is returned, so the template does not need to
implement threshold filtering.

When CI has no command-line `--reasoning` value or repository `[ci].reasoning`,
a custom type's `reasoning` field controls that type. An explicit CI value
applies to every review in the matrix.

## Thermonuclear example

Download the Cursor Team Kit rubric into the repository:

```bash
mkdir -p .roborev/reviews
curl -L \
  https://raw.githubusercontent.com/cursor/plugins/main/cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md \
  -o .roborev/reviews/thermonuclear-skill.md
```

Create `.roborev/reviews/thermonuclear.tmpl`:

```gotemplate
# Thermonuclear maintainability review

Apply the following rubric directly to the reviewed change. Treat its skill
metadata as descriptive text; do not invoke or delegate to another skill.

{{ index .Includes "rubric" }}

For every actionable finding, choose one of roborev's severity levels. Reserve
critical and high for concrete correctness, security, operability, or severe
maintainability risks. Use medium or low for narrower maintainability costs.
```

Then add this configuration:

```toml
[review.types.thermonuclear]
template = ".roborev/reviews/thermonuclear.tmpl"
agent = "codex"
reasoning = "thorough"

[review.types.thermonuclear.includes]
rubric = ".roborev/reviews/thermonuclear-skill.md"
```

Run it with:

```bash
roborev review --type thermonuclear --wait
```
