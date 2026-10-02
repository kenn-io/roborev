package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestResolveRepoIdentifier(t *testing.T) {
	// 1. Simple identifiers (no disk interaction or simple non-path)
	t.Run("simple identifiers", func(t *testing.T) {
		tests := []struct {
			name  string
			input string
			want  string
		}{
			{"name unchanged", "my-project", "my-project"},
			{"name with slash unchanged", "org/project", "org/project"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assertPath(t, resolveRepoIdentifier(t.Context(), tt.input), tt.want)
			})
		}
	})

	// 2. Permission test (isolated)
	t.Run("inaccessible path", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("skipping permission test on Windows")
		}
		if os.Getuid() == 0 {
			t.Skip("skipping permission test when running as root")
		}

		tmpDir := t.TempDir()
		orgDir := filepath.Join(tmpDir, "org")
		require.NoError(t, os.Mkdir(orgDir, 0o755), "Failed to create org dir")

		// Ensure we are in a safe directory (tmpDir) before modifying permissions of orgDir
		chdir(t, tmpDir)

		require.NoError(t, os.Chmod(orgDir, 0o000), "Failed to chmod")
		defer func() { _ = os.Chmod(orgDir, 0o755) }()

		// The test expects "org/project" because it can't stat "org" to see if it's a repo,
		// so it treats it as a name string.
		assertPath(t, resolveRepoIdentifier(t.Context(), "org/project"), "org/project")
	})

	// 3. Git repo resolution
	t.Run("git repo resolution", func(t *testing.T) {
		root := newTestGitRepo(t).Dir
		// Create common structure: root/sub/dir
		subDir := filepath.Join(root, "sub", "dir")
		require.NoError(t, os.MkdirAll(subDir, 0o755))

		// Also create a non-git temp dir for that one case
		nonGitDir := t.TempDir()
		resolvedNonGit, err := filepath.EvalSymlinks(nonGitDir)
		require.NoError(t, err, "Failed to resolve symlinks")

		tests := []struct {
			name  string
			dir   string // directory to execute from
			input string
			want  string
		}{
			{
				name:  "dot in subdir",
				dir:   filepath.Join(root, "sub", "dir"),
				input: ".",
				want:  root,
			},
			{
				name:  "relative path",
				dir:   filepath.Join(root, "sub"),
				input: "./",
				want:  root,
			},
			{
				name:  "absolute path",
				dir:   root,
				input: subDir,
				want:  root,
			},
			{
				name:  "parent traversal",
				dir:   filepath.Join(root, "sub", "dir"),
				input: "..",
				want:  root,
			},
			{
				name:  "non-git path returns absolute path",
				dir:   resolvedNonGit,
				input: ".",
				want:  resolvedNonGit,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				chdir(t, tt.dir)
				got := resolveRepoIdentifier(t.Context(), tt.input)
				assertPath(t, got, tt.want)
			})
		}
	})
}

func assertPath(t *testing.T, got, want string) {
	t.Helper()
	assert.Equal(t, want, got)
}

func TestRepoCommandsUseDaemonAPI(t *testing.T) {
	for _, action := range []string{"list", "show", "rename", "move", "delete", "merge"} {
		t.Run(action, func(t *testing.T) {
			root := filepath.ToSlash(t.TempDir())
			newPath := filepath.ToSlash(t.TempDir())
			source := map[string]any{"id": 11, "name": "source", "root_path": root, "created_at": "2026-01-01T00:00:00Z"}
			target := map[string]any{"id": 22, "name": "target", "root_path": newPath, "created_at": "2026-01-01T00:00:00Z"}
			var calls []string
			NewMockDaemon(t, MockRefineHooks{
				OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					if !strings.HasPrefix(r.URL.Path, "/api/repos") {
						return false
					}
					calls = append(calls, r.Method+" "+r.URL.Path)
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/repos":
						require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"repos": []map[string]any{{"name": "source", "root_path": root, "count": 3}}, "total_count": 3}))
					case "/api/repos/detail":
						assert.Equal(t, "false", r.URL.Query().Get("by_path"))
						identifier := r.URL.Query().Get("identifier")
						repo := source
						if identifier == "target" {
							repo = target
						} else {
							assert.Equal(t, "source", identifier)
						}
						require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"repo": repo, "total_jobs": 3, "queued_jobs": 1, "completed_jobs": 2}))
					default:
						var body map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
						switch r.URL.Path {
						case "/api/repos/rename":
							assert.Equal(t, map[string]any{"identifier": "source", "name": "friendly", "by_path": false}, body)
							require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"repo": source}))
						case "/api/repos/move":
							assert.Equal(t, map[string]any{"repo_id": float64(11), "path": newPath, "identity": "local://" + filepath.FromSlash(newPath)}, body)
							w.WriteHeader(http.StatusNoContent)
						case "/api/repos/delete":
							assert.Equal(t, map[string]any{"repo_id": float64(11), "cascade": true}, body)
							w.WriteHeader(http.StatusNoContent)
						case "/api/repos/merge":
							assert.Equal(t, map[string]any{"source_id": float64(11), "target_id": float64(22)}, body)
							require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"moved": 3}))
						}
					}
					return true
				},
			})
			cases := map[string]struct {
				args   []string
				want   []string
				output string
			}{
				"list":   {[]string{"list"}, []string{"GET /api/repos"}, "Total: 1 repositories, 3 reviews"},
				"show":   {[]string{"show", "source"}, []string{"GET /api/repos/detail"}, "Jobs:       3 total"},
				"rename": {[]string{"rename", "source", "friendly"}, []string{"POST /api/repos/rename"}, `Renamed repository to "friendly"`},
				"move":   {[]string{"move", "source", newPath}, []string{"GET /api/repos/detail", "POST /api/repos/move"}, `Moved repository "source" to ` + newPath},
				"delete": {[]string{"delete", "--cascade", "--yes", "source"}, []string{"GET /api/repos/detail", "POST /api/repos/delete"}, `Deleted repository "source" and 3 jobs`},
				"merge":  {[]string{"merge", "--yes", "source", "target"}, []string{"GET /api/repos/detail", "GET /api/repos/detail", "POST /api/repos/merge"}, `Merged 3 jobs from "source" into "target"`},
			}
			tc := cases[action]
			command := repoCmd()
			command.SetArgs(tc.args)
			output := captureStdout(t, func() { require.NoError(t, command.Execute()) })
			assert.Equal(t, tc.want, calls)
			assert.Contains(t, output, tc.output)
		})
	}
}

func TestRepoCommandsCancelBeforeMutation(t *testing.T) {
	for _, action := range []string{"delete", "merge"} {
		t.Run(action, func(t *testing.T) {
			var mutations int
			NewMockDaemon(t, MockRefineHooks{
				OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					if !strings.HasPrefix(r.URL.Path, "/api/repos") {
						return false
					}
					if r.Method != http.MethodGet {
						mutations++
						return true
					}
					identifier := r.URL.Query().Get("identifier")
					id := 11
					if identifier == "target" {
						id = 22
					}
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"repo": map[string]any{"id": id, "name": identifier, "root_path": "/src/" + identifier, "created_at": "2026-01-01T00:00:00Z"}, "total_jobs": 3}))
					return true
				},
			})
			input, err := os.CreateTemp(t.TempDir(), "confirmation")
			require.NoError(t, err)
			t.Cleanup(func() { _ = input.Close() })
			_, err = input.WriteString("n\n")
			require.NoError(t, err)
			_, err = input.Seek(0, 0)
			require.NoError(t, err)
			original := os.Stdin
			os.Stdin = input
			t.Cleanup(func() { os.Stdin = original })
			command := repoCmd()
			args := []string{action, "source"}
			if action == "merge" {
				args = append(args, "target")
			}
			command.SetArgs(args)
			output := captureStdout(t, func() { require.NoError(t, command.Execute()) })
			assert.Contains(t, output, "Cancelled")
			assert.Zero(t, mutations)
		})
	}
}

func TestRepoCommandShowsDaemonError(t *testing.T) {
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/repos/detail" {
				return false
			}
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"status": 404, "detail": "repository not found: missing"}))
			return true
		},
	})
	command := repoShowCmd()
	command.SetArgs([]string{"missing"})
	err := command.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository not found: missing")
}

func TestRepoCommandsSendResolvedPathKind(t *testing.T) {
	for _, action := range []string{"show", "rename"} {
		t.Run(action, func(t *testing.T) {
			root := newTestGitRepo(t).Dir
			subdir := filepath.Join(root, "subdir")
			require.NoError(t, os.Mkdir(subdir, 0o755))
			chdir(t, subdir)
			var calls int
			NewMockDaemon(t, MockRefineHooks{
				OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					if !strings.HasPrefix(r.URL.Path, "/api/repos/") {
						return false
					}
					calls++
					if action == "show" {
						assert.Equal(t, "/api/repos/detail", r.URL.Path)
						assert.Equal(t, root, r.URL.Query().Get("identifier"))
						assert.Equal(t, "true", r.URL.Query().Get("by_path"))
					} else {
						assert.Equal(t, "/api/repos/rename", r.URL.Path)
						var body map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
						assert.Equal(t, map[string]any{"identifier": root, "name": "friendly", "by_path": true}, body)
					}
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"repo": map[string]any{"id": 11, "name": "friendly", "root_path": root, "created_at": "2026-01-01T00:00:00Z"}}))
					return true
				},
			})
			command := repoCmd()
			args := []string{action, "."}
			if action == "rename" {
				args = append(args, "friendly")
			}
			command.SetArgs(args)
			captureStdout(t, func() { require.NoError(t, command.Execute()) })
			assert.Equal(t, 1, calls)
		})
	}
}

func TestRepoCommandExplicitServerDoesNotManageLocalDaemon(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	origTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = origTransport })
	patchServerAddr(t, "http://127.0.0.1:7373")
	origGet, origStart, origRestart, origCleanup := getAnyRunningDaemon, startDaemonForEnsure, restartDaemonForEnsure, cleanupZombieDaemons
	t.Cleanup(func() {
		getAnyRunningDaemon, startDaemonForEnsure, restartDaemonForEnsure, cleanupZombieDaemons = origGet, origStart, origRestart, origCleanup
	})
	var localCalls int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { localCalls++; return nil, ErrDaemonNotRunning }
	startDaemonForEnsure = func() error { localCalls++; return nil }
	restartDaemonForEnsure = func() error { localCalls++; return nil }
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { localCalls++; return 0 }
	command := repoCmd()
	command.SetArgs([]string{"rename", "project-a", "friendly"})
	command.SilenceUsage = true
	require.Error(t, command.Execute())
	assert.Zero(t, localCalls)
	command = daemonCmd()
	command.SetArgs([]string{"start"})
	command.SilenceUsage = true
	require.Error(t, command.Execute(), "daemon start must check the selected endpoint")
	assert.Zero(t, localCalls)
}

func TestRepoCommandWaitsForMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/ping" {
				newMockRefineState().handlePing(w, r)
				return
			}
			time.Sleep(31 * time.Second)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"repo":{"id":1,"name":"friendly","root_path":"/repo","created_at":"2026-01-01T00:00:00Z"}}`)
		}))
		origTransport := http.DefaultTransport
		http.DefaultTransport = server.Client().Transport
		t.Cleanup(func() { http.DefaultTransport = origTransport })
		patchServerAddr(t, "http://127.0.0.1:7373")
		command := repoCmd()
		command.SetArgs([]string{"rename", "project-a", "friendly"})
		command.SilenceUsage = true
		require.NoError(t, command.Execute())
	})
}
