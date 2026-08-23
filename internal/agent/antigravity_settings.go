package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// Official agy settings path. There is no documented --settings flag or env
// override that headless print-mode honors; permissions are read from
// ~/.gemini/antigravity-cli/settings.json.
// See https://antigravity.google/docs/cli/permissions/
func defaultAntigravitySettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

// antigravitySettingsPathForTest, when set, redirects settings writes away
// from the developer's real ~/.gemini tree (agent tests do not isolate HOME).
var antigravitySettingsPathForTest func() string

func antigravitySettingsPath() string {
	if antigravitySettingsPathForTest != nil {
		return antigravitySettingsPathForTest()
	}
	return defaultAntigravitySettingsPath()
}

// Inspect commands reviews run (pwd/wc/ls/...) must be allowlisted. In
// headless print mode, unconfigured command() actions default to Ask and
// are soft-denied or hard-fail with "permission check failed for command".
var antigravityReviewAllowPermissions = []string{
	"read_file(*)",
	"command(pwd)",
	"command(wc)",
	"command(ls)",
	"command(cat)",
	"command(head)",
	"command(tail)",
	"command(stat)",
	"command(file)",
}

var antigravitySettingsGate = make(chan struct{}, 1)

const antigravitySettingsLockRetryDelay = 10 * time.Millisecond

// ensureAntigravityReviewPermissions merges the allow-rules non-agentic
// reviews need into settings.json. Existing keys and allow entries are
// preserved; only missing allow strings are appended. Invalid JSON is
// left untouched.
func ensureAntigravityReviewPermissions(ctx context.Context, settingsPath string) (err error) {
	if settingsPath == "" {
		return nil
	}

	select {
	case antigravitySettingsGate <- struct{}{}:
		defer func() { <-antigravitySettingsGate }()
	case <-ctx.Done():
		return fmt.Errorf("acquire settings lock: %w", ctx.Err())
	}

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return fmt.Errorf("prepare %s: %w", settingsPath, err)
	}
	lockPath := settingsPath + ".lock"
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	locked, err := lock.TryLockContext(ctx, antigravitySettingsLockRetryDelay)
	if err != nil {
		_ = lock.Close()
		return fmt.Errorf("lock %s: %w", lockPath, err)
	}
	if !locked {
		_ = lock.Close()
		return fmt.Errorf("lock %s: %w", lockPath, ctx.Err())
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("unlock %s: %w", lockPath, unlockErr))
		}
		if closeErr := lock.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", lockPath, closeErr))
		}
	}()

	doc := map[string]any{}
	raw, err := os.ReadFile(settingsPath)
	switch {
	case err == nil:
		if trimmed := trimSpaceBytes(raw); len(trimmed) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if err := decoder.Decode(&doc); err != nil {
				return fmt.Errorf("parse %s: %w", settingsPath, err)
			}
			if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
				if err == nil {
					err = errors.New("multiple JSON values")
				}
				return fmt.Errorf("parse %s: %w", settingsPath, err)
			}
			if doc == nil {
				doc = map[string]any{}
			}
		}
	case os.IsNotExist(err):
		// create below
	default:
		return fmt.Errorf("read %s: %w", settingsPath, err)
	}

	permissions, err := settingsObject(doc, "permissions")
	if err != nil {
		return fmt.Errorf("%s: %w", settingsPath, err)
	}
	doc["permissions"] = permissions

	allow, changed, err := mergeAllowList(permissions["allow"], antigravityReviewAllowPermissions)
	if err != nil {
		return fmt.Errorf("%s permissions.allow: %w", settingsPath, err)
	}
	if !changed && fileExists(settingsPath) {
		return nil
	}
	permissions["allow"] = allow

	if err := writeSettingsJSON(settingsPath, doc); err != nil {
		return fmt.Errorf("write %s: %w", settingsPath, err)
	}
	return nil
}

func ensureAntigravityReviewSettings(ctx context.Context) error {
	path := antigravitySettingsPath()
	if path == "" {
		log.Printf("antigravity: skipping settings merge; cannot resolve home directory")
		return nil
	}
	if err := ensureAntigravityReviewPermissions(ctx, path); err != nil {
		log.Printf("antigravity: could not merge review permissions into %s: %v", path, err)
		return err
	}
	return nil
}

func settingsObject(doc map[string]any, key string) (map[string]any, error) {
	raw, ok := doc[key]
	if !ok || raw == nil {
		return map[string]any{}, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a JSON object", key)
	}
	return obj, nil
}

func mergeAllowList(existing any, needed []string) (allow []any, changed bool, err error) {
	switch v := existing.(type) {
	case nil:
		allow = nil
	case []any:
		allow = append([]any(nil), v...)
	default:
		return nil, false, fmt.Errorf("not a JSON array")
	}

	have := make(map[string]struct{}, len(allow))
	for _, item := range allow {
		s, ok := item.(string)
		if !ok {
			continue
		}
		have[s] = struct{}{}
	}
	for _, rule := range needed {
		if _, ok := have[rule]; ok {
			continue
		}
		allow = append(allow, rule)
		have[rule] = struct{}{}
		changed = true
	}
	if existing == nil {
		changed = true
	}
	return allow, changed, nil
}

func writeSettingsJSON(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	target, mode, err := settingsWriteTarget(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func settingsWriteTarget(path string) (string, os.FileMode, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return path, 0o644, nil
	}
	if err != nil {
		return "", 0, err
	}
	target := path
	if info.Mode()&os.ModeSymlink != 0 {
		target, err = filepath.EvalSymlinks(path)
		if err != nil {
			return "", 0, fmt.Errorf("resolve %s: %w", path, err)
		}
	}
	info, err = os.Stat(target)
	if os.IsNotExist(err) {
		return target, 0o644, nil
	}
	if err != nil {
		return "", 0, err
	}
	return target, info.Mode().Perm(), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func trimSpaceBytes(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}
