# GoReleaser Linux Packages Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the hand-built release artifact workflow with GoReleaser and add Linux DEB/RPM release artifacts.

**Architecture:** GoReleaser becomes the single artifact builder and GitHub release publisher. The workflow still uploads generated tarballs as workflow artifacts so the existing Homebrew tap update job can keep consuming the same filenames and checksums.

**Tech Stack:** GoReleaser v2, nFPM through GoReleaser, GitHub Actions, Go 1.26.3, existing systemd user units.

---

## File Structure

- Create `.goreleaser.yaml`: GoReleaser build, archive, checksum, and Linux package definition.
- Modify `.github/workflows/release.yml`: replace manual matrix build/release jobs with a GoReleaser release job; keep Homebrew update separate.
- Modify `.gitignore`: keep `/.roborev/` ignored for local snapshot output.
- Create `docs/superpowers/plans/2026-06-02-goreleaser-linux-packages.md`: implementation plan.

### Task 1: Add GoReleaser Configuration

**Files:**
- Create: `.goreleaser.yaml`
- Modify: `.gitignore`

- [ ] **Step 1: Verify GoReleaser config is currently absent**

Run:

```bash
nix run 'nixpkgs#goreleaser' -- check
```

Expected: PASS with a message showing GoReleaser is using defaults
because no config file exists.

- [ ] **Step 2: Add `.goreleaser.yaml`**

Create `.goreleaser.yaml` with:

```yaml
version: 2

project_name: roborev

builds:
  - id: roborev
    main: ./cmd/roborev
    binary: roborev
    env:
      - CGO_ENABLED=0
    flags:
      - -trimpath
    ldflags:
      - -s -w -X go.kenn.io/roborev/internal/version.Version=v{{ .Version }}
    goos:
      - linux
      - darwin
      - windows
    goarch:
      - amd64
      - arm64

archives:
  - id: roborev
    ids:
      - roborev
    formats:
      - tar.gz
    name_template: >-
      {{ .ProjectName }}_
      {{- .Version }}_
      {{- .Os }}_
      {{- .Arch }}

checksum:
  name_template: SHA256SUMS

nfpms:
  - id: roborev-linux-packages
    ids:
      - roborev
    package_name: roborev
    file_name_template: "{{ .ProjectName }}_{{ .Version }}_linux_{{ .Arch }}"
    vendor: roborev
    homepage: https://github.com/roborev-dev/roborev
    maintainer: roborev contributors
    description: Continuous code review for AI coding agents.
    license: MIT
    formats:
      - deb
      - rpm
    bindir: /usr/bin
    contents:
      - src: packaging/systemd/roborev.service
        dst: /usr/lib/systemd/user/roborev.service
      - src: packaging/systemd/roborev.socket
        dst: /usr/lib/systemd/user/roborev.socket
```

Keep this `.gitignore` addition:

```gitignore
# roborev snapshots
/.roborev/
```

- [ ] **Step 3: Validate GoReleaser config**

Run:

```bash
nix run 'nixpkgs#goreleaser' -- check
```

Expected: PASS with no warnings.

- [ ] **Step 4: Commit**

```bash
git add .goreleaser.yaml .gitignore docs/superpowers/plans/2026-06-02-goreleaser-linux-packages.md
git commit -m "build: add goreleaser package config"
```

### Task 2: Migrate Release Workflow To GoReleaser

**Files:**
- Modify: `.github/workflows/release.yml`

- [ ] **Step 1: Validate current workflow before editing**

Run:

```bash
nix run 'nixpkgs#actionlint' -- .github/workflows/release.yml
```

Expected: PASS for the current workflow.

- [ ] **Step 2: Replace manual build/release jobs**

Edit `.github/workflows/release.yml` so the `release` job uses GoReleaser:

```yaml
name: Release

on:
  push:
    tags:
      - 'v*'

permissions:
  contents: write

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6.0.2
        with:
          fetch-depth: 0

      - uses: actions/setup-go@4a3601121dd01d1626a1e23e37211e3254c1c06c  # v6.4.0
        with:
          go-version: '1.26.3'

      - name: Resolve release notes
        id: release_notes
        run: |
          TAG_NAME="${GITHUB_REF#refs/tags/}"
          ARGS="release --clean"

          TAG_TYPE=$(git cat-file -t "$TAG_NAME")
          if [ "$TAG_TYPE" = "tag" ]; then
            TAG_MSG=$(git tag -l --format='%(contents:body)' "$TAG_NAME")
            if [ -n "$TAG_MSG" ]; then
              printf '%s\n' "$TAG_MSG" > release-notes.md
              ARGS="$ARGS --release-notes=release-notes.md"
            fi
          else
            echo "Warning: $TAG_NAME is a lightweight tag, using GoReleaser-generated notes"
          fi

          echo "args=$ARGS" >> "$GITHUB_OUTPUT"

      - name: Run GoReleaser
        uses: goreleaser/goreleaser-action@5daf1e915a5f0af01ddbcd89a43b8061ff4f1a89  # v7.2.2
        with:
          distribution: goreleaser
          version: '~> v2'
          args: ${{ steps.release_notes.outputs.args }}
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}

      - uses: actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a  # v7.0.1
        with:
          name: roborev-release-tarballs
          path: dist/*.tar.gz
```

Keep the existing `update-homebrew` job after this release job. Preserve
its artifact download, formula update, verification, and push behavior.
Quote the checksum script's file paths and `$GITHUB_OUTPUT` writes so
the edited workflow passes actionlint.

- [ ] **Step 3: Validate edited workflow**

Run:

```bash
nix run 'nixpkgs#actionlint' -- .github/workflows/release.yml
```

Expected: PASS with no warnings.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: release with goreleaser"
```

### Task 3: Verify Release Artifacts Locally

**Files:**
- Modify only if verification exposes a config or workflow defect.

- [ ] **Step 1: Run a snapshot release**

Run:

```bash
nix run 'nixpkgs#goreleaser' -- release --snapshot --clean --skip=publish
```

Expected: PASS. `dist/` contains six tarballs, Linux `.deb` packages,
Linux `.rpm` packages, and `SHA256SUMS`.

- [ ] **Step 2: Verify expected artifact names exist**

Run:

```bash
ls dist
```

Expected output includes filenames matching:

```text
roborev_*_darwin_amd64.tar.gz
roborev_*_darwin_arm64.tar.gz
roborev_*_linux_amd64.tar.gz
roborev_*_linux_arm64.tar.gz
roborev_*_windows_amd64.tar.gz
roborev_*_windows_arm64.tar.gz
roborev_*_linux_amd64.deb
roborev_*_linux_arm64.deb
roborev_*_linux_amd64.rpm
roborev_*_linux_arm64.rpm
SHA256SUMS
```

- [ ] **Step 3: Verify Linux archive contents**

Run:

```bash
tar -tzf dist/roborev_*_linux_amd64.tar.gz
```

Expected: output contains `roborev`.

- [ ] **Step 4: Verify package contents**

Run:

```bash
nix shell 'nixpkgs#dpkg' --command dpkg-deb --contents dist/roborev_*_linux_amd64.deb
```

Expected: output contains:

```text
/usr/bin/roborev
/usr/lib/systemd/user/roborev.service
/usr/lib/systemd/user/roborev.socket
```

Run:

```bash
nix shell 'nixpkgs#rpm' --command rpm -qpl dist/roborev_*_linux_amd64.rpm
```

Expected: output contains:

```text
/usr/bin/roborev
/usr/lib/systemd/user/roborev.service
/usr/lib/systemd/user/roborev.socket
```

- [ ] **Step 5: Re-run GoReleaser config validation**

Run:

```bash
nix run 'nixpkgs#goreleaser' -- check
```

Expected: PASS with no warnings.

### Task 4: Run Repo Quality Gates

**Files:**
- Modify only if checks expose a defect.

- [ ] **Step 1: Build all packages**

Run:

```bash
go build ./...
```

Expected: PASS.

- [ ] **Step 2: Run unit tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Run vet**

Run:

```bash
go vet ./...
```

Expected: PASS.

- [ ] **Step 4: Inspect final diff**

Run:

```bash
git diff --stat upstream/main...HEAD
git status --short
```

Expected: only intentional committed changes remain, with no unstaged files except ignored `dist/` output.
