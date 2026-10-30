// Package sandbox confines agent commands with a pinned nono policy.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"go.kenn.io/kit/pathresolve"
)

// Job describes the caller's filesystem, environment, network, and memory grants.
// Env must give nono a job-private, disposable HOME outside WorkDir for its session state.
type Job struct {
	WorkDir   string
	ReadPaths []string
	// DenyReadPaths names files that must be unreadable, including through symlinks.
	DenyReadPaths []string
	Route         Route
	Env           []string
	MemoryMax     int64
}

// Route lets nono capture a host credential and inject it into matching requests.
type Route struct {
	Name, Upstream, Path, EnvVar string
	Capture                      []string
}

// Command verifies nono and the job policy before returning a wrapped command.
// The caller must call cleanup after the command finishes.
// On cancellation, call cleanup instead of killing the wrapped process directly.
// The wrapped command starts in a private directory, so callers pass an absolute working directory, as Codex does with -C.
func (j Job) Command(ctx context.Context, argv []string) (cmd, env []string, cleanup func(), err error) {
	workDir, err := ResolvePath(j.WorkDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve sandbox WorkDir: %w", err)
	}
	j.WorkDir = workDir
	readPaths := make([]string, 0, len(j.ReadPaths))
	for _, path := range j.ReadPaths {
		resolved, err := ResolvePath(path)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolve sandbox read path %q: %w", path, err)
		}
		rel, err := filepath.Rel(workDir, resolved)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("compare sandbox read path %q with WorkDir: %w", path, err)
		}
		if filepath.IsLocal(rel) {
			return nil, nil, nil, fmt.Errorf("read path %q is inside sandbox WorkDir %q; read paths must be outside it", path, workDir)
		}
		readPaths = append(readPaths, resolved)
	}
	j.ReadPaths = readPaths
	nono, err := exec.LookPath("nono")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("find sandbox nono: %w", err)
	}
	binary, err := os.ReadFile(nono)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read sandbox nono: %w", err)
	}
	const pinnedSHA256 = "e02d003d52b3f65e8c75358eb73e339ad952a2876529047e61183b46eff1287d"
	if fmt.Sprintf("%x", sha256.Sum256(binary)) != pinnedSHA256 {
		return nil, nil, nil, fmt.Errorf("sandbox nono checksum mismatch: requires pinned nono 0.80.0 linux/amd64")
	}

	keys := make([]string, 0, len(j.Env))
	for _, entry := range j.Env {
		key, _, _ := strings.Cut(entry, "=")
		keys = append(keys, key)
	}
	upstream, err := url.Parse(j.Route.Upstream)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse sandbox route upstream: %w", err)
	}
	// nono adds embedded default groups without resolving a named user profile.
	profile := map[string]any{
		"groups":   map[string]any{"exclude": []string{"system_write_linux", "system_read_linux_core"}},
		"workdir":  map[string]any{"access": "none"},
		"linux":    map[string]any{"af_unix_mediation": "pathname"},
		"security": map[string]any{"signal_mode": "isolated", "process_info_mode": "isolated"},
		"filesystem": map[string]any{
			// nono 0.80.0's core read group includes host terminals. Keep its other grants.
			"read": []string{
				"/bin", "/sbin", "/usr/bin", "/usr/sbin", "/usr/local/bin", "/lib", "/lib64",
				"/usr/lib", "/usr/lib64", "/usr/local/lib", "/usr/local/lib64", "/usr/libexec", "/usr/include", "/usr/share",
				"/etc/resolv.conf", "/run/systemd/resolve", "/etc/hosts", "/etc/nsswitch.conf", "/etc/gai.conf", "/etc/group",
				"/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/etc/crypto-policies", "/etc/fonts",
				"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d", "/etc/localtime", "/etc/timezone", "/etc/locale.conf", "/etc/locale.gen",
				"/etc/alternatives", "/etc/os-release", "/etc/passwd", "/etc/services", "/etc/mime.types", "/etc/manpath.config", "/etc/man_db.conf",
				"/etc/terminfo", "/var/cache/man", "/run/systemd/userdb",
				"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/full", "/dev/fd",
				"/proc/self", "/proc/cpuinfo", "/proc/meminfo", "/proc/stat", "/proc/loadavg", "/proc/version", "/proc/filesystems",
			},
			"write_file": []string{"/dev/null", "/dev/zero", "/dev/full"},
		},
		"network": map[string]any{
			"credentials": []string{j.Route.Name},
			// HTTP methods are nonempty, so this rule cannot authorize a request.
			"allow_domain": []any{map[string]any{"domain": upstream.Hostname(), "endpoints": []any{map[string]any{"method": "", "path": "/nono-no-direct-access"}}}},
			"custom_credentials": map[string]any{j.Route.Name: map[string]any{
				"upstream": j.Route.Upstream, "credential_key": "cmd://" + j.Route.Name,
				"env_var": j.Route.EnvVar, "credential_format": "Bearer {}",
				"endpoint_rules": []any{map[string]any{"method": "POST", "path": j.Route.Path}},
			}},
		},
		"credential_capture": map[string]any{j.Route.Name: map[string]any{
			"command": j.Route.Capture, "cache_ttl_secs": 300,
		}},
		"environment": map[string]any{
			"allow_vars": keys,
			"set_vars": map[string]string{
				"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
				"GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "core.excludesFile",
				"GIT_CONFIG_VALUE_0": "/dev/null", "GIT_CONFIG_KEY_1": "core.attributesFile",
				"GIT_CONFIG_VALUE_1": "/dev/null",
			},
		},
	}
	dir, err := os.MkdirTemp("", "roborev-sandbox-*")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create sandbox profile: %w", err)
	}
	unit := filepath.Base(dir) + ".scope"
	cleanup = func() {
		killScope(unit)
		_ = os.RemoveAll(dir)
	}
	data, err := json.Marshal(profile)
	if err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("render sandbox profile: %w", err)
	}
	profilePath := filepath.Join(dir, "profile.json")
	if err = os.WriteFile(profilePath, data, 0o600); err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("write sandbox profile: %w", err)
	}
	cmd = []string{"systemd-run", "--working-directory=" + dir, "--user", "--scope", "-q", "-p", "MemoryMax=" + strconv.FormatInt(j.MemoryMax, 10), "-p", "MemorySwapMax=0", "--", nono, "run", "-s", "--profile", profilePath, "--no-rollback", "--no-audit", "--allow", j.WorkDir}
	// CLI grants are literal paths; profile paths undergo template expansion.
	for _, path := range j.ReadPaths {
		info, err := os.Stat(path)
		if err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("stat sandbox read path: %w", err)
		}
		flag := "--read-file"
		if info.IsDir() {
			flag = "--read"
		}
		cmd = append(cmd, flag, path)
	}
	cmd = append(cmd, "--")
	env = append([]string{}, j.Env...)
	for _, key := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	checkUnit := filepath.Base(dir) + "-check.scope"
	checkCmd := append([]string{cmd[0], "--unit=" + checkUnit}, cmd[1:]...)
	err = j.check(ctx, checkCmd, env, func() { killScope(checkUnit) })
	killScope(checkUnit)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	cmd = append([]string{cmd[0], "--unit=" + unit}, cmd[1:]...)
	return append(cmd, argv...), env, cleanup, nil
}

func killScope(unit string) {
	// Use the host environment and a fresh context: the job may be cancelled.
	// Stop sends SIGTERM so nono can remove its files, waits for the scope to
	// exit, and escalates to SIGKILL after systemd's stop timeout.
	_ = exec.Command("systemctl", "--user", "stop", unit).Run()
}

// ResolvePath rejects empty grants and resolves absolute paths through symlinks.
func ResolvePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	resolved, err := pathresolve.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}
