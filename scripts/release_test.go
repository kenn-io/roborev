package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReleaseUpdatesOnlyPackageVersionBeforeNixBuild(t *testing.T) {
	repo := t.TempDir()
	scriptsDir := filepath.Join(repo, "scripts")
	fakeBin := filepath.Join(repo, "fake-bin")
	remote := filepath.Join(repo, "origin.git")
	require.NoError(t, os.MkdirAll(scriptsDir, 0o755))
	require.NoError(t, os.MkdirAll(fakeBin, 0o755))

	installReleaseFixture(t, scriptsDir)
	writeExecutable(t, filepath.Join(fakeBin, "nix"), `#!/usr/bin/env bash
set -e

flake="$PWD/flake.nix"
fake_hash="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
expected_hash="sha256-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="

if ! grep -q 'version = "1.27.0";' "$flake"; then
    echo "Go toolchain version changed unexpectedly" >&2
    exit 1
fi

if grep -q "$fake_hash" "$flake"; then
    echo "specified: $fake_hash" >&2
    echo "got: $expected_hash" >&2
    exit 1
fi

grep -q "$expected_hash" "$flake"
`)
	writeExecutable(t, filepath.Join(fakeBin, "gh"), `#!/usr/bin/env bash
if [[ "$1 $2" == "pr list" ]]; then
    exit 0
fi
echo "https://example.invalid/pull/1"
`)

	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.name", "Release Test")
	git(t, repo, "config", "user.email", "release-test@example.invalid")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "release fixture")
	git(t, repo, "tag", "v0.64.0")
	git(t, repo, "init", "--bare", remote)
	git(t, repo, "remote", "add", "origin", remote)

	cmd := exec.Command("bash", bashPath(t, filepath.Join(scriptsDir, "release.sh")), "0.65.0")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader("n\n")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	updatedFlake := gitOutput(t, repo, "show", "release/v0.65.0-nix-update:flake.nix")
	assert := assert.New(t)
	assert.Contains(string(output), "Release cancelled.")
	assert.Contains(updatedFlake, `version = "1.27.0";`)
	assert.Contains(updatedFlake, `version = "0.65.0";`)
	assert.Contains(updatedFlake, `vendorHash = "sha256-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=";`)
}

func installReleaseFixture(t *testing.T, scriptsDir string) {
	t.Helper()

	for _, name := range []string{"release.sh", "update-nix-vendor-hash.sh"} {
		script := readShellScript(t, name)
		require.NoError(t, os.WriteFile(filepath.Join(scriptsDir, name), script, 0o755))
	}

	writeExecutable(t, filepath.Join(scriptsDir, "changelog.sh"), `#!/usr/bin/env bash
echo "Synthetic changelog"
`)

	flake := `{
  outputs = { self }: {
    goPinned = {
      version = "1.27.0";
      url = "https://go.dev/dl/go${version}.src.tar.gz";
    };
    packages.default = {
      pname = "roborev";
      version = "0.64.0";
      vendorHash = "sha256-CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC=";
    };
  };
}
`
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(scriptsDir), "flake.nix"), []byte(flake), 0o644))
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o755))
}
