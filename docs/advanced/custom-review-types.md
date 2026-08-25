---
title: Custom Review Types
description: Define reusable schema-constrained reviews with Go templates
---

Custom review types let you give a domain-specific review a name and run it
through the normal roborev workflows:

```bash
roborev review --type thermonuclear
roborev review --branch --type thermonuclear
```

Define a type in `.roborev.toml` for one repository or in
`~/.roborev/config.toml` to make it available globally:

```toml
[review.types.api-contract]
template = ".roborev/reviews/api-contract.md"
agent = "codex"
reasoning = "thorough"
```

The template supplies the review rubric. Roborev still constructs the commit or
range context, project guidelines, diff, and oversized-diff snapshot
instructions. It also owns the output schema, severity filtering, pass/fail
verdict, and final Markdown rendering.

## Configuration reference

Each `[review.types.<name>]` table supports these fields:

| Field | Required | Description |
|-------|----------|-------------|
| `template` | Yes | Path to the Go template containing the review rubric |
| `includes` | No | Map of names to files made available through `.Includes` |
| `agent` | No | Agent override for this type |
| `model` | No | Model override for this type |
| `reasoning` | No | Reasoning override for this type |

Names must start with a lowercase letter and contain only lowercase letters,
digits, and hyphens. Names may not replace built-in types or their aliases:
`default`, `review`, `general`, `security`, `design`, and `lookahead`. Names
used by other built-in workflows are also reserved: `fix`, `refine`, and
`classify`.

A repository definition replaces a global definition with the same name. Its
fields are not merged individually.

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
| `.ReviewType` | string | Configured type name, such as `thermonuclear` |
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
  "schema_version": 1,
  "summary": "Overall assessment",
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

`schema_version`, `summary`, and `findings` are required. The current schema
version is `1`; Roborev rejects missing or unsupported versions. Every finding
requires `severity`, `problem`, `fix`, and `location`; use `null` when a finding
has no location. Severity must be one of `critical`, `high`, `medium`, or `low`.

Roborev removes findings below `review_min_severity` or `--min-severity`. The
review passes when no findings remain, and fails otherwise. Built-in review
types keep their existing prose output path.

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
