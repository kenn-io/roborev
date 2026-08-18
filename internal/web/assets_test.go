package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func completeDistribution() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte(`<!doctype html><meta name="roborev-web-distribution" content="production"><meta name="roborev-base-path" content="" /><base href="/" /><div id="app"></div>`)},
		".vite/manifest.json":     {Data: []byte(`{"index.html":{"file":"assets/index-a1b2c3.js","css":["assets/index-a1b2c3.css"]}}`)},
		"assets/index-a1b2c3.js":  {Data: []byte(`console.log("ready")`)},
		"assets/index-a1b2c3.css": {Data: []byte(`body{margin:0}`)},
	}
}

func TestValidateReleaseDistributionRejectsCompilationStub(t *testing.T) {
	for _, index := range []string{
		compilationStub,
		strings.ReplaceAll(compilationStub, "\n", "\r\n"),
	} {
		_, err := validateReleaseDistribution(fstest.MapFS{
			"index.html": {Data: []byte(index)},
		})
		require.ErrorContains(t, err, "compilation stub")
	}
}

func TestLoadDistributionAcceptsCompilationStubAcrossLineEndings(t *testing.T) {
	for _, index := range []string{
		compilationStub,
		strings.ReplaceAll(compilationStub, "\n", "\r\n"),
	} {
		catalog, err := loadDistribution(fstest.MapFS{
			"index.html": {Data: []byte(index)},
		})
		require.NoError(t, err)
		assert.True(t, catalog.stub)
	}
}

func TestLoadDistributionRejectsUnknownIncompleteDistribution(t *testing.T) {
	_, err := loadDistribution(fstest.MapFS{
		"index.html": {Data: []byte("not the canonical stub")},
	})
	require.ErrorContains(t, err, "production marker")
}

func TestValidateReleaseDistributionRejectsMissingManifestAsset(t *testing.T) {
	files := completeDistribution()
	delete(files, "assets/index-a1b2c3.js")
	_, err := validateReleaseDistribution(files)
	require.ErrorContains(t, err, "assets/index-a1b2c3.js")
}

func TestValidateReleaseDistributionAcceptsCompleteViteGraph(t *testing.T) {
	catalog, err := validateReleaseDistribution(completeDistribution())
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{
		"assets/index-a1b2c3.css": {},
		"assets/index-a1b2c3.js":  {},
	}, catalog.immutable)
}

func TestValidateReleaseDistributionRejectsUnsafeUnreferencedFile(t *testing.T) {
	files := completeDistribution()
	files["token.json"] = &fstest.MapFile{Data: []byte(`{"value":"not-a-real-token"}`)}
	_, err := validateReleaseDistribution(files)
	require.ErrorContains(t, err, "secret-like")
}

func TestLoadAssetCatalogRejectsTraversalAndSecretLikePaths(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "traversal", path: "../outside.js"},
		{name: "backslash", path: `assets\\outside.js`},
		{name: "dot segment", path: "assets/.hidden.js"},
		{name: "secret base", path: "assets/token.js"},
		{name: "private key", path: "assets/client.pem"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := completeDistribution()
			files[".vite/manifest.json"] = &fstest.MapFile{Data: []byte(`{"index.html":{"file":"` + tt.path + `"}}`)}
			files[tt.path] = &fstest.MapFile{Data: []byte("asset")}
			_, err := validateReleaseDistribution(files)
			require.Error(t, err)
		})
	}
}

func TestEmbeddedReleaseDistribution(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_WEB_RELEASE_CHECK") != "1" {
		t.Skip("release asset validation is enabled by the release target")
	}
	require.NoError(t, ValidateEmbeddedRelease())
}

func TestEmbeddedReleaseBasePathInjection(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_WEB_RELEASE_CHECK") != "1" {
		t.Skip("release asset validation is enabled by the release target")
	}
	handler, err := NewEmbeddedHandler("/review-ui")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/reviews/42", nil)
	req.Header.Set("Accept", "text/html")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `<meta name="roborev-base-path" content="/review-ui" />`)
	assert.Contains(t, recorder.Body.String(), `<base href="/review-ui/" />`)
}

func TestManifestAssetMustBeRegular(t *testing.T) {
	files := completeDistribution()
	files["assets/index-a1b2c3.js"].Mode = fs.ModeSymlink
	_, err := validateReleaseDistribution(files)
	require.ErrorContains(t, err, "regular file")
}
