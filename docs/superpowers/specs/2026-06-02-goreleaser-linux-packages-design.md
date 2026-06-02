# GoReleaser Linux Package Artifacts Design

## Context

Issue #610 asks for pre-built DEB and RPM release artifacts so Linux
users can install roborev through distro package managers. The issue
also mentions install-script package-manager defaults and disabling
`roborev update` for package-managed installs, but those parts are out
of scope for this pass.

The current release workflow builds tarballs manually for Linux, macOS,
and Windows, publishes those files to GitHub Releases, generates a
`SHA256SUMS` file, and updates the Homebrew tap in a separate job.
Upstream already includes user-level systemd units at
`packaging/systemd/roborev.service` and `packaging/systemd/roborev.socket`.

## Goals

- Move release artifact generation to GoReleaser.
- Preserve existing tarball platforms and filenames.
- Preserve the `SHA256SUMS` checksum filename used by updater code.
- Add Linux `.deb` and `.rpm` artifacts for `amd64` and `arm64`.
- Include the existing systemd user service and socket units in Linux
  packages.
- Keep install scripts and `roborev update` behavior unchanged.

## Design

Add a `.goreleaser.yaml` file as the release definition. It will build
`./cmd/roborev` for:

- `linux/amd64`
- `linux/arm64`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`
- `windows/arm64`

The Go build will use `CGO_ENABLED=0`, `-trimpath`, and release ldflags
matching the module path:

```text
-s -w -X go.kenn.io/roborev/internal/version.Version=v{{ .Version }}
```

Archives will keep the existing release filename shape:

```text
roborev_{{ .Version }}_{{ .Os }}_{{ .Arch }}.tar.gz
```

GoReleaser will generate `SHA256SUMS` and include all published archives
and Linux packages in it.

The nFPM section will package only Linux builds, producing both `deb`
and `rpm` formats. Package contents will include:

- `roborev` under `/usr/bin/roborev`
- `packaging/systemd/roborev.service` under
  `/usr/lib/systemd/user/roborev.service`
- `packaging/systemd/roborev.socket` under
  `/usr/lib/systemd/user/roborev.socket`

Package metadata will use the existing project name, homepage, MIT
license, and CLI description. The package will not publish to an APT or
YUM repository in this change.

## Workflow Changes

Replace the manual release build and GitHub release upload jobs in
`.github/workflows/release.yml` with a GoReleaser job:

- checkout with full history for tag/changelog handling
- set up the repo's Go version
- run `goreleaser/goreleaser-action@v7` with `version: '~> v2'`
- invoke `goreleaser release --clean`
- pass a token that can publish release assets
- upload generated Linux, macOS, and Windows tarballs as workflow
  artifacts for the existing Homebrew job

The existing Homebrew tap update job stays separate for this issue. It
will continue to consume workflow artifacts with the preserved tarball
filenames and compute the formula checksums the same way it does today.
This avoids changing the Homebrew install surface while adding DEB/RPM
release artifacts.

## Risks

- GoReleaser defaults differ from the current workflow, so the config
  must pin archive and checksum names explicitly.
- Package architecture names may differ between GoReleaser, Debian, and
  RPM conventions. The snapshot build must inspect generated filenames.
- The Homebrew job depends on exact artifact names, so the GoReleaser
  job must upload tarball workflow artifacts in the same shape as the old
  matrix build.

## Verification

- Run `goreleaser check`.
- Run a local snapshot release through GoReleaser and verify that
  archives, `.deb`, `.rpm`, and `SHA256SUMS` are generated.
- Run Go build/tests relevant to release config changes.
- Inspect the generated package contents for the binary and systemd user
  units.
