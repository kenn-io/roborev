# Integrate Documentation Design

## Goal

Move the roborev Zensical documentation from the separate `roborev-docs`
repository into the main roborev repository while keeping image media out of
`main` to avoid clone bloat. The resulting workflow should match kata's manual
Vercel publishing model, including `make docs-deploy`.

## Repository Layout

The roborev repository will own the docs source under `docs/`:

- Markdown pages
- `zensical.toml`
- `pyproject.toml` and `uv.lock`
- `vercel.json`
- `vercel-build.sh`
- CSS, JavaScript, templates, and docs maintenance scripts

Image media will not be committed to `main`. Docs pages will reference media
through stable `/assets/...` URLs that are hydrated before local and production
builds.

## Asset Branches

The repo will use two orphan asset branches:

- `docs-assets` for curated static media, including logos, favicons, Open Graph
  images, diagrams, agent icons, and manually captured integration images.
- `docs-generated-assets` for regenerated CLI and TUI screenshots produced by
  the Docker, tmux, and freeze screenshot workflow.

Both branches should be single-purpose branches with generated commits. The
generated screenshot update flow must update only `docs-generated-assets` during
normal docs deploys so curated media is not accidentally rewritten.

## Build And Hydration

The docs build will hydrate both asset branches into ignored directories under
`docs/assets/` before running Zensical. Hydration must:

- Use local branches when present.
- Fetch shallow copies from `origin` when local refs are missing.
- Fail with a clear message if expected assets cannot be found.
- Keep hydrated files ignored by git.

Vercel will use `docs/` as the project root with:

- Install command: `uv sync --frozen --no-dev`
- Build command: `uv run --frozen bash ./vercel-build.sh`
- Output directory: `site`

## Commands

Add Make targets that match kata's operator workflow:

- `make docs-install`
- `make docs-build`
- `make docs-serve`
- `make docs-check`
- `make docs-assets-branch`
- `make docs-generated-assets-branch`
- `make docs-deploy`

`make docs-deploy` should run `vercel deploy --prod` from the repository root.
The higher-level `scripts/update-docs.sh` helper should require a clean tracked
tree, update and push `docs-generated-assets`, hydrate both asset branches,
build and check the docs, then call `make docs-deploy`.

## Validation

The migration should be validated by:

- Hydrating assets from local orphan branches.
- Running a strict Zensical build through the same wrapper Vercel uses.
- Running docs structure and built-site checks.
- Confirming the generated site resolves local page links, media assets,
  metadata, and Vercel redirects.

## Non-Goals

This migration will not publish to Vercel automatically, remove the separate
`roborev-docs` repository, or change product documentation content beyond path
and workflow updates needed for in-repo hosting.
