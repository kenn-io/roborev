package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.kenn.io/kit/selfupdate"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/version"
)

const (
	releaseOwner         = "roborev-dev"
	releaseRepo          = "roborev"
	binaryName           = "roborev"
	cacheFileName        = "update_check.json"
	cacheDuration        = time.Hour
	checksumsAssetName   = "SHA256SUMS"
	defaultGitHubBaseURL = "https://github.com"
	maxChecksumsBytes    = 1 << 20
	metadataTimeout      = 30 * time.Second
	downloadTimeout      = 30 * time.Minute
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
	GitHubBaseURL    string
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
		deps.Client = defaultHTTPClient()
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
	if deps.GitHubBaseURL == "" {
		deps.GitHubBaseURL = defaultGitHubBaseURL
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
	ctx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancel()
	info, err := u.client().Check(ctx, selfupdate.CheckOptions{
		Force:  forceCheck,
		GOOS:   u.deps.GOOS,
		GOARCH: u.deps.GOARCH,
	})
	if err == nil {
		return info, nil
	}
	// The GitHub API check is unauthenticated, and shared egress IPs (CI
	// runners, corporate NAT) routinely exhaust the per-IP rate limit with
	// a 403. Resolve the release through github.com instead, which is not
	// subject to API rate limits.
	fallbackCtx, fallbackCancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer fallbackCancel()
	info, fallbackErr := u.fallbackCheck(fallbackCtx)
	if fallbackErr != nil {
		return nil, fmt.Errorf("%w (github.com fallback also failed: %w)", err, fallbackErr)
	}
	return info, nil
}

// fallbackCheck resolves the latest release without the GitHub API: the
// /releases/latest web URL redirects to the tag, and SHA256SUMS provides the
// asset checksum. Asset names follow the kit release naming convention.
func (u *Updater) fallbackCheck(ctx context.Context) (*UpdateInfo, error) {
	tag, err := u.latestReleaseTag(ctx)
	if err != nil {
		return nil, err
	}

	current := strings.TrimPrefix(u.deps.Version, "v")
	latest := strings.TrimPrefix(tag, "v")
	isDev := selfupdate.IsDevBuildVersion(current)
	// Mirrors kit's offer rule: release builds update only to newer
	// versions; dev builds without a parseable base version always see the
	// latest release.
	offer := selfupdate.IsNewer(latest, current)
	if !offer && isDev && semverBase(current) == "" {
		offer = true
	}
	if !offer {
		return nil, nil
	}

	extension := ".tar.gz"
	if u.deps.GOOS == "windows" {
		extension = ".zip"
	}
	assetName := selfupdate.DefaultAssetName(selfupdate.AssetRequest{
		BinaryName: binaryName,
		Version:    latest,
		GOOS:       u.deps.GOOS,
		GOARCH:     u.deps.GOARCH,
		Extension:  extension,
	})

	downloadBase := fmt.Sprintf("%s/%s/%s/releases/download/%s",
		u.deps.GitHubBaseURL, releaseOwner, releaseRepo, tag)
	checksum, err := u.fetchChecksum(ctx, downloadBase+"/"+checksumsAssetName, assetName)
	if err != nil {
		return nil, err
	}
	downloadURL := downloadBase + "/" + assetName

	return &UpdateInfo{
		Owner:          releaseOwner,
		Repo:           releaseRepo,
		CurrentVersion: u.deps.Version,
		LatestVersion:  tag,
		DownloadURL:    downloadURL,
		AssetName:      assetName,
		GOOS:           u.deps.GOOS,
		GOARCH:         u.deps.GOARCH,
		Size:           u.assetSize(ctx, downloadURL),
		Checksum:       checksum,
		IsDevBuild:     isDev,
	}, nil
}

// latestReleaseTag follows the github.com /releases/latest redirect chain
// (including repo renames) and extracts the tag from the final URL.
func (u *Updater) latestReleaseTag(ctx context.Context) (string, error) {
	pageURL := fmt.Sprintf("%s/%s/%s/releases/latest",
		u.deps.GitHubBaseURL, releaseOwner, releaseRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "roborev/"+u.deps.Version)

	// Response.Request is only populated by http.Transport, so track the
	// final URL through the client's redirect hook instead.
	finalURL := req.URL
	client := *u.deps.Client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		finalURL = req.URL
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest release page: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release page returned %s", resp.Status)
	}
	const marker = "/releases/tag/"
	idx := strings.Index(finalURL.Path, marker)
	if idx < 0 {
		return "", fmt.Errorf("latest release did not redirect to a tag (got %s)", finalURL)
	}
	tag := finalURL.Path[idx+len(marker):]
	if tag == "" {
		return "", fmt.Errorf("empty release tag in %s", finalURL)
	}
	return tag, nil
}

func (u *Updater) fetchChecksum(ctx context.Context, checksumsURL, assetName string) (string, error) {
	resp, err := u.get(ctx, checksumsURL)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", checksumsAssetName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: %s", checksumsAssetName, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumsBytes))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", checksumsAssetName, err)
	}
	checksum := selfupdate.ExtractChecksum(string(body), assetName)
	if checksum == "" {
		return "", fmt.Errorf("no checksum for %s in %s", assetName, checksumsURL)
	}
	return checksum, nil
}

// assetSize fetches the asset Content-Length for progress display. Size is
// informational; checksum verification covers integrity, so failures
// degrade to an unknown size.
func (u *Updater) assetSize(ctx context.Context, downloadURL string) int64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, downloadURL, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", "roborev/"+u.deps.Version)
	resp, err := u.deps.Client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength < 0 {
		return 0
	}
	return resp.ContentLength
}

func (u *Updater) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "roborev/"+u.deps.Version)
	return u.deps.Client.Do(req)
}

// semverBase mirrors kit's unexported base-version extraction used by its
// update-offer rule.
func semverBase(v string) string {
	v = strings.TrimPrefix(v, "v")
	if len(v) == 0 || v[0] < '0' || v[0] > '9' || !strings.Contains(v, ".") {
		return ""
	}
	if idx := strings.Index(v, "-"); idx > 0 {
		v = v[:idx]
	}
	return v
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
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	if err := u.client().Install(ctx, info, selfupdate.InstallOptions{
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
		GitHubWebBaseURL:       u.deps.GitHubBaseURL,
		GitHubToken:            selfupdate.EnvironmentGitHubToken(),
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

func defaultHTTPClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{}
	}
	cloned := transport.Clone()
	cloned.ResponseHeaderTimeout = metadataTimeout
	return &http.Client{Transport: cloned}
}

func (nopReporter) Stepf(string, ...any) {}

func (nopReporter) Progress(int64, int64) {}
