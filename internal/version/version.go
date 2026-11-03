package version

import (
	"runtime/debug"
)

const (
	defaultVersion  = "dev"
	develVersion    = "(devel)"
	settingRevision = "vcs.revision"
	settingModified = "vcs.modified"
	settingVCSTime  = "vcs.time"
	shortHashLen    = 7
	dirtySuffix     = "-dirty"
)

var readBuildInfo = debug.ReadBuildInfo

// Version is the build version. Set via -ldflags for releases,
// otherwise falls back to git commit hash from VCS info.
// For dev builds with git describe version, use: make install
var Version = defaultVersion

// Distribution names the channel that delivered this binary. Package builds
// set it via -ldflags to their package manager, such as "homebrew", and do
// not replace themselves. Official direct downloads use "kenn" and, like an
// empty value for source builds, may replace themselves.
var Distribution string

func init() {
	// If Version was set via ldflags, use it
	if Version != defaultVersion {
		return
	}

	// Fall back to VCS info for development builds
	Version = getVersionFromVCS()
}

func getVersionFromVCS() string {
	info, ok := readBuildInfo()
	if !ok {
		return defaultVersion
	}

	// Use module version when installed via `go install pkg@version`
	if v := info.Main.Version; v != "" && v != develVersion {
		return v
	}

	revision := buildSetting(info, settingRevision)
	if revision == "" {
		return defaultVersion
	}

	// Use short hash
	if len(revision) > shortHashLen {
		revision = revision[:shortHashLen]
	}

	// Mark dirty builds
	if buildSetting(info, settingModified) == "true" {
		revision += dirtySuffix
	}

	return revision
}

// Full returns the full version string with additional build info
func Full() string {
	info, ok := readBuildInfo()
	if !ok {
		return Version
	}

	vcsTime := buildSetting(info, settingVCSTime)
	if vcsTime == "" {
		return Version
	}

	return Version + " " + vcsTime
}

func buildSetting(info *debug.BuildInfo, key string) string {
	for _, setting := range info.Settings {
		if setting.Key == key {
			return setting.Value
		}
	}

	return ""
}
