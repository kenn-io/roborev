package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"go.kenn.io/kit/selfupdate"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/version"
)

const (
	releaseOwner  = "roborev-dev"
	releaseRepo   = "roborev"
	binaryName    = "roborev"
	cacheFileName = "update_check.json"
	cacheDuration = time.Hour
)

type UpdateInfo = selfupdate.Info

type Reporter interface {
	Stepf(format string, args ...any)
	Progress(downloaded, total int64)
}

type Deps struct {
	Client           *http.Client
	Now              func() time.Time
	Version          string
	GOOS             string
	GOARCH           string
	CacheDir         func() string
	Executable       func() (string, error)
	GitHubAPIBaseURL string
}

type Updater struct {
	deps Deps
}

type stdoutReporter struct {
	out        io.Writer
	progressFn func(downloaded, total int64)
}

type nopReporter struct{}

func CheckForUpdate(forceCheck bool) (*UpdateInfo, error) {
	return defaultUpdater().CheckForUpdate(forceCheck)
}

func PerformUpdate(info *UpdateInfo, progressFn func(downloaded, total int64)) error {
	return defaultUpdater().PerformUpdate(info, stdoutReporter{
		out:        os.Stdout,
		progressFn: progressFn,
	})
}

func RestartDaemon() error {
	return nil
}

func GetCacheDir() string {
	return config.DataDir()
}

func NewUpdater(deps Deps) *Updater {
	if deps.Client == nil {
		deps.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Version == "" {
		deps.Version = version.Version
	}
	if deps.GOOS == "" {
		deps.GOOS = runtime.GOOS
	}
	if deps.GOARCH == "" {
		deps.GOARCH = runtime.GOARCH
	}
	if deps.CacheDir == nil {
		deps.CacheDir = config.DataDir
	}
	if deps.Executable == nil {
		deps.Executable = os.Executable
	}
	return &Updater{deps: deps}
}

func defaultUpdater() *Updater {
	return NewUpdater(Deps{})
}

func (u *Updater) CheckForUpdate(forceCheck bool) (*UpdateInfo, error) {
	if selfupdate.IsDevBuildVersion(u.deps.Version) && !forceCheck {
		return nil, nil
	}
	return u.client().Check(context.Background(), selfupdate.CheckOptions{
		Force:  forceCheck,
		GOOS:   u.deps.GOOS,
		GOARCH: u.deps.GOARCH,
	})
}

func (u *Updater) PerformUpdate(info *UpdateInfo, reporter Reporter) error {
	reporter = normalizeReporter(reporter)
	if info == nil {
		return fmt.Errorf("update info is nil")
	}
	if info.Checksum == "" {
		return fmt.Errorf("no checksum available for %s - refusing to install unverified binary", info.AssetName)
	}

	installDir, err := u.installDir()
	if err != nil {
		return err
	}
	targetBinary := executableName(u.deps.GOOS)
	dstPath := filepath.Join(installDir, targetBinary)

	reporter.Stepf("Downloading %s...\n", info.AssetName)
	if err := u.client().Install(context.Background(), info, selfupdate.InstallOptions{
		DestinationPath:   dstPath,
		ArchiveBinaryName: targetBinary,
		Progress:          reporter.Progress,
	}); err != nil {
		return err
	}
	reporter.Stepf("Installing %s... OK\n", targetBinary)
	return nil
}

func (u *Updater) client() selfupdate.Client {
	return selfupdate.Client{
		Owner:                  releaseOwner,
		Repo:                   releaseRepo,
		BinaryName:             binaryName,
		CurrentVersion:         u.deps.Version,
		CacheDir:               u.deps.CacheDir(),
		HTTPClient:             u.deps.Client,
		Clock:                  u.deps.Now,
		GitHubAPIBaseURL:       u.deps.GitHubAPIBaseURL,
		UserAgent:              "roborev/" + u.deps.Version,
		CacheFileName:          cacheFileName,
		CacheDuration:          cacheDuration,
		AllowUnsignedChecksums: true,
	}
}

func (u *Updater) installDir() (string, error) {
	currentExe, err := u.deps.Executable()
	if err != nil {
		return "", fmt.Errorf("find current executable: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}
	return filepath.Dir(currentExe), nil
}

func executableName(goos string) string {
	if goos == "windows" {
		return binaryName + ".exe"
	}
	return binaryName
}

func FormatSize(bytes int64) string {
	return selfupdate.FormatSize(bytes)
}

func normalizeReporter(reporter Reporter) Reporter {
	if reporter == nil {
		return nopReporter{}
	}
	return reporter
}

func (r stdoutReporter) Stepf(format string, args ...any) {
	if r.out == nil {
		return
	}
	fmt.Fprintf(r.out, format, args...)
}

func (r stdoutReporter) Progress(downloaded, total int64) {
	if r.progressFn != nil {
		r.progressFn(downloaded, total)
	}
}

func (nopReporter) Stepf(string, ...any) {}

func (nopReporter) Progress(int64, int64) {}
