package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestConfigGetWarnsForUnusedDaemonSections(t *testing.T) {
	for _, tc := range []struct {
		name, section string
		warnings      []string
	}{
		{"web token file", "[web]\nauth_token_file = 'missing-token'\n", []string{"[web]"}},
		{"search provider", "[search.embeddings]\nbase_url = 'https://example.test/v1'\n", []string{"[search]"}},
		{"budget", "[budget]\nenabled = true\n", []string{"[budget]"}},
		{"remote listener", "[remote]\nenabled = true\n", []string{"[remote]"}},
		{"CI installations", "[ci.github_app_installations]\nExample = 1\nexample = 2\n", []string{"[ci]"}},
		{"multiple sections", "[remote]\nenabled = true\n[web]\nauth_mode = 'proxy'\n", []string{"[remote]", "[web]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
			require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("hook_timeout_seconds = 17\n"+tc.section), 0o600))
			cmd := newRootCmd()
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"config", "get", "hook_timeout_seconds", "--global"})
			stdout := captureStdout(t, func() { require.NoError(t, cmd.Execute()) })
			assert.Equal(t, "17\n", stdout)
			for _, section := range tc.warnings {
				assert.Contains(t, stderr.String(), section)
				assert.Equal(t, 1, bytes.Count(stderr.Bytes(), []byte(section)))
			}
		})
	}
}

func TestCommandsRejectMalformedGlobalTOML(t *testing.T) {
	for _, tc := range []struct{ contents, wantError string }{
		{"[web\n", "invalid global config TOML"},
		{"[web]\nenabled = 'yes'\n", "incompatible types"},
	} {
		for _, args := range [][]string{{"config", "get", "hook_timeout_seconds", "--global"}, {"version"}, {"quickstart"}, {"export", "reviews"}, {"daemon", "run"}} {
			t.Run(fmt.Sprint(args, tc.contents), func(t *testing.T) {
				oldEndpoint := parsedServerEndpoint
				t.Cleanup(func() { parsedServerEndpoint = oldEndpoint })
				t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
				t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
				var requests atomic.Int32
				NewMockDaemon(t, MockRefineHooks{
					OnPing: func(http.ResponseWriter, *http.Request, *mockRefineState) bool {
						requests.Add(1)
						return false
					},
					OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
						requests.Add(1)
						if r.URL.Path != "/api/export/reviews" {
							return false
						}
						writeExportTestPage(t, w, r.URL.Query().Get("profile"), false, nil, nil)
						return true
					},
				})
				cmd := newRootCmd()
				require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(tc.contents), 0o600))
				cmd.SetArgs(args)
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(&bytes.Buffer{})
				require.ErrorContains(t, cmd.Execute(), tc.wantError)
				assert.Zero(t, requests.Load())
				if args[0] == "export" {
					require.NoError(t, os.WriteFile(config.GlobalConfigPath(), nil, 0o600))
					require.NoError(t, cmd.Execute(), "the same export succeeds with valid config")
					assert.Positive(t, requests.Load())
				}
			})
		}
	}
}

func TestDaemonAndConfigValidateRejectInvalidWebConfig(t *testing.T) {
	for _, args := range [][]string{{"daemon", "run"}, {"config", "validate", "--global"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dir)
			t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
			require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[web]\nauth_token_file = 'missing-token'\n"), 0o600))
			cmd := newRootCmd()
			cmd.SetArgs(args)
			cmd.SetErr(&bytes.Buffer{})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			cmd.SetContext(ctx)
			require.ErrorContains(t, cmd.Execute(), "read web auth token file")
			_, err := os.Stat(filepath.Join(dir, "reviews.db"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestDoctorRejectsInvalidWebConfig(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[web]\nauth_token_file = 'missing-token'\n"), 0o600))
	env := loadDoctorEnv(t.Context(), "", fakeDoctorDaemon{})
	require.ErrorContains(t, env.globalErr, "read web auth token file")
}

func TestDaemonRunValidatesExplicitConfigInsteadOfDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[broken\n"), 0o600))
	path := filepath.Join(dir, "daemon.toml")
	require.NoError(t, os.WriteFile(path, []byte("[web]\nauth_mode = 'proxy'\n"), 0o600))
	cmd := newRootCmd()
	cmd.SetArgs([]string{"daemon", "run", "--config", path})
	cmd.SetErr(&bytes.Buffer{})
	require.ErrorContains(t, cmd.Execute(), "requires a public origin")
}

func TestConfigGetPreservesUnknownKeyPolicy(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("hook_timeout_seconds = 17\n[future_section]\nfuture_key = true\n"), 0o600))
	cmd := newRootCmd()
	cmd.SetArgs([]string{"config", "get", "hook_timeout_seconds", "--global"})
	output := captureStdout(t, func() { require.NoError(t, cmd.Execute()) })
	assert.Equal(t, "17\n", output)
}

func TestExportReviewsWarnsForUnusedWebConfig(t *testing.T) {
	assert := assert.New(t)
	oldEndpoint := parsedServerEndpoint
	t.Cleanup(func() { parsedServerEndpoint = oldEndpoint })
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
	cmd := newRootCmd()
	NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		if r.URL.Path != "/api/export/reviews" {
			return false
		}
		writeExportTestPage(t, w, "metadata", false, nil, []map[string]any{{"review_id": testUUID("exported-review")}})
		return true
	}})
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[web]\nauth_token_file = 'missing-token'\n"), 0o600))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"export", "reviews", "--profile", "metadata"})
	require.NoError(t, cmd.Execute())
	var document struct {
		Reviews []struct {
			ReviewID string `json:"review_id"`
		} `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &document))
	require.Len(t, document.Reviews, 1)
	assert.Equal(testUUIDText("exported-review"), document.Reviews[0].ReviewID)
	assert.Contains(stderr.String(), "[web]")
	assert.NotContains(stdout.String(), "Warning")
}

func TestVersionDoesNotValidateUnusedWorkflowSettings(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[acp.reviewer]\ncommand = ''\n"), 0o600))
	cmd := newRootCmd()
	cmd.SetArgs([]string{"version"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.NoError(t, cmd.Execute())
}

func TestBuildCompactPromptWarnsOnConfigLoadFailure(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[web\n"), 0o600))
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	defer writer.Close()
	old := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = old }()
	buildCompactPrompt(t.Context(), nil, "", t.TempDir(), "test")
	require.NoError(t, writer.Close())
	output, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Contains(t, string(output), "Warning: load global config")
}
