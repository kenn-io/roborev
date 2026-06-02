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
