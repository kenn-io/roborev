// Package sandbox confines agent commands with a pinned nono policy.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"go.kenn.io/kit/pathresolve"
)

// Job describes the caller's filesystem, environment, network, and memory grants.
type Job struct {
	WorkDir   string
	ReadPaths []string
	Route     Route
	Env       []string
	MemoryMax int64
}

// Route lets nono capture a host credential and inject it into matching requests.
type Route struct {
	Name, Upstream, Path, EnvVar string
	Capture                      []string
}

// Command verifies nono and the job policy before returning a wrapped command.
// The caller must call cleanup after the command finishes.
func (j Job) Command(ctx context.Context, argv []string) (cmd, env []string, cleanup func(), err error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, nil, nil, fmt.Errorf("sandbox requires linux/amd64")
	}
	if j.MemoryMax <= 0 {
		return nil, nil, nil, fmt.Errorf("sandbox MemoryMax must be positive")
	}
	workDir, err := filepath.Abs(j.WorkDir)
	if err == nil {
		workDir, err = pathresolve.EvalSymlinks(workDir)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve sandbox WorkDir: %w", err)
	}
	j.WorkDir = workDir
	tempDir, err := filepath.Abs(os.TempDir())
	if err == nil {
		tempDir, err = pathresolve.EvalSymlinks(tempDir)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve sandbox temp dir: %w", err)
	}
	readPaths := make([]string, 0, len(j.ReadPaths))
	for _, path := range j.ReadPaths {
		resolved, err := filepath.Abs(path)
		if err == nil {
			resolved, err = pathresolve.EvalSymlinks(resolved)
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolve sandbox read path %q: %w", path, err)
		}
		rel, err := filepath.Rel(workDir, resolved)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("compare sandbox read path %q with WorkDir: %w", path, err)
		}
		if rel == "." || filepath.IsLocal(rel) {
			return nil, nil, nil, fmt.Errorf("read path %q is inside sandbox WorkDir %q; read paths must be outside it", path, workDir)
		}
		rel, err = filepath.Rel(resolved, tempDir)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("compare sandbox read path %q with temp dir: %w", path, err)
		}
		if rel == "." || filepath.IsLocal(rel) {
			return nil, nil, nil, fmt.Errorf("read path %q contains sandbox temp dir %q", path, tempDir)
		}
		readPaths = append(readPaths, resolved)
	}
	j.ReadPaths = readPaths
	nono, err := exec.LookPath("nono")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("find sandbox nono: %w", err)
	}
	nono, err = filepath.Abs(nono)
	if err == nil {
		nono, err = pathresolve.EvalSymlinks(nono)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve sandbox nono: %w", err)
	}
	f, err := os.Open(nono)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open sandbox nono: %w", err)
	}
	hash := sha256.New()
	_, err = io.Copy(hash, f)
	_ = f.Close()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("hash sandbox nono: %w", err)
	}
	const pinnedSHA256 = "e02d003d52b3f65e8c75358eb73e339ad952a2876529047e61183b46eff1287d"
	if fmt.Sprintf("%x", hash.Sum(nil)) != pinnedSHA256 {
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
		"groups":   map[string]any{"exclude": []string{"system_write_linux"}},
		"workdir":  map[string]any{"access": "none"},
		"linux":    map[string]any{"af_unix_mediation": "pathname"},
		"security": map[string]any{"signal_mode": "isolated", "process_info_mode": "isolated"},
		"filesystem": map[string]any{
			"read":       []string{"/usr/libexec", "/usr/include"},
			"write_file": []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/tty"},
			"write":      []string{"/dev/pts"},
		},
		"network": map[string]any{
			"credentials":  []string{j.Route.Name},
			"allow_domain": []any{map[string]any{"domain": upstream.Hostname(), "endpoints": []any{map[string]any{"method": "GET", "path": "/nono-no-direct-access"}}}},
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
		cmd = append(cmd, "--read", path)
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
	err = j.check(ctx, checkCmd, env)
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
	_ = exec.Command("systemctl", "--user", "kill", "--signal=KILL", "--kill-whom=all", unit).Run()
	// Wait for the unit to stop before the caller removes the job's files.
	_ = exec.Command("systemctl", "--user", "stop", unit).Run()
}
