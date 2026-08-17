# Documentation Screenshot Pipeline

The Dockerized screenshot pipeline creates the terminal and browser images used
by the documentation site. It follows the same shape as the AgentsView docs
pipeline: prepare a reduced database on the host, build the release application
in Docker, and drive the UI at fixed dimensions.

## Safety

The source database is opened read-only. The extractor accepts only the
canonical public `github.com/kenn-io/roborev` repository, copies completed
commit reviews, rewrites local paths and credential-shaped strings, and applies
the private terms file at `$KENN_PRIVATE_TERMS_FILE` or
`~/.config/kenn/private-terms.txt` when present. The daemon and browser run only
inside Docker against the reduced copy.

## Generate Screenshots

From the repository root:

```bash
make docs-screenshots
```

Set `ROBOREV_DOCS_SOURCE_DB` to use a source other than
`~/.roborev/reviews.db`. The output is written to the ignored
`docs/assets/generated/` directory.

Terminal captures use tmux and Freeze. The browser capture uses Playwright with
a 1440 x 900 dark-mode Chromium viewport. The browser test opens the newest
visible failing review so the image shows both the review queue and its detail
drawer.

To replace the local orphan asset branch after inspecting every generated
image:

```bash
bash docs/screenshots/update-generated-assets-branch.sh --skip-generate
```

Add `--push` only when the updated `docs-generated-assets` branch is ready to
publish.
