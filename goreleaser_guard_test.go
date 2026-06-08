package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestGoreleaserShipsWindowsTarGzAndZip guards the dual-format Windows release.
//
// roborev <= 0.56 shipped its own self-updater that only ever looks for a
// windows tar.gz asset; roborev >= 0.57 uses the kit updater, which downloads
// the windows zip. If a release drops either format, that population can no
// longer self-update and is stranded on "no release asset found for
// windows/amd64". See issue #816.
func TestGoreleaserShipsWindowsTarGzAndZip(t *testing.T) {
	root := repoRootFromWorkingDir(t)
	data, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	require.NoError(t, err)

	var cfg struct {
		Archives []struct {
			Formats         []string `yaml:"formats"`
			FormatOverrides []struct {
				Goos    string   `yaml:"goos"`
				Formats []string `yaml:"formats"`
			} `yaml:"format_overrides"`
		} `yaml:"archives"`
	}
	require.NoError(t, yaml.Unmarshal(data, &cfg))
	require.NotEmpty(t, cfg.Archives, "expected at least one archives entry in .goreleaser.yaml")

	// A windows format_override replaces the archive's base formats for windows,
	// so resolve the effective windows formats the same way GoReleaser does.
	var windowsFormats []string
	for _, archive := range cfg.Archives {
		formats := archive.Formats
		for _, override := range archive.FormatOverrides {
			if override.Goos == "windows" {
				formats = override.Formats
			}
		}
		windowsFormats = append(windowsFormats, formats...)
	}

	assert.Contains(t, windowsFormats, "tar.gz",
		"windows must ship tar.gz for roborev <= 0.56 self-updaters (issue #816)")
	assert.Contains(t, windowsFormats, "zip",
		"windows must ship zip for the roborev >= 0.57 kit self-updater")
}
