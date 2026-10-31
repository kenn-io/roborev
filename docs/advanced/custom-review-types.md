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

To have your coding agent interview you and write this setup, invoke
`/roborev-setup-review` (`$roborev-setup-review` in Codex). See
[Agent Skills](../guides/agent-skills.md#set-up-a-custom-review). The skill also
helps calibrate the rubric: agree on a known mistake, a clean case, and expected
results before spending review calls. Run each case twice on fresh commits, then
rerun the whole set after every rubric edit. Check hooks and enabled review
experiments before trials; closing a trial cannot undo hooks that already ran.

The template supplies instructions for what the reviewer should check. Roborev
still constructs the commit or range context, project guidelines, diff, and
oversized-diff snapshot instructions. It also owns the output schema, severity
filtering, pass/fail verdict, and final Markdown rendering.

## Configuration reference

Each `[review.types.<name>]` table supports these fields:

| Field | Required | Description |
|-------|----------|-------------|
| `template` | Yes | Path to the Go template containing the review rubric |
| `includes` | No | Map of names to files made available through `.Includes` |
| `agent` | No | Agent override for this type |
| `model` | No | Model override for this type |
| `reasoning` | No | Reasoning override for this type |
| `tools` | No | Strict read-only tool policy with exact MCP tools; requires Claude Code. See [Strict Review Tool Policies](/docs/configuration/#strict-review-tool-policies) |
| `binary_evidence` | No | Give reviewers the committed copies of changed binary files. See [Binary evidence](#binary-evidence) |

Names must start with a lowercase letter and contain only lowercase letters,
digits, and hyphens. Names may not replace built-in types or their aliases:
`default`, `review`, `general`, `security`, `design`, `lookahead`, and
`test-value`. Names used by other built-in workflows are also reserved: `fix`,
`refine`, and `classify`.

A repository definition replaces a global definition with the same name. Its
fields are not merged individually.

### Granting MCP tools

A strict review type can name the exact MCP tools its reviewer may call. For
example, a citations rubric can look up sources while other types get no MCP
access:

```toml
[review.types.citations.tools]
mode = "read-only"

[review.types.citations.tools.mcp]
sources = ["read", "find"]
```

The operator's global config must define the `sources` server and allow both
tools. See
[Strict Review Tool Policies](/docs/configuration/#strict-review-tool-policies).

### Binary evidence

A text diff only says that a PDF or image changed. When a rubric needs to read
those files, for example to check a draft against its exhibits, set
`binary_evidence = true`:

```toml
[review.types.exhibits]
template = "rubrics/exhibits.md"
binary_evidence = true
```

For a review of a commit or a commit range, roborev saves the committed version
of each changed binary file under `snapshot_dir` (default `.roborev/`) in the
checkout where the reviewer runs. The prompt gains a "Committed review evidence"
section that lists each file's path, whether it was added, modified, or deleted,
and the saved copy to read. Deleted files are listed without a copy. The copies
come from Git, so later edits in the working tree do not change them. roborev
removes them when the review attempt ends.

The reviewer opens each copy with its own tools, so choose an agent that can
read the file type:

- Claude Code and Grok read PDFs and images directly.
- Codex reads images. It reads a PDF only by running a converter, such as
    `pdftotext`, that is installed on the machine running the review. It gets no
    text from a scanned PDF.

`exclude_patterns` applies to the evidence as well as the diff, so a binary file
that matches it gets no copy.

Only review types that set `binary_evidence` get the section; other panel
members are unchanged. Reviews of uncommitted changes fail with an error when
the type sets it. Keep `snapshot_dir` in `.gitignore`; `roborev init` adds it.
Keep committed rubrics outside `snapshot_dir`, because roborev will not write
snapshots into a directory that holds tracked files.

## Paths and includes

Repository and global configuration accept:

- Repository-relative paths, resolved separately for the repository being
    reviewed.
- Absolute paths.
- Home-relative paths beginning with `~/`.

Paths containing `..` and symlinks may also point outside the repository.
Roborev treats these paths as user-selected files, not as a security boundary.
In CI, repository-relative files that remain inside the repository are read from
the configured base ref. External paths are read from the filesystem.

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
  "schema_version": 3,
  "summary": "Overall assessment",
  "verdict": "fail",
  "findings": [
    {
      "severity": "high",
      "kind": "defect",
      "topic": "security",
      "confidence": 0.8,
      "problem": "What is wrong and why it matters",
      "fix": "A concrete correction",
      "file": "internal/file.go",
      "start_line": 42,
      "end_line": 45
    }
  ]
}
```

`schema_version`, `summary`, `verdict`, and `findings` are required. The current
schema version is `3`. Stored version `1` and `2` documents still load, and
Roborev rejects missing or unsupported versions. `verdict` is `pass`, `fail`, or
`unable_to_review`.

Every finding requires these members:

- `severity`: `critical`, `high`, `medium`, or `low`.
- `kind`: `defect` when the code behaves wrongly in a case the finding names, or
    `improvement` when the code works but should still change, such as a missing
    or weak test, wrong documentation, or structure that will cost later work.
- `topic`: `tests`, `documentation`, `security`, `performance`, or `design` when
    the finding is about one of those areas, otherwise `null`. A defect can have
    a topic, and so can an improvement.
- `confidence`: a number from 0 to 1. Roborev uses it only to break ties when it
    orders findings.
- `problem` and `fix`.
- `file`, `start_line`, and `end_line`: the path relative to the repository root
    and the line range in the reviewed code. Use `null` for the lines when a
    finding covers a whole file, and `null` for all three when it has no file.

`kind`, `topic`, and `confidence` are stored in the review document and exported
with it. They never appear in the rendered review or in pull request comments.

Before storing a review, Roborev checks every cited file against the reviewed
commit, or the working tree for uncommitted changes. A finding whose file does
not exist there keeps its text but loses its location, and a line range past the
end of the file is shortened. Roborev then merges findings with identical
problem and fix text at nearly the same lines of the same file. It orders the
rest by severity, then by how many panel members reported them, then by
confidence.

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

## Document review example

Custom types are not limited to code. This `.roborev.toml` reviews drafts of a
written report with two rubrics, batching five commits into one panel:

```toml
post_commit_review = "commit"
post_commit_batch_size = 5

[review]
hook_review_panel = "drafts"

[review.types.citations]
template = "rubrics/citations.md"
agent = "claude-code" # strict tool policies require Claude Code
binary_evidence = true

[review.types.citations.tools]
mode = "read-only"
mcp = { archive = ["lookup"] }

[review.types.quotes]
template = "rubrics/quotes.md"
binary_evidence = true

[review.subagents.citations]
review_type = "citations"

[review.subagents.quotes]
review_type = "quotes"

[review.panels.drafts]
members = ["citations", "quotes"]
synthesis_agent = "claude-code"
```

The citations rubric checks claims against the committed exhibits and may call
`archive/lookup` once the operator allows it (see
[Granting MCP tools](#granting-mcp-tools)). The quotes rubric compares
quotations with the exhibits and gets no extra tools. Commit source PDFs next to
the drafts, add the snapshot directory (`.roborev/` by default) to `.gitignore`,
and run `roborev init`. The combined review labels each finding with its rubric
and does not describe the material as code.

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
