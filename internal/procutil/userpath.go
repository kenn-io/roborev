package procutil

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// UserToolDirs lists the directories where common installers, version
// managers, and package managers put a user's commands. A daemon inherits the
// PATH of whatever started it: a service manager, a supervisor, or a hook.
// That PATH often lacks these directories, so agents installed through them
// would not be found.
func UserToolDirs(home string, getenv func(string) string) []string {
	if runtime.GOOS == "windows" || home == "" {
		return nil
	}
	dataHome := getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	miseData := getenv("MISE_DATA_DIR")
	if miseData == "" {
		miseData = filepath.Join(dataHome, "mise")
	}
	asdfData := getenv("ASDF_DATA_DIR")
	if asdfData == "" {
		asdfData = filepath.Join(home, ".asdf")
	}
	dirs := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(miseData, "shims"),
		filepath.Join(asdfData, "shims"),
		filepath.Join(home, ".nix-profile", "bin"),
	}
	if user := getenv("USER"); user != "" {
		dirs = append(dirs, filepath.Join("/etc/profiles/per-user", user, "bin"))
	}
	return append(dirs,
		"/nix/var/nix/profiles/default/bin",
		"/run/current-system/sw/bin",
		"/opt/homebrew/bin",
		"/home/linuxbrew/.linuxbrew/bin",
		"/usr/local/bin",
		filepath.Join(home, ".npm-global", "bin"),
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".volta", "bin"),
		filepath.Join(home, ".cargo", "bin"),
	)
}

// AppendPathDirs appends each directory in dirs that exists and is not already
// in pathList. Existing entries keep their order and precedence. It returns the
// new list and the directories it added.
func AppendPathDirs(pathList string, dirs []string) (string, []string) {
	entries := filepath.SplitList(pathList)
	var added []string
	for _, dir := range dirs {
		if slices.Contains(entries, dir) {
			continue
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		entries = append(entries, dir)
		added = append(added, dir)
	}
	return strings.Join(entries, string(os.PathListSeparator)), added
}
