// Package sandbox confines agent commands with a pinned nono policy.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Job describes the caller's filesystem, environment, network, and memory grants.
type Job struct {
	WorkDir   string
	ReadPaths []string
	ProxyPort int
	Env       []string
	MemoryMax int64
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
	nono, err := exec.LookPath("nono")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("find sandbox nono: %w", err)
	}
	nono, err = filepath.EvalSymlinks(nono)
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
	// nono adds embedded default groups without resolving a named user profile.
	profile := map[string]any{
		"groups":   map[string]any{"exclude": []string{"system_write_linux"}},
		"workdir":  map[string]any{"access": "none"},
		"linux":    map[string]any{"af_unix_mediation": "pathname"},
		"security": map[string]any{"signal_mode": "isolated", "process_info_mode": "isolated"},
		"filesystem": map[string]any{
			"allow":      []string{j.WorkDir},
			"read":       append(append([]string{}, j.ReadPaths...), "/usr/libexec", "/usr/include"),
			"write_file": []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/tty"},
			"write":      []string{"/dev/pts"},
		},
		"network": map[string]any{"allow_domain": []string{"proxy-mode.invalid"}, "open_port": []int{j.ProxyPort}},
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
	file, err := os.CreateTemp("", "roborev-sandbox-*.json")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create sandbox profile: %w", err)
	}
	unit := filepath.Base(file.Name()) + ".scope"
	cleanup = func() {
		killScope(unit)
		_ = os.Remove(file.Name())
	}
	if err = json.MarshalWrite(file, profile); err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, nil, fmt.Errorf("write sandbox profile: %w", err)
	}
	if err = file.Close(); err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("close sandbox profile: %w", err)
	}
	cmd = []string{"systemd-run", "--user", "--scope", "-q", "-p", "MemoryMax=" + strconv.FormatInt(j.MemoryMax, 10), "-p", "MemorySwapMax=0", "--", nono, "run", "-s", "--profile", file.Name(), "--no-rollback", "--no-audit", "--"}
	env = append([]string{}, j.Env...)
	for _, key := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	checkUnit := filepath.Base(file.Name()) + "-check.scope"
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
