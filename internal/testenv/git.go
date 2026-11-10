package testenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"go.kenn.io/roborev/internal/testenv/gitfixture"
)

var gitLauncherImage struct {
	once sync.Once
	data []byte
	err  error
}

func buildGitLauncher(ctx context.Context) ([]byte, error) {
	dir, err := os.MkdirTemp("", "roborev-git-launcher-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	executable := filepath.Join(dir, "gitfixture")
	cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-race=false", "-cover=false", "-coverpkg=", "-o", executable,
		"go.kenn.io/roborev/internal/testenv/gitfixture/cmd/gitfixture")
	// Preserve the selected toolchain and GOFLAGS; disable CGO only for this helper.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build fixture Git launcher: %w\n%s", err, output)
	}
	return os.ReadFile(executable)
}

func installGitLauncher(ctx context.Context, tmpDir, global string, set func(string, string) error) error {
	realGit, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	realGit, err = filepath.Abs(realGit)
	if err != nil {
		return err
	}
	// Resolve aliases before reading either the executable image or its sidecar.
	realGit, err = filepath.EvalSymlinks(realGit)
	if err != nil {
		return err
	}
	var data []byte
	previous, err := gitfixture.ReadMetadata(realGit)
	if err == nil {
		// A fresh nested process can reuse this image without Go or a module CWD.
		data, err = os.ReadFile(realGit)
		realGit = previous.RealGit
	} else if errors.Is(err, os.ErrNotExist) {
		gitLauncherImage.once.Do(func() { gitLauncherImage.data, gitLauncherImage.err = buildGitLauncher(ctx) })
		data, err = gitLauncherImage.data, gitLauncherImage.err
	}
	if err != nil {
		return err
	}
	bin := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	name := "git"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	launcher := filepath.Join(bin, name)
	if err := os.WriteFile(launcher, data, 0o755); err != nil {
		return err
	}
	metadata := gitfixture.Metadata{
		RealGit:     realGit,
		Global:      global,
		Home:        os.Getenv("HOME"),
		UserProfile: os.Getenv("USERPROFILE"),
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	if err := os.WriteFile(launcher+gitfixture.MetadataSuffix, encoded, 0o600); err != nil {
		return err
	}
	return set("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
