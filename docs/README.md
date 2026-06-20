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
Screenshot data is generated from a deterministic synthetic fixture database in
`$TMPDIR/roborev-demo-data`; it does not read the maintainer's live roborev
database.

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
