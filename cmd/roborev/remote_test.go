package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/requestsigning"
)

type streamOutputFunc func([]byte) (int, error)

func (f streamOutputFunc) Write(p []byte) (int, error) { return f(p) }

func TestRemoteCLIStreamReconnectsWithFreshSignature(t *testing.T) {
	oldStart, oldMax := pollStartInterval, pollMaxInterval
	pollStartInterval, pollMaxInterval = time.Second, 5*time.Second
	t.Cleanup(func() { pollStartInterval, pollMaxInterval = oldStart, oldMax })
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	secret := make([]byte, 64)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	key := requestsigning.Key{ID: "reader", Secret: secret}
	var mu sync.Mutex
	var nonces []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := requestsigning.VerifyHeaders(r, "https://"+r.Host+r.URL.RequestURI(), map[string]requestsigning.Key{key.ID: key}, time.Now())
		assert.NoError(t, err)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/history/api/ping":
			_, _ = w.Write([]byte(`{"ok":true,"service":"roborev","version":"test"}`))
		case "/history/api/stream/events":
			mu.Lock()
			nonces = append(nonces, v.Nonce)
			empty := len(nonces) <= 6
			mu.Unlock()
			if empty {
				return
			}
			_, _ = w.Write([]byte("{\"type\":\"review.completed\",\"job_id\":42}\n"))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	t.Setenv("TEST_REMOTE_STREAM_SECRET", hex.EncodeToString(secret))
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), fmt.Appendf(nil, "[remote_client]\nexternal_url='%s'\nkey_id='reader'\nsecret_env='TEST_REMOTE_STREAM_SECRET'\nca_file='%s'\n", server.URL+"/history", caFile), 0o600))
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	serverAddr, parsedServerEndpoint = server.URL+"/history", nil
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	// Exercise real TLS signing without wall-clock backoff. Timer behavior is
	// covered separately under synctest; external sockets cannot drive that clock.
	cmd := streamCmdWithWait(func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return ctx.Err()
	})
	cmd.SetContext(ctx)
	var output, notices bytes.Buffer
	cmd.SetErr(&notices)
	cmd.SetOut(streamOutputFunc(func(p []byte) (int, error) {
		n, err := output.Write(p)
		if strings.Count(output.String(), "\n") == 2 {
			cancel()
		}
		return n, err
	}))
	require.NoError(t, cmd.Execute())
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, nonces, 8)
	assert := assert.New(t)
	unique := map[string]bool{}
	for _, nonce := range nonces {
		unique[nonce] = true
	}
	assert.Len(unique, 8)
	assert.Equal(2, strings.Count(output.String(), "\n"))
	assert.Equal([]time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second, time.Second}, delays)
	assert.Equal(1, strings.Count(notices.String(), "reconnect"), "the reconnect notice prints once per stream")
}

func TestRemoteCommandsRejectLocalOperations(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[remote_client]\nexternal_url='https://reviews.example.com/history'\n"), 0o600))
	old := serverAddr
	serverAddr = "https://reviews.example.com/history"
	t.Cleanup(func() { serverAddr = old; parsedServerEndpoint = nil })
	for _, name := range []string{"fix", "refine", "review", "daemon"} {
		require.Error(t, validateRemoteCommand(name))
	}
	for _, name := range []string{"list", "show", "wait"} {
		require.NoError(t, validateRemoteCommand(name))
	}
}

func TestRemoteCLIPureCommandsDoNotNeedDaemonCredentials(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[remote_client]\nexternal_url='https://reviews.example.com/history'\n"), 0o600))
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint })
	for _, args := range [][]string{{"help", "version"}, {"version"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}, {"completion", "powershell"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			root := &cobra.Command{Use: "roborev", PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
				return validateRemoteCommandFor(cmd)
			}}
			root.PersistentFlags().StringVar(&serverAddr, "server", "", "daemon endpoint")
			root.AddCommand(versionCmd())
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs(append([]string{"--server", "https://reviews.example.com/history"}, args...))
			require.NoError(t, root.Execute())
			assert.NotEmpty(t, output.String())
		})
	}
}

func TestRemoteStreamRejectsLocalPathBeforeConnecting(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	// No client credentials: path filters must fail before daemon probing.
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[remote_client]\nexternal_url='https://reviews.example.com/history'\n"), 0o600))
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	serverAddr = "https://reviews.example.com/history"
	parsedServerEndpoint = nil
	t.Cleanup(func() { serverAddr = oldAddr; parsedServerEndpoint = oldEndpoint })
	cmd := streamCmd()
	cmd.SetArgs([]string{"--repo", "."})
	require.ErrorContains(t, cmd.Execute(), "remote stream uses server repository grants; local path filters are unavailable")
}

func TestRemoteListAndShowRejectUsageBeforeConnecting(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[remote_client]\nexternal_url='https://reviews.example.com/history'\n"), 0o600))
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	serverAddr, parsedServerEndpoint = "https://reviews.example.com/history", nil
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint })
	for _, tc := range []struct {
		name string
		cmd  func() *cobra.Command
		args []string
		want string
	}{
		{"list_repo", listCmd, []string{"--repo", "."}, "remote list uses server repository grants"},
		{"list_file", listCmd, []string{"--file", "example.go"}, "remote list uses server repository grants"},
		{"show_default", showCmd, nil, "remote show requires --job"},
		{"show_ref", showCmd, []string{"HEAD"}, "remote show requires --job"},
		{"show_invalid_id", showCmd, []string{"--job", "invalid"}, "remote show requires a positive numeric job ID"},
		{"show_zero", showCmd, []string{"--job", "0"}, "remote show requires a positive numeric job ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.cmd()
			cmd.SetArgs(tc.args)
			require.ErrorContains(t, cmd.Execute(), tc.want)
		})
	}
}

func TestSigningInitWritesPrivateNewFiles(t *testing.T) {
	key := filepath.Join(t.TempDir(), "reader.key")
	cmd := signingInitCmd()
	cmd.SetArgs([]string{"--secret-file", key})
	if runtime.GOOS == "windows" {
		require.Error(t, cmd.Execute())
		assert.NoFileExists(t, key)
		return
	}
	require.NoError(t, cmd.Execute())
	info, err := os.Stat(key)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.Error(t, cmd.Execute(), "an existing key file is never overwritten")
}

func TestRemoteCLIListOutsideGitUsesNativeSigning(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	secret := make([]byte, 64)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	key := requestsigning.Key{ID: "reader", Secret: secret}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := requestsigning.VerifyHeaders(r, "https://"+r.Host+r.URL.RequestURI(), map[string]requestsigning.Key{key.ID: key}, time.Now())
		assert.NoError(t, err)
		if err != nil {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/history/api/ping":
			_, _ = w.Write([]byte(`{"ok":true,"service":"roborev","version":"test"}`))
		case "/history/api/jobs":
			assert.Empty(t, r.URL.Query().Get("repo_prefix"))
			assert.Empty(t, r.URL.Query().Get("repo"))
			assert.Empty(t, r.URL.Query().Get("branch"))
			if r.URL.Query().Has("repo_prefix") {
				w.WriteHeader(403)
				return
			}
			_, _ = w.Write([]byte(`{"jobs":[],"has_more":false}`))
		default:
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	t.Setenv("TEST_REMOTE_SIGNING_SECRET", hex.EncodeToString(secret))
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), fmt.Appendf(nil, "[remote_client]\nexternal_url='%s'\nkey_id='reader'\nsecret_env='TEST_REMOTE_SIGNING_SECRET'\nca_file='%s'\n", server.URL+"/history", caFile), 0o600))
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	serverAddr = server.URL + "/history"
	parsedServerEndpoint = nil
	t.Cleanup(func() { serverAddr = oldAddr; parsedServerEndpoint = oldEndpoint })
	cmd := listCmd()
	cmd.SetArgs([]string{"--json"})
	output := captureStdout(t, func() { require.NoError(t, cmd.Execute()) })
	assert.Contains(t, output, "[]")
}
