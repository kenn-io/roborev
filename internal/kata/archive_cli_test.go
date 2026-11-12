//go:build kata

package kata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveRubricPinnedCLI(t *testing.T) {
	bin := os.Getenv("ROBOREV_KATA_TEST_BINARY")
	if bin == "" {
		t.Skip("ROBOREV_KATA_TEST_BINARY is required for pinned CLI coverage")
	}
	for _, tc := range []struct {
		version string
		uploads int
	}{{"0.45.0", 1}, {"0.44.0", 0}} {
		t.Run(tc.version, func(t *testing.T) {
			var mu sync.Mutex
			var uploads int
			var project, body, reuse, name string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					fmt.Fprintf(w, `{"ok":true,"api_schema_version":%q}`, tc.version)
				case "/api/v1/projects/resolve":
					var req struct {
						Name string `json:"name"`
					}
					_ = json.NewDecoder(r.Body).Decode(&req)
					mu.Lock()
					project = req.Name
					mu.Unlock()
					_, _ = io.WriteString(w, `{"project":{"id":1,"name":"demo"}}`)
				case "/api/v1/projects/1/issues/abc4/attachments":
					data, _ := io.ReadAll(r.Body)
					mu.Lock()
					uploads++
					body = string(data)
					reuse = r.URL.Query().Get("reuse_existing")
					name = r.URL.Query().Get("name")
					mu.Unlock()
					_, _ = io.WriteString(w, `{"attachment_id":"attachment.test"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			home := t.TempDir()
			c := NewCLIClient(home)
			c.bin = bin
			c.run = func(ctx context.Context, c *CLIClient, args []string, _ io.Reader) ([]byte, error) {
				switch args[0] {
				case "projects":
					return []byte(`{"project":{"id":1}}`), nil
				case "show":
					return []byte(`{"issue":{"project_id":1,"uid":"uid1","short_id":"abc4"}}`), nil
				}
				cmd := exec.CommandContext(ctx, c.bin, args...)
				cmd.Dir = home
				cmd.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "XDG_CACHE_HOME=" + filepath.Join(home, "cache"), "KATA_SERVER=" + server.URL, "USER=fixture", "PATH=/usr/bin:/bin"}
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				out, err := cmd.Output()
				if err != nil {
					return nil, fmt.Errorf("kata: %w: %s", err, stderr.String())
				}
				return out, nil
			}
			err := c.ArchiveRubric(t.Context(), "demo", "uid1", "rubric.md", []byte("rubric\n"))
			if tc.uploads == 1 {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "requires daemon API 0.45.0")
			}
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, "demo", project)
			assert.Equal(t, tc.uploads, uploads)
			if tc.uploads == 1 {
				assert.Equal(t, "rubric\n", body)
				assert.Equal(t, "true", reuse)
				assert.Equal(t, "rubric.md", name)
			}
		})
	}
}
