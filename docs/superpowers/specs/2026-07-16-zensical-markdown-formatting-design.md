# Zensical Markdown Formatting Design

## Goal

Keep every Markdown page published by Zensical consistently wrapped at 80
columns so documentation diffs remain easy to review, without changing table
layout.

## Scope

The formatter will operate on the Markdown source set derived from
`docs/zensical.toml`: `docs/index.md` plus every Markdown path in
`project.nav`. This reuses `docs/scripts/public_markdown_sources.py`, so the
formatting scope stays aligned with the pages Zensical publishes as navigation
changes over time.

The initial change will reformat all existing source pages. Markdown files that
are not Zensical inputs, generated `docs/site/` content, virtual environments,
and cached files are outside the scope.

## Formatting Behavior

Pin `mdformat` as a documentation development dependency and invoke it through
the existing `docs` uv project. A small repository script will provide two
modes:

- write mode reformats every Zensical Markdown source at an 80-column wrap;
- check mode exits nonzero when any source differs from the formatted result
  and does not modify files.

Markdown table blocks must be preserved byte-for-byte in both modes. The
wrapper will protect table blocks from formatter rewrites while allowing prose
around them to wrap normally. Fenced code, URLs, and other Markdown constructs
that cannot be safely wrapped may remain longer than 80 columns according to
the formatter's syntax-aware behavior.

## Developer And CI Integration

The Makefile will expose an explicit formatting target and a non-mutating check
target. The check target will be registered as an always-run local `prek` hook,
matching the repository's existing lint convention.

The GitHub Actions lint job will run for Markdown and formatting-tool changes,
not only Go changes. It will install the pinned documentation environment and
run the non-mutating Markdown check alongside the existing Go lint behavior.
This ensures Markdown-only pull requests receive a CI lint result.

Developer documentation will describe how to apply formatting and how the
pre-commit and CI checks enforce it.

## Testing And Validation

Focused tests will exercise the wrapper against temporary Markdown sources and
configuration. They will verify:

- prose over 80 columns fails check mode and is wrapped in write mode;
- check mode never mutates input;
- Markdown tables remain byte-for-byte unchanged;
- the source list comes from the Zensical configuration;
- formatter failures are reported with a nonzero exit status.

Repository validation will include the focused tests, the Markdown check over
all reformatted sources, the full pre-commit suite, the documentation build,
and the relevant CI workflow syntax check.
