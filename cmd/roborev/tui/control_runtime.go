package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// TUIRuntimeInfo stores metadata about a running TUI instance for
// discoverability by external tools. Filter state is intentionally
// omitted — it changes at runtime and should be queried via the
// control socket's get-filters command.
type TUIRuntimeInfo struct {
	PID        int    `json:"pid"`
	SocketPath string `json:"socket_path"`
	ServerAddr string `json:"server_addr"`
}

// tuiRuntimePath returns the metadata file path for a given PID.
func tuiRuntimePath(pid int) string {
	return filepath.Join(
		config.DataDir(), fmt.Sprintf("tui.%d.json", pid),
	)
}

// WriteTUIRuntime writes the TUI runtime metadata file atomically.
func WriteTUIRuntime(info TUIRuntimeInfo) error {
	path := tuiRuntimePath(info.PID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(dir, "tui.*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}

	success = true
	return nil
}

// RemoveTUIRuntime removes the runtime metadata file for the
// current process. The socket file is removed separately by the
// control listener's cleanup function.
func RemoveTUIRuntime() {
	os.Remove(tuiRuntimePath(os.Getpid()))
}

// ListAllTUIRuntimes returns metadata for all discovered TUI
// runtime files.
func ListAllTUIRuntimes() ([]*TUIRuntimeInfo, error) {
	dataDir := config.DataDir()
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var runtimes []*TUIRuntimeInfo
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "tui.") ||
			!strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dataDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info TUIRuntimeInfo
		if err := json.Unmarshal(data, &info); err != nil {
			os.Remove(path)
			continue
		}
		if info.PID <= 0 || info.SocketPath == "" {
			os.Remove(path)
			continue
		}
		runtimes = append(runtimes, &info)
	}
	return runtimes, nil
}

// CleanupStaleTUIRuntimes removes socket and metadata files for
// TUI processes that are no longer running.
func CleanupStaleTUIRuntimes() int {
	runtimes, err := ListAllTUIRuntimes()
	if err != nil {
		return 0
	}

	cleaned := 0
	for _, info := range runtimes {
		if isProcessAlive(info.PID) {
			continue
		}
		// Only remove the socket if it's actually a stale
		// socket file. This prevents deleting a live socket
		// at a shared custom path or a non-socket file.
		// Errors are ignored: the metadata is still cleaned
		// since the owning PID is dead.
		_ = removeStaleSocket(info.SocketPath)
		os.Remove(tuiRuntimePath(info.PID))
		cleaned++
	}
	return cleaned
}

// buildTUIRuntimeInfo constructs the runtime metadata for the
// current process. This is the single source of truth for what
// gets written to the runtime metadata file.
func buildTUIRuntimeInfo(
	socketPath, serverAddr string,
) TUIRuntimeInfo {
	return TUIRuntimeInfo{
		PID:        os.Getpid(),
		SocketPath: socketPath,
		ServerAddr: serverAddr,
	}
}

// defaultControlSocketPath returns the default socket path for the
// current process: {DataDir}/tui.{PID}.sock
func defaultControlSocketPath() string {
	return filepath.Join(
		config.DataDir(),
		fmt.Sprintf("tui.%d.sock", os.Getpid()),
	)
}
