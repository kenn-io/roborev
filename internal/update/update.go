package update

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/version"
)

const (
	// githubLatestReleaseURL is the HTML endpoint that 302-redirects to
	// /releases/tag/<tag>. Unlike api.github.com it is not rate-limited
	// at 60 req/hr per IP for unauthenticated callers.
	githubLatestReleaseURL    = "https://github.com/roborev-dev/roborev/releases/latest"
	githubReleaseDownloadBase = "https://github.com/roborev-dev/roborev/releases/download"
	cacheFileName             = "update_check.json"
	cacheDuration             = 1 * time.Hour
)

var (
	gitDescribePattern = regexp.MustCompile(`-\d+-g[0-9a-f]+(-dirty)?$`)
	checksumPattern    = regexp.MustCompile(`(?i)[a-f0-9]{64}`)
	semverBasePattern  = regexp.MustCompile(`^\d+(?:\.\d+)+`)
)

// UpdateInfo contains information about an available update
type UpdateInfo struct {
	CurrentVersion string
	LatestVersion  string
	DownloadURL    string
	AssetName      string
	Size           int64
	Checksum       string // SHA256 if available
	IsDevBuild     bool   // True if running a dev build (hash version)
}

// Reporter handles user-facing update progress.
type Reporter interface {
	Stepf(format string, args ...any)
	Progress(downloaded, total int64)
}

// Deps holds environment and process dependencies for updater operations.
type Deps struct {
	Client     *http.Client
	Now        func() time.Time
	Version    string
	GOOS       string
	GOARCH     string
	CacheDir   func() string
	Executable func() (string, error)
	MkdirTemp  func(dir, pattern string) (string, error)
}

// Updater coordinates release checks and installs using injected dependencies.
type Updater struct {
	deps Deps
}

type cachedCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Version   string    `json:"version"`
}

type buildInfo struct {
	raw     string
	version parsedVersion
}

type parsedVersion struct {
	raw   string
	base  string
	parts []int
	dev   bool
}

type stdoutReporter struct {
	out        io.Writer
	progressFn func(downloaded, total int64)
}

type nopReporter struct{}

// CheckForUpdate checks if a newer version is available.
// Uses a 1-hour cache to avoid hitting GitHub API too often.
func CheckForUpdate(forceCheck bool) (*UpdateInfo, error) {
	return defaultUpdater().CheckForUpdate(forceCheck)
}

// PerformUpdate downloads and installs the update.
func PerformUpdate(info *UpdateInfo, progressFn func(downloaded, total int64)) error {
	return defaultUpdater().PerformUpdate(info, stdoutReporter{
		out:        os.Stdout,
		progressFn: progressFn,
	})
}

// RestartDaemon stops and starts the daemon
func RestartDaemon() error {
	// The CLI will handle the actual restart via `roborev daemon restart`
	// Since we're in a library, we just return nil
	return nil
}

// GetCacheDir returns the roborev cache directory
func GetCacheDir() string {
	return config.DataDir()
}

// NewUpdater returns an updater with defaults filled for any missing dependencies.
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
	if deps.MkdirTemp == nil {
		deps.MkdirTemp = os.MkdirTemp
	}
	return &Updater{deps: deps}
}

func defaultUpdater() *Updater {
	return NewUpdater(Deps{})
}

// CheckForUpdate checks if a newer version is available.
func (u *Updater) CheckForUpdate(forceCheck bool) (*UpdateInfo, error) {
	build := u.currentBuild()

	// Don't nag on dev builds. Explicit `roborev update` still works.
	if build.version.dev && !forceCheck {
		return nil, nil
	}

	if cached, handled, err := u.cachedUpdate(build, forceCheck); err != nil {
		return nil, err
	} else if handled {
		return cached, nil
	}

	return u.fetchReleaseInfo(build)
}

// PerformUpdate downloads and installs the update.
func (u *Updater) PerformUpdate(info *UpdateInfo, reporter Reporter) error {
	reporter = normalizeReporter(reporter)

	if info.Checksum == "" {
		return fmt.Errorf("no checksum available for %s - refusing to install unverified binary", info.AssetName)
	}

	tempDir, err := u.deps.MkdirTemp("", "roborev-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	archivePath, checksum, err := u.downloadArchive(tempDir, info, reporter)
	if err != nil {
		return err
	}
	if err := verifyChecksum(checksum, info.Checksum, reporter); err != nil {
		return err
	}

	extractDir, err := u.extractArchive(tempDir, archivePath, reporter)
	if err != nil {
		return err
	}

	installDir, err := u.installDir()
	if err != nil {
		return err
	}

	return u.installBinaries(extractDir, installDir, reporter)
}

func (u *Updater) currentBuild() buildInfo {
	return buildInfo{
		raw:     u.deps.Version,
		version: parseVersion(u.deps.Version),
	}
}

func (u *Updater) cachedUpdate(build buildInfo, forceCheck bool) (*UpdateInfo, bool, error) {
	if forceCheck {
		return nil, false, nil
	}

	cached, err := u.loadCache()
	if err != nil {
		return nil, false, nil
	}
	if u.deps.Now().Sub(cached.CheckedAt) >= cacheDuration {
		return nil, false, nil
	}

	latest := parseVersion(cached.Version)
	if !latest.newerThan(build.version) {
		return nil, true, nil
	}

	return nil, false, nil
}

func (u *Updater) fetchReleaseInfo(build buildInfo) (*UpdateInfo, error) {
	tag, err := u.resolveLatestTag(githubLatestReleaseURL)
	if err != nil {
		return nil, fmt.Errorf("check for updates: %w", err)
	}

	// Cache failures should not block update checks.
	_ = u.saveCache(tag)

	latest := parseVersion(tag)
	if !build.version.dev && !latest.newerThan(build.version) {
		return nil, nil
	}

	assetVersion := strings.TrimPrefix(tag, "v")
	assetName := fmt.Sprintf("roborev_%s_%s_%s.tar.gz", assetVersion, u.deps.GOOS, u.deps.GOARCH)
	downloadURL := fmt.Sprintf("%s/%s/%s", githubReleaseDownloadBase, tag, assetName)
	checksumsURL := fmt.Sprintf("%s/%s/SHA256SUMS", githubReleaseDownloadBase, tag)

	// HEAD the asset to confirm it exists for this platform. The previous
	// API-based code returned "no release asset" up front; now that we
	// construct the URL ourselves, we have to verify it resolves.
	size, err := u.fetchContentLength(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("no release asset for %s/%s: %w", u.deps.GOOS, u.deps.GOARCH, err)
	}

	checksum, _ := u.fetchChecksumFromFile(checksumsURL, assetName)

	return &UpdateInfo{
		CurrentVersion: build.raw,
		LatestVersion:  tag,
		DownloadURL:    downloadURL,
		AssetName:      assetName,
		Size:           size,
		Checksum:       checksum,
		IsDevBuild:     build.version.dev,
	}, nil
}

func (u *Updater) downloadArchive(tempDir string, info *UpdateInfo, reporter Reporter) (string, string, error) {
	reporter.Stepf("Downloading %s...\n", info.AssetName)

	archivePath := filepath.Join(tempDir, info.AssetName)
	checksum, err := u.downloadFile(info.DownloadURL, archivePath, info.Size, reporter.Progress)
	if err != nil {
		return "", "", fmt.Errorf("download: %w", err)
	}

	return archivePath, checksum, nil
}

func verifyChecksum(actual, expected string, reporter Reporter) error {
	reporter = normalizeReporter(reporter)
	reporter.Stepf("Verifying checksum... ")
	if !strings.EqualFold(actual, expected) {
		reporter.Stepf("FAILED\n")
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}
	reporter.Stepf("OK\n")
	return nil
}

func (u *Updater) extractArchive(tempDir, archivePath string, reporter Reporter) (string, error) {
	reporter.Stepf("Extracting...\n")

	extractDir := filepath.Join(tempDir, "extracted")
	if err := extractTarGz(archivePath, extractDir); err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}

	return extractDir, nil
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

func (u *Updater) installBinaries(extractDir, installDir string, reporter Reporter) error {
	for _, binary := range u.binaryNames() {
		srcPath := filepath.Join(extractDir, binary)
		if _, err := os.Stat(srcPath); os.IsNotExist(err) {
			continue
		}

		dstPath := filepath.Join(installDir, binary)
		reporter.Stepf("Installing %s... ", binary)
		if err := u.installBinary(srcPath, dstPath); err != nil {
			return err
		}
		reporter.Stepf("OK\n")
	}

	return nil
}

func (u *Updater) installBinary(srcPath, dstPath string) error {
	backupPath := dstPath + ".old"
	_ = os.Remove(backupPath)

	if _, err := os.Stat(dstPath); err == nil {
		if err := os.Rename(dstPath, backupPath); err != nil {
			binary := filepath.Base(dstPath)
			if u.deps.GOOS == "windows" {
				return fmt.Errorf("cannot update %s while it is running - please stop the daemon and try again: %w", binary, err)
			}
			return fmt.Errorf("backup %s: %w", binary, err)
		}
	}

	if err := copyFile(srcPath, dstPath); err != nil {
		if _, statErr := os.Stat(backupPath); statErr == nil {
			if restoreErr := os.Rename(backupPath, dstPath); restoreErr != nil {
				return fmt.Errorf("restore backup for %s: %w", filepath.Base(dstPath), restoreErr)
			}
		}
		return fmt.Errorf("install %s: %w", filepath.Base(dstPath), err)
	}

	if u.deps.GOOS != "windows" {
		if err := os.Chmod(dstPath, 0755); err != nil {
			return fmt.Errorf("chmod %s: %w", filepath.Base(dstPath), err)
		}
	}

	_ = os.Remove(backupPath)
	return nil
}

func (u *Updater) binaryNames() []string {
	if u.deps.GOOS == "windows" {
		return []string{"roborev.exe"}
	}
	return []string{"roborev"}
}

func (u *Updater) newRequest(method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "roborev/"+u.deps.Version)
	return req, nil
}

func (u *Updater) get(url string) (*http.Response, error) {
	req, err := u.newRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return u.deps.Client.Do(req)
}

// resolveLatestTag follows the /releases/latest 302 redirect to
// /releases/tag/<tag> and returns the tag. Using the HTML endpoint
// avoids api.github.com's 60-req/hr unauthenticated rate limit.
func (u *Updater) resolveLatestTag(url string) (string, error) {
	req, err := u.newRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	// Wrap the injected client so we don't follow the redirect; we just want
	// to read the Location header off the 302. Sharing Transport/Timeout
	// keeps test transports and configured timeouts in effect.
	client := &http.Client{
		Transport: u.deps.Client.Transport,
		Timeout:   u.deps.Client.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("expected redirect from %s, got %s", url, resp.Status)
	}

	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("read Location header: %w", err)
	}

	const marker = "/releases/tag/"
	idx := strings.Index(loc.Path, marker)
	if idx < 0 {
		return "", fmt.Errorf("unexpected redirect target %q", loc.String())
	}
	tag := loc.Path[idx+len(marker):]
	if tag == "" {
		return "", fmt.Errorf("empty tag in redirect target %q", loc.String())
	}
	return tag, nil
}

// fetchContentLength does a HEAD request and returns the Content-Length
// of the eventual asset (following redirects to the S3 backend).
// Returns 0 if the size can't be determined; callers degrade gracefully.
func (u *Updater) fetchContentLength(url string) (int64, error) {
	req, err := u.newRequest(http.MethodHead, url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := u.deps.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HEAD %s returned %s", url, resp.Status)
	}
	if resp.ContentLength < 0 {
		return 0, nil
	}
	return resp.ContentLength, nil
}

func (u *Updater) downloadFile(url, dest string, totalSize int64, progressFn func(downloaded, total int64)) (string, error) {
	resp, err := u.get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}

	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()

	hasher := sha256.New()
	writer := io.MultiWriter(out, hasher)

	var downloaded int64
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				return "", writeErr
			}
			downloaded += int64(n)
			if progressFn != nil {
				progressFn(downloaded, totalSize)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func extractTarGz(archivePath, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve dest dir: %w", err)
	}

	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := extractTarEntry(tr, header, absDestDir); err != nil {
			return err
		}
	}

	return nil
}

func extractTarEntry(tr *tar.Reader, header *tar.Header, destDir string) error {
	target, err := sanitizeTarPath(destDir, header.Name)
	if err != nil {
		return fmt.Errorf("invalid tar entry %q: %w", header.Name, err)
	}

	if isTarLink(header) {
		return nil
	}

	switch header.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0755)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		outFile, err := os.Create(target)
		if err != nil {
			return err
		}
		defer outFile.Close()
		if _, err := io.Copy(outFile, tr); err != nil {
			return err
		}
		return os.Chmod(target, os.FileMode(header.Mode))
	default:
		return nil
	}
}

func isTarLink(header *tar.Header) bool {
	return header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink
}

// sanitizeTarPath validates and sanitizes a tar entry path to prevent directory traversal.
func sanitizeTarPath(destDir, name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute path not allowed")
	}

	cleanName := filepath.Clean(name)
	if filepath.IsAbs(cleanName) {
		return "", fmt.Errorf("absolute path not allowed")
	}
	if strings.HasPrefix(cleanName, "..") || strings.Contains(cleanName, string(filepath.Separator)+"..") {
		return "", fmt.Errorf("path traversal not allowed")
	}

	target := filepath.Join(destDir, cleanName)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absTarget, absDestDir+string(filepath.Separator)) && absTarget != absDestDir {
		return "", fmt.Errorf("path escapes destination directory")
	}

	return target, nil
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	_, err = io.Copy(out, in)
	return err
}

func (u *Updater) fetchChecksumFromFile(url, assetName string) (string, error) {
	resp, err := u.get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch checksums: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return extractChecksum(string(body), assetName), nil
}

func extractChecksum(releaseBody, assetName string) string {
	for line := range strings.SplitSeq(releaseBody, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, assetName) {
			continue
		}
		if match := checksumPattern.FindString(line); match != "" {
			return strings.ToLower(match)
		}
	}
	return ""
}

func (u *Updater) loadCache() (*cachedCheck, error) {
	cachePath := filepath.Join(u.deps.CacheDir(), cacheFileName)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, err
	}

	var cached cachedCheck
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, err
	}
	return &cached, nil
}

func (u *Updater) saveCache(version string) error {
	cached := cachedCheck{
		CheckedAt: u.deps.Now(),
		Version:   version,
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return err
	}

	cachePath := filepath.Join(u.deps.CacheDir(), cacheFileName)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		return err
	}
	return os.WriteFile(cachePath, data, 0644)
}

func parseVersion(raw string) parsedVersion {
	trimmed := strings.TrimPrefix(raw, "v")
	base := semverBasePattern.FindString(trimmed)
	version := parsedVersion{
		raw:  raw,
		base: base,
		dev:  base == "" || gitDescribePattern.MatchString(trimmed),
	}
	if base == "" {
		return version
	}

	parts := strings.Split(base, ".")
	version.parts = make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return parsedVersion{raw: raw, dev: true}
		}
		version.parts = append(version.parts, n)
	}
	return version
}

func (v parsedVersion) Compare(other parsedVersion) int {
	maxLen := max(len(other.parts), len(v.parts))
	for i := range maxLen {
		var left, right int
		if i < len(v.parts) {
			left = v.parts[i]
		}
		if i < len(other.parts) {
			right = other.parts[i]
		}
		if left > right {
			return 1
		}
		if left < right {
			return -1
		}
	}
	return 0
}

func (v parsedVersion) newerThan(other parsedVersion) bool {
	if len(v.parts) == 0 || len(other.parts) == 0 {
		return false
	}
	return v.Compare(other) > 0
}

// extractBaseSemver extracts the base semver from a version string.
func extractBaseSemver(v string) string {
	return parseVersion(v).base
}

// isDevBuildVersion returns true if the version is a dev build.
func isDevBuildVersion(v string) bool {
	return parseVersion(v).dev
}

// isNewer returns true if v1 is newer than v2.
func isNewer(v1, v2 string) bool {
	return parseVersion(v1).newerThan(parseVersion(v2))
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

// FormatSize formats bytes as human-readable string.
func FormatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
