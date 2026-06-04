package agenthook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/procutil"
	"go.kenn.io/roborev/internal/version"
)

func Post(ctx context.Context, req Request) (Response, error) {
	client, baseURL, err := EnsureClient(ctx)
	if err != nil {
		return Response{}, err
	}
	body := strings.NewReader(mustJSON(req))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/hook", body)
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return Response{}, fmt.Errorf("agent hook daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Response{}, err
	}
	return out, nil
}

func RunStatus(stdout io.Writer) error {
	client, baseURL, err := EnsureClient(context.Background())
	if err != nil {
		return err
	}
	resp, err := client.Get(baseURL + "/api/sessions")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent hook daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, err = io.Copy(stdout, resp.Body)
	return err
}

func RunReset(opts ResetOptions, sessionID string, stdout io.Writer) error {
	if !opts.All && sessionID == "" {
		return fmt.Errorf("reset requires a session id or --all")
	}
	client, baseURL, err := EnsureClient(context.Background())
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"all":        opts.All,
		"session_id": sessionID,
	})
	if err != nil {
		return err
	}
	resp, err := client.Post(baseURL+"/api/reset", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent hook daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, err = io.Copy(stdout, resp.Body)
	return err
}

func EnsureClient(ctx context.Context) (*http.Client, string, error) {
	rec, _, err := agentHookDaemonManager().Ensure(ctx, 5*time.Second)
	if err != nil {
		return nil, "", err
	}
	ep := rec.Endpoint()
	return ep.HTTPClient(kitdaemon.HTTPClientOptions{
		Timeout:               5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		DisableKeepAlives:     true,
	}), ep.BaseURL(), nil
}

func agentHookDaemonManager() kitdaemon.Manager {
	return kitdaemon.Manager{
		Store: runtimeStore(),
		Discover: kitdaemon.DiscoverOptions{
			RequirePIDAlive: true,
			Probe: kitdaemon.ProbeOptions{
				ExpectedService: ServiceName,
				Timeout:         750 * time.Millisecond,
			},
		},
		Compatible: func(_ kitdaemon.RuntimeRecord, info kitdaemon.PingInfo) bool {
			return info.Service == ServiceName && info.Version == version.Version
		},
		Start: startReplacingDaemon,
	}
}

// startReplacingDaemon stops any running agent hook daemon and starts a fresh
// one from the caller's binary. The manager invokes it only after discovery
// fails to find a compatible daemon, so a daemon left over from an older
// roborev binary is replaced rather than left to keep serving stale clients.
func startReplacingDaemon(ctx context.Context) error {
	if _, err := stopLiveDaemons(ctx); err != nil {
		return err
	}
	exe, err := restartExecutable()
	if err != nil {
		return err
	}
	return startDetachedDaemonExecutable(ctx, exe)
}

// restartExecutable resolves the binary that invoked this process so that
// `daemon start`/`daemon restart` launch the caller's binary rather than the
// one a currently-running daemon was started from.
func restartExecutable() (string, error) {
	arg0 := os.Args[0]
	if arg0 == "" {
		return os.Executable()
	}
	if filepath.Base(arg0) == arg0 {
		return exec.LookPath(arg0)
	}
	return filepath.Abs(arg0)
}

func startDetachedDaemonExecutable(ctx context.Context, exe string) error {
	logPath, err := DaemonLogPath()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open agent hook daemon log: %w", err)
	}
	defer logFile.Close()

	return kitdaemon.StartDetached(ctx, kitdaemon.StartDetachedOptions{
		Executable:      exe,
		Args:            []string{"agent-hook", "daemon", "run"},
		Env:             procutil.FilterGitEnv(os.Environ()),
		Stdout:          logFile,
		Stderr:          logFile,
		RefuseEphemeral: true,
	})
}

func WriteRuntime(ep kitdaemon.Endpoint) (string, error) {
	return runtimeStore().Write(kitdaemon.NewRuntimeRecord(ServiceName, version.Version, ep))
}

func ListRuntimes() ([]kitdaemon.RuntimeRecord, error) {
	return runtimeStore().List()
}

func ProbeDaemon(ep kitdaemon.Endpoint) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	info, err := kitdaemon.Probe(ctx, ep, kitdaemon.ProbeOptions{
		ExpectedService: ServiceName,
		Timeout:         750 * time.Millisecond,
	})
	return err == nil && info.Service == ServiceName
}

func DefaultDaemonAddress() string {
	if raw := os.Getenv(DaemonAddrEnv); raw != "" {
		return raw
	}
	if runtime.GOOS == "windows" {
		return "127.0.0.1:0"
	}
	if path := kitdaemon.DefaultSocketPath(ServiceName); path != "" {
		return "unix://" + path
	}
	return "127.0.0.1:0"
}

func RuntimeDir() string {
	return filepath.Join(config.DataDir(), "agent-hook", "runtime")
}

func RuntimePath(pid int) string {
	path, err := runtimeStore().Path(pid)
	if err != nil {
		return filepath.Join(RuntimeDir(), fmt.Sprintf("daemon.%d.json", pid))
	}
	return path
}

func runtimeStore() kitdaemon.RuntimeStore {
	return kitdaemon.RuntimeStore{
		Dir:    RuntimeDir(),
		Prefix: "daemon",
	}
}

func DaemonLogPath() (string, error) {
	dir := filepath.Join(config.DataDir(), "agent-hook")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
}

func mustJSON(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(body)
}
