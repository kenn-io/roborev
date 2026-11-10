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
	"syscall"
)

// Metadata belongs to the exact executable, so a copy used as a native hook
// continues its normal hook dispatch without inheriting launcher identity.
type gitFixtureMetadata struct {
	RealGit     string
	Global      string
	Home        string
	UserProfile string
}

const gitFixtureMetadataSuffix = ".roborev-git-fixture.json"

func readGitFixtureMetadata(executable string) (gitFixtureMetadata, error) {
	var metadata gitFixtureMetadata
	data, err := os.ReadFile(executable + gitFixtureMetadataSuffix)
	if err != nil {
		return metadata, err
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return metadata, err
	}
	if !filepath.IsAbs(metadata.RealGit) || !filepath.IsAbs(metadata.Global) {
		return metadata, fmt.Errorf("fixture Git metadata requires absolute executable and config paths")
	}
	return metadata, nil
}

func init() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	metadata, err := readGitFixtureMetadata(exe)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "read fixture Git metadata: %v\n", err)
		syscall.Exit(1)
	}
	// This launcher is a helper process with no caller context.
	// Only recognized launchers bypass test/race runtime finalization.
	syscall.Exit(forwardFixtureGit(context.Background(), metadata))
}

func forwardFixtureGit(ctx context.Context, metadata gitFixtureMetadata) int {
	if _, explicit := os.LookupEnv("GIT_CONFIG_GLOBAL"); !explicit &&
		os.Getenv("HOME") == metadata.Home &&
		os.Getenv("USERPROFILE") == metadata.UserProfile {
		_ = os.Setenv("GIT_CONFIG_GLOBAL", metadata.Global)
	}
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return runFixtureGit(ctx, metadata.RealGit)
}

func installGitLauncher(tmpDir, global string, set func(string, string) error) error {
	realGit, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	realGit, err = filepath.Abs(realGit)
	if err != nil {
		return err
	}
	// Nested setup unwraps the current fixture launcher before exposing a new one.
	previous, err := readGitFixtureMetadata(realGit)
	if err == nil {
		realGit = previous.RealGit
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(exe)
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
	metadata := gitFixtureMetadata{
		RealGit:     realGit,
		Global:      global,
		Home:        os.Getenv("HOME"),
		UserProfile: os.Getenv("USERPROFILE"),
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	if err := os.WriteFile(launcher+gitFixtureMetadataSuffix, encoded, 0o600); err != nil {
		return err
	}
	return set("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
