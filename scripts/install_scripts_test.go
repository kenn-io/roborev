package scripts

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsInstallersUseZipReleaseAssets(t *testing.T) {
	shellInstaller, err := os.ReadFile("install.sh")
	require.NoError(t, err)
	powerShellInstaller, err := os.ReadFile("install.ps1")
	require.NoError(t, err)

	shell := string(shellInstaller)
	powerShell := string(powerShellInstaller)

	assert.Contains(t, shell, `filename="roborev_${version#v}_${platform}.zip"`)
	assert.Contains(t, shell, `archive_path="$tmpdir/release.zip"`)
	assert.Contains(t, shell, `binary="roborev.exe"`)
	assert.Contains(t, powerShell, `$archiveName = "roborev_${versionNum}_windows_${arch}.zip"`)
	assert.Contains(t, powerShell, `Expand-Archive -LiteralPath $archivePath -DestinationPath $tmpDir -Force`)
}

func TestShellInstallerFailsReleasePathOnExtractionOrMissingBinary(t *testing.T) {
	shellInstaller, err := os.ReadFile("install.sh")
	require.NoError(t, err)

	shell := string(shellInstaller)

	assert.Contains(t, shell, `if ! unzip -q "$archive_path" -d "$tmpdir"; then`)
	assert.Contains(t, shell, `if ! tar -xzf "$archive_path" -C "$tmpdir"; then`)
	assert.Contains(t, shell, `if [ ! -f "$tmpdir/$binary" ]; then`)
}

func TestShellInstallerUsesModulePathForGoInstallFallback(t *testing.T) {
	shellInstaller, err := os.ReadFile("install.sh")
	require.NoError(t, err)

	shell := string(shellInstaller)

	assert.Contains(t, shell, `if ! go install "go.kenn.io/roborev/cmd/roborev@latest"; then`)
	assert.Contains(t, shell, `return 1`)
}
