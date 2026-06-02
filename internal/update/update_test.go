package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type archiveEntry struct {
	Name     string
	Content  string
	TypeFlag byte
	LinkName string
	Mode     int64
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type testReporter struct {
	steps    bytes.Buffer
	progress []int64
}

func (r *testReporter) Stepf(format string, args ...any) {
	_, _ = fmt.Fprintf(&r.steps, format, args...)
}

func (r *testReporter) Progress(downloaded, total int64) {
	r.progress = append(r.progress, downloaded, total)
}

func TestUpdaterCheckForUpdateSkipsNetworkWithFreshCache(t *testing.T) {
	cacheDir := t.TempDir()
	now := time.Date(2026, 3, 19, 12, 0, 0, 0, time.UTC)
	writeCachedCheck(t, cacheDir, "v1.2.3", now.Add(-15*time.Minute))

	requests := 0
	updater := NewUpdater(Deps{
		Client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				return nil, fmt.Errorf("unexpected request to %s", req.URL.String())
			}),
		},
		Now:      func() time.Time { return now },
		Version:  "v1.2.3",
		GOOS:     "darwin",
		GOARCH:   "arm64",
		CacheDir: func() string { return cacheDir },
	})

	info, err := updater.CheckForUpdate(false)
	require.NoError(t, err)
	require.Nil(t, info)
	assert.Equal(t, 0, requests)
}

func TestUpdaterCheckForUpdateUsesKitGitHubAPIReleaseDiscovery(t *testing.T) {
	const releaseTag = "v1.3.0"
	const assetName = "roborev_1.3.0_windows_amd64.zip"
	const checksum = "abc123def456789012345678901234567890123456789012345678901234abcd"

	apiBaseURL := "https://api.example.test"
	releaseURL := apiBaseURL + "/repos/roborev-dev/roborev/releases/latest"
	downloadURL := "https://downloads.example.test/" + assetName
	checksumsURL := "https://downloads.example.test/SHA256SUMS"
	seen := []string{}

	updater := NewUpdater(Deps{
		Client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				seen = append(seen, req.Method+" "+req.URL.String())
				switch req.URL.String() {
				case releaseURL:
					require.Equal(t, http.MethodGet, req.Method)
					assert.Equal(t, "application/vnd.github.v3+json", req.Header.Get("Accept"))
					body := fmt.Sprintf(`{
						"tag_name": %q,
						"body": "",
						"assets": [
							{"name": %q, "size": 42, "browser_download_url": %q},
							{"name": "SHA256SUMS", "size": 128, "browser_download_url": %q}
						]
					}`, releaseTag, assetName, downloadURL, checksumsURL)
					return newHTTPResponse(http.StatusOK, body), nil
				case checksumsURL:
					require.Equal(t, http.MethodGet, req.Method)
					return newHTTPResponse(http.StatusOK, fmt.Sprintf("%s  %s\n", checksum, assetName)), nil
				default:
					return nil, fmt.Errorf("unexpected request to %s", req.URL.String())
				}
			}),
		},
		Now:              func() time.Time { return time.Unix(0, 0) },
		Version:          "v1.2.0",
		GOOS:             "windows",
		GOARCH:           "amd64",
		CacheDir:         t.TempDir,
		GitHubAPIBaseURL: apiBaseURL,
	})

	info, err := updater.CheckForUpdate(true)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, []string{
		http.MethodGet + " " + releaseURL,
		http.MethodGet + " " + checksumsURL,
	}, seen)
	assert.Equal(t, "roborev-dev", info.Owner)
	assert.Equal(t, "roborev", info.Repo)
	assert.Equal(t, "windows", info.GOOS)
	assert.Equal(t, "amd64", info.GOARCH)
	assert.Equal(t, "v1.2.0", info.CurrentVersion)
	assert.Equal(t, releaseTag, info.LatestVersion)
	assert.Equal(t, assetName, info.AssetName)
	assert.Equal(t, downloadURL, info.DownloadURL)
	assert.Equal(t, int64(42), info.Size)
	assert.Equal(t, checksum, info.Checksum)
	assert.False(t, info.IsDevBuild)
}

func TestUpdaterPerformUpdateInstallsBinary(t *testing.T) {
	binaryName := "roborev"
	if runtime.GOOS == "windows" {
		binaryName = "roborev.exe"
	}

	archiveData := createTestArchiveBytes(t, []archiveEntry{
		{Name: binaryName, Content: "new-binary", Mode: 0o755},
	})
	sum := sha256.Sum256(archiveData)
	expectedChecksum := hex.EncodeToString(sum[:])

	binDir := t.TempDir()
	currentBinary := filepath.Join(binDir, binaryName)
	require.NoError(t, os.WriteFile(currentBinary, []byte("old-binary"), 0o755))

	updater := NewUpdater(Deps{
		Client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "https://downloads.example/"+binaryName+".tar.gz", req.URL.String())
				return newBinaryResponse(http.StatusOK, archiveData), nil
			}),
		},
		Version:    "v1.2.0",
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		Executable: func() (string, error) { return currentBinary, nil },
		CacheDir:   t.TempDir,
	})

	reporter := &testReporter{}
	err := updater.PerformUpdate(&UpdateInfo{
		AssetName:   binaryName + ".tar.gz",
		DownloadURL: "https://downloads.example/" + binaryName + ".tar.gz",
		Size:        int64(len(archiveData)),
		Checksum:    expectedChecksum,
	}, reporter)
	require.NoError(t, err)

	installed, readErr := os.ReadFile(currentBinary)
	require.NoError(t, readErr)
	assert.Equal(t, "new-binary", string(installed))
	requirePathMissing(t, currentBinary+".old")
	assert.Contains(t, reporter.steps.String(), "Downloading")
	assert.Contains(t, reporter.steps.String(), "Installing "+binaryName+"... OK")
	assert.NotEmpty(t, reporter.progress)
}

func TestUpdaterPerformUpdateInstallsWindowsZipBinary(t *testing.T) {
	const binaryName = "roborev.exe"
	const assetName = "roborev_1.3.0_windows_amd64.zip"

	archiveData := createTestZipArchiveBytes(t, []archiveEntry{
		{Name: binaryName, Content: "new-windows-binary", Mode: 0o755},
	})
	sum := sha256.Sum256(archiveData)
	expectedChecksum := hex.EncodeToString(sum[:])

	binDir := t.TempDir()
	currentBinary := filepath.Join(binDir, binaryName)
	require.NoError(t, os.WriteFile(currentBinary, []byte("old-windows-binary"), 0o755))

	updater := NewUpdater(Deps{
		Client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "https://downloads.example/"+assetName, req.URL.String())
				return newBinaryResponse(http.StatusOK, archiveData), nil
			}),
		},
		Version:    "v1.2.0",
		GOOS:       "windows",
		GOARCH:     "amd64",
		Executable: func() (string, error) { return currentBinary, nil },
		CacheDir:   t.TempDir,
	})

	reporter := &testReporter{}
	err := updater.PerformUpdate(&UpdateInfo{
		AssetName:   assetName,
		DownloadURL: "https://downloads.example/" + assetName,
		Size:        int64(len(archiveData)),
		Checksum:    expectedChecksum,
	}, reporter)
	require.NoError(t, err)

	installed, readErr := os.ReadFile(currentBinary)
	require.NoError(t, readErr)
	assert.Equal(t, "new-windows-binary", string(installed))
	requirePathMissing(t, currentBinary+".old")
	assert.Contains(t, reporter.steps.String(), "Downloading")
	assert.Contains(t, reporter.steps.String(), "Installing "+binaryName+"... OK")
	assert.NotEmpty(t, reporter.progress)
}

func createTestArchiveBytes(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for _, entry := range entries {
		mode := entry.Mode
		if mode == 0 {
			mode = 0o644
		}
		typeFlag := entry.TypeFlag
		if typeFlag == 0 {
			typeFlag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.Name,
			Mode:     mode,
			Size:     int64(len(entry.Content)),
			Typeflag: typeFlag,
			Linkname: entry.LinkName,
		}
		require.NoError(t, tw.WriteHeader(header))
		if len(entry.Content) > 0 {
			_, err := tw.Write([]byte(entry.Content))
			require.NoError(t, err)
		}
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	return buf.Bytes()
}

func createTestZipArchiveBytes(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for _, entry := range entries {
		mode := entry.Mode
		if mode == 0 {
			mode = 0o644
		}
		header := &zip.FileHeader{Name: entry.Name}
		header.SetMode(os.FileMode(mode))
		writer, err := zw.CreateHeader(header)
		require.NoError(t, err)
		if len(entry.Content) > 0 {
			_, err := writer.Write([]byte(entry.Content))
			require.NoError(t, err)
		}
	}

	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func requirePathMissing(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	require.Error(t, err)
	require.True(t, os.IsNotExist(err), "expected %s to be absent, got %v", path, err)
}

func writeCachedCheck(t *testing.T, cacheDir, cachedVersion string, checkedAt time.Time) {
	t.Helper()
	data, err := json.Marshal(struct {
		CheckedAt time.Time `json:"checked_at"`
		Version   string    `json:"version"`
	}{
		CheckedAt: checkedAt,
		Version:   cachedVersion,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, cacheFileName), data, 0o600))
}

func newHTTPResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func newBinaryResponse(statusCode int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}
