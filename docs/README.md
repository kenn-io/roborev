# roborev docs maintainer guide

This directory contains the Zensical source for <https://roborev.io>. The docs
source lives on `main`; image media lives on orphan asset branches so normal
clones do not pull large screenshots and PNGs into the main history.

## Layout

- `*.md`, `guides/`, `advanced/`, `integrations/`, `agents/`: public docs
  source.
- `zensical.toml`: Zensical site configuration and navigation.
- `pyproject.toml` and `uv.lock`: pinned docs toolchain.
- `vercel.json` and `vercel-build.sh`: Vercel project configuration.
- `assets/hydrate-assets.sh`: hydrates ignored local assets from orphan
  branches.
- `assets/update-static-assets-branch.sh`: updates curated static assets.
- `screenshots/`: Docker/tmux/freeze screenshot generator and generated asset
  branch updater.
- `scripts/check_built_site.py` and `scripts/check_vercel_redirects.py`:
  post-build validation.

`docs/assets/static/`, `docs/assets/generated/`, `docs/site/`, and `docs/.venv/`
are ignored local outputs.

## Asset Branches

- `docs-assets`: curated static media, including logos, favicons, Open Graph
  images, diagrams, agent icons, and manually captured integration images.
- `docs-generated-assets`: generated CLI and TUI screenshots.

Docs pages should reference media through:

- `/assets/static/...` for curated assets.
- `/assets/generated/...` for generated screenshots.

Do not commit image media to `main`.

## Local Development

Install the docs toolchain:

```bash
make docs-install
```

Hydrate assets and build:

```bash
make docs-build
```

Preview locally:

```bash
make docs-serve
```

Run all docs validation:

```bash
make docs-check
```

`make docs-check` hydrates assets, runs a strict Zensical build, checks generated
links/assets/metadata, and validates `vercel.json` redirects.

Asset hydration force-fetches `origin/docs-assets` and
`origin/docs-generated-assets` by default so force-pushed orphan branches do not
silently leave stale local media in place. Set
`ROBOREV_DOCS_USE_LOCAL_ASSET_BRANCHES=1` only when you intentionally want to
hydrate from local asset branch refs instead of origin.

## Updating Generated Screenshots

Regenerate generated CLI/TUI screenshots and update the local
`docs-generated-assets` orphan branch:

```bash
make docs-generated-assets-branch
```

Push that branch when the generated screenshots should be published:

```bash
bash docs/screenshots/update-generated-assets-branch.sh --push
```

The script writes screenshots to ignored `docs/assets/generated/`, validates the
expected SVGs, creates a temporary git repository with a single commit, then
fetches that commit into `docs-generated-assets`. It does not switch branches.
Screenshot data is derived from the maintainer's local roborev database, selected
only from committed review jobs for repos whose identity or `origin` remote
matches the canonical public `kenn-io/roborev`, `kenn-io/kata`,
`kenn-io/msgvault`, and `kenn-io/agentsview` repositories. The demo DB keeps
public repo/job metadata such as repo names, refs, branches, statuses, timings,
verdict mix, token usage, prompts, diffs, and review output so screenshots stay
representative of real open-source review data. The copy omits responses and
sanitizes local paths, local usernames, email addresses, and credential-shaped
tokens before Docker renders screenshots. The generated demo database is written
to `$TMPDIR/roborev-demo-data`.

For the initial import or a manual refresh from an existing directory:

```bash
bash docs/screenshots/update-generated-assets-branch.sh --source /path/to/assets --push
```

## Updating Static Assets

Hydrate or stage curated media under ignored `docs/assets/static/`, then update
the local `docs-assets` orphan branch:

```bash
make docs-assets-branch
```

Push it only when curated static assets should be published:

```bash
bash docs/assets/update-static-assets-branch.sh --push
```

This branch is separate from `docs-generated-assets` so normal screenshot
regeneration cannot accidentally overwrite curated media.

## Publishing

The Vercel project should be linked from the repository root with `docs/` as the
Vercel root directory:

| Setting | Value |
| --- | --- |
| Framework preset | `Other` |
| Root directory | `docs` |
| Install command | `uv sync --frozen --no-dev` |
| Build command | `uv run --frozen bash ./vercel-build.sh` |
| Output directory | `site` |

Link the checkout once from the repository root:

```bash
vercel link
```

When prompted, choose the roborev docs Vercel project. The generated `.vercel/`
directory is local-only and ignored.

Deploy committed docs changes with:

```bash
scripts/update-docs.sh
```

That helper requires a clean tracked tree, installs the docs toolchain,
regenerates and pushes `docs-generated-assets`, clears and rehydrates local
assets, builds, checks, and then runs:

```bash
make docs-deploy
```

Create a Vercel preview/staging deployment before production with:

```bash
make docs-deploy-staging
```

Use `make docs-deploy` directly only when the asset branches and local build
state are already correct.
