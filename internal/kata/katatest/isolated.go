package katatest

import (
	"path/filepath"
	"slices"
	"strings"
)

// isolatedAllowlist names the inherited variables a real-Kata test process
// may see. Everything else, including every KATA_* routing, token, and
// attribution variable, is dropped so the caller's Kata setup cannot leak in.
var isolatedAllowlist = []string{
	"PATH", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
	"SYSTEMROOT", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "WINDIR",
}

// IsolatedEnv builds the complete environment for a Kata subprocess from
// parent: the allowlisted variables, plus a disposable HOME, config and data
// directories under it, and KATA_HOME. Entries are matched case-insensitively
// so a spelling variant cannot slip past on a case-insensitive platform.
func IsolatedEnv(parent []string, home, kataHome string) []string {
	var out []string
	seen := map[string]bool{}
	for _, entry := range parent {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		upper := strings.ToUpper(key)
		if seen[upper] || !slices.Contains(isolatedAllowlist, upper) {
			continue
		}
		seen[upper] = true
		out = append(out, entry)
	}
	return append(out,
		"HOME="+home,
		"USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"KATA_HOME="+kataHome,
	)
}
