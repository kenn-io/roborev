package daemon

import (
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobLogAPIKeepsRawOrphanAccessAndPath(t *testing.T) {
	srv, _, _ := newTestServer(t)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.MkdirAll(JobLogDir(), 0o700))
	const content = "orphan output\n"
	require.NoError(t, os.WriteFile(JobLogPath(42), []byte(content), 0o600))
	raw := serveHuma(t, srv, http.MethodGet, "/api/job/log?job_id=42&raw=true", nil)
	require.Equal(t, http.StatusOK, raw.Code)
	assert.Equal(t, content, raw.Body.String())
	path := serveHuma(t, srv, http.MethodGet, "/api/job/log?job_id=43&path=true", nil)
	require.Equal(t, http.StatusOK, path.Code)
	assert.Equal(t, JobLogPath(43), path.Header().Get("X-Log-Path"))
	formatted := serveHuma(t, srv, http.MethodGet, "/api/job/log?job_id=42", nil)
	assert.Equal(t, http.StatusNotFound, formatted.Code)
}
