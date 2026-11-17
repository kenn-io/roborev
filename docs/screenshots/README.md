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

The host step needs [mise](https://mise.jdx.dev/). `prepare-demo-db.sh`
installs the uv version pinned in the root `mise.toml` and `mise.lock`, and uv
supplies a managed Python 3.14. The script does not use the `python3` on your
`PATH`.

From the repository root:

```bash
make docs-screenshots
```

Set `ROBOREV_DOCS_SOURCE_DB` to use a source other than
`~/.roborev/reviews.db`. Set `ROBOREV_DOCS_HOME` to redact a home directory
other than `$HOME`; the private terms file default moves with it. The output is written to the ignored
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

## Capture From an Approved Corpus

An automated screenshot runner has no `~/.roborev/reviews.db`, so it captures
from a reviewed corpus instead. After preparing and inspecting a demo database,
write it as a corpus generation:

```bash
python3 docs/screenshots/corpus.py write \
  "${TMPDIR:-/tmp}/roborev-demo-data/reviews.db" GENERATION_DIR
```

The command prints the manifest's SHA-256, which pins the generation. A runner
then sets `DOCS_SCREENSHOTS_OUTPUT` to a new directory, `DOCS_SCREENSHOTS_CORPUS`
to the generation's `manifest.json`, and `DOCS_SCREENSHOTS_DATE` to the
corpus reference time, and runs `make docs-screenshots`. The target checks the
database against the manifest before it builds anything, labels the build with
the newest release tag, measures the web view's relative times from the
reference time, and writes every file of the `docs-generated-assets` branch
under `assets/` in the output directory. The host needs Bash, Git, Python 3 and
Docker.
