# Integrate Documentation Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move roborev's Zensical docs into this repository while keeping all image media in `docs-assets` and `docs-generated-assets` orphan branches and preserving `make docs-deploy`.

**Architecture:** `main` owns docs source and tooling under `docs/`, plus validation scripts and Make targets. Hydration scripts populate ignored `docs/assets/static/` and `docs/assets/generated/` directories from local or remote orphan branches before Zensical builds. Asset update scripts create single-commit orphan refs without switching the developer's current branch.

**Tech Stack:** Bash, Make, uv, Zensical 0.0.43, Python validation scripts, git orphan asset refs.

---

### Task 1: Docs Structure Check

**Files:**
- Create: `scripts/check-docs.sh`
- Modify: `Makefile`

- [ ] **Step 1: Write the failing docs structure check**

Create `scripts/check-docs.sh` to require the integrated docs files, the docs Make targets, the Vercel build wrapper, hydration scripts, and no tracked files under `docs/assets/static/` or `docs/assets/generated/`.

- [ ] **Step 2: Run the check to verify it fails**

Run: `bash scripts/check-docs.sh`

Expected: FAIL because integrated docs files and Make targets do not exist yet.

- [ ] **Step 3: Add docs Make targets**

Add `docs-install`, `docs-build`, `docs-serve`, `docs-check`, `docs-assets-branch`, `docs-generated-assets-branch`, and `docs-deploy` to `Makefile`.

- [ ] **Step 4: Run the check again**

Run: `bash scripts/check-docs.sh`

Expected: still FAIL because docs source and hydration scripts are not imported yet.

- [ ] **Step 5: Commit the red check and Make target shell**

Commit once the failing check documents the required behavior.

### Task 2: Import Docs Source

**Files:**
- Create/modify: `docs/**/*.md`, `docs/stylesheets/extra.css`, `docs/javascripts/lightbox.js`, `docs/zensical.toml`, `docs/pyproject.toml`, `docs/uv.lock`, `docs/vercel.json`, `docs/vercel-build.sh`, `docs/zensical-docs.sh`, `docs/scripts/check_built_site.py`, `docs/scripts/check_vercel_redirects.py`, `docs/overrides/**`
- Modify: `.gitignore`
- Modify: `README.md`

- [ ] **Step 1: Copy non-media docs source from `~/code/roborev-docs`**

Copy Markdown, CSS, JavaScript, overrides, Python check scripts, Vercel config, uv metadata, and Zensical config into `docs/`. Do not copy image media into tracked paths.

- [ ] **Step 2: Rewrite media references**

Rewrite curated asset references to `/assets/static/...` and generated screenshot references to `/assets/generated/...`. Update `docs/zensical.toml` logo/favicon paths and Open Graph metadata. Update README image links away from `roborev-docs`.

- [ ] **Step 3: Add the Zensical wrapper**

Add `docs/zensical-docs.sh`, adapted from kata, so builds run from `docs/` with a temporary copied docs directory.

- [ ] **Step 4: Run the docs structure check**

Run: `bash scripts/check-docs.sh`

Expected: FAIL only on missing hydrated asset branches or missing asset scripts if those are not implemented yet.

- [ ] **Step 5: Commit docs source import**

Commit the source import separately from asset branch creation.

### Task 3: Asset Branch Scripts And Initial Orphan Refs

**Files:**
- Create: `docs/assets/hydrate-assets.sh`
- Create: `docs/assets/update-static-assets-branch.sh`
- Create/modify: `docs/screenshots/**`
- Modify: `.gitignore`

- [ ] **Step 1: Add hydration script**

`docs/assets/hydrate-assets.sh` should hydrate `docs/assets/static/` from `docs-assets` and `docs/assets/generated/` from `docs-generated-assets`, fetching shallow refs from `origin` if local branches are missing.

- [ ] **Step 2: Add static asset branch update script**

`docs/assets/update-static-assets-branch.sh` should create/update `docs-assets` from `docs/assets/static/` using a temporary git repository and `git fetch`, without switching branches.

- [ ] **Step 3: Import screenshot generation scripts**

Copy the Docker/tmux/freeze screenshot scripts into `docs/screenshots/`, adapt output paths to `docs/assets/generated/`, and add `docs/screenshots/update-generated-assets-branch.sh`.

- [ ] **Step 4: Create initial local orphan refs**

Create `docs-assets` from the curated media in `~/code/roborev-docs/docs` and `docs-generated-assets` from the generated CLI/TUI screenshots in that same source tree. Do this without changing the current worktree branch.

- [ ] **Step 5: Hydrate from the local refs**

Run: `bash docs/assets/hydrate-assets.sh`

Expected: PASS and populate ignored `docs/assets/static/` and `docs/assets/generated/`.

- [ ] **Step 6: Run the docs structure check**

Run: `bash scripts/check-docs.sh`

Expected: PASS for structure and hydration.

- [ ] **Step 7: Commit asset workflow scripts**

Commit scripts and ignore rules; do not commit hydrated assets.

### Task 4: Build And Publish Workflow Validation

**Files:**
- Modify: `scripts/update-docs.sh`
- Modify: `docs/development.md`

- [ ] **Step 1: Add docs update helper**

Create `scripts/update-docs.sh` to require a clean tracked tree, run `make docs-install`, update and push `docs-generated-assets`, clear and hydrate assets, run `make docs-build`, run `make docs-check`, and run `make docs-deploy`.

- [ ] **Step 2: Document deployment**

Update docs development/deploying content so users link Vercel from the repo root and deploy with `scripts/update-docs.sh` or `make docs-deploy`.

- [ ] **Step 3: Run docs build**

Run: `make docs-build`

Expected: Zensical builds `docs/site` successfully.

- [ ] **Step 4: Run docs check**

Run: `make docs-check`

Expected: PASS, including built-site and redirect checks.

- [ ] **Step 5: Commit publish workflow**

Commit the deploy helper and docs updates.

### Task 5: Final Verification

**Files:**
- No planned edits.

- [ ] **Step 1: Verify no tracked media assets**

Run: `git ls-files docs | rg '\\.(png|svg|jpg|jpeg|webp|gif)$'`

Expected: no output except non-media docs source should not match.

- [ ] **Step 2: Verify asset refs exist**

Run: `git show-ref --heads docs-assets docs-generated-assets`

Expected: both refs exist locally.

- [ ] **Step 3: Run repository checks**

Run: `make docs-check`

Expected: PASS.

- [ ] **Step 4: Inspect status and commit state**

Run: `git status --short --branch`

Expected: branch contains committed docs changes; ignored hydrated assets and site output may exist but are not tracked.
