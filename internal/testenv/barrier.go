package testenv

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type fileSnapshot struct {
	name string
	path string
	size int64
}

type runtimeSnapshot struct {
	path   string
	exists bool
	size   int64
	mtime  time.Time
}

// ProdLogBarrier records the state of production log files before
// tests run, and provides a Check method that fails hard if any
// test activity leaked into production logs.
type ProdLogBarrier struct {
	pid         int
	prodDataDir string
	logs        []fileSnapshot
	runtime     runtimeSnapshot
}

// DefaultProdDataDir returns the default production data directory
// (~/.roborev). This is resolved from the user's home directory,
// ignoring ROBOREV_DATA_DIR so it always points to the real dir.
func DefaultProdDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".roborev"), nil
}

// NewProdLogBarrier snapshots the current production data directory
// state. Call Check() after m.Run() to detect test pollution.
// prodDataDir should be the default data directory (e.g. ~/.roborev)
// resolved BEFORE ROBOREV_DATA_DIR is overridden for tests.
func NewProdLogBarrier(prodDataDir string) *ProdLogBarrier {
	b := &ProdLogBarrier{
		pid:         os.Getpid(),
		prodDataDir: prodDataDir,
		logs: []fileSnapshot{
			newFileSnapshot("activity.log", filepath.Join(prodDataDir, "activity.log")),
			newFileSnapshot("errors.log", filepath.Join(prodDataDir, "errors.log")),
		},
	}
	b.runtime = newRuntimeSnapshot(prodDataDir, b.pid)
	return b
}

// Violations reports production data-dir writes caused by the current test
// process. The returned strings are stable enough for direct test assertions.
func (b *ProdLogBarrier) Violations() []string {
	violations := b.checkRuntimeFile()
	for _, log := range b.logs {
		violations = append(violations, b.checkLogFile(log)...)
	}
	return violations
}

// Check verifies no test pollution reached production files.
// Returns a non-empty error message if pollution is detected.
func (b *ProdLogBarrier) Check() string {
	return formatViolations(b.Violations())
}

func (b *ProdLogBarrier) checkRuntimeFile() []string {
	info, err := os.Stat(b.runtime.path)
	if err == nil {
		if !b.runtime.exists {
			return []string{
				fmt.Sprintf(
					"test wrote daemon.%d.json to prod runtime dir",
					b.pid,
				),
			}
		}
		if info.Size() != b.runtime.size || !info.ModTime().Equal(b.runtime.mtime) {
			return []string{
				fmt.Sprintf(
					"test modified daemon.%d.json in prod runtime dir"+
						" (size %d→%d, mtime %s→%s)",
					b.pid, b.runtime.size, info.Size(),
					b.runtime.mtime.Format(time.RFC3339Nano),
					info.ModTime().Format(time.RFC3339Nano),
				),
			}
		}
		return nil
	}
	if b.runtime.exists {
		return []string{
			fmt.Sprintf(
				"test deleted daemon.%d.json from prod runtime dir",
				b.pid,
			),
		}
	}
	return nil
}

func (b *ProdLogBarrier) checkLogFile(log fileSnapshot) []string {
	markers, err := scanNewLinesBestEffort(log.path, log.size, b.pid)
	if err != nil {
		markers = append(markers,
			fmt.Sprintf("scan error (barrier may be incomplete): %v", err))
	}
	if len(markers) == 0 {
		return nil
	}
	return []string{
		fmt.Sprintf(
			"test pollution in prod %s: %s",
			log.name,
			strings.Join(markers, "; "),
		),
	}
}

// scanNewLinesBestEffort reads appended lines and returns any detected test
// markers. Open and seek failures are intentionally ignored; scanner failures
// are returned so callers can report that the barrier result may be incomplete.
func scanNewLinesBestEffort(
	path string, startOffset int64, pid int,
) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()

	if _, err := f.Seek(startOffset, 0); err != nil {
		return nil, nil
	}

	pidStr := strconv.Itoa(pid)
	var markers []string
	seen := map[string]bool{}
	scanner := bufio.NewScanner(f)
	// 1MB buffer to handle large log lines without silent truncation.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		for _, m := range testMarkers(line, pidStr) {
			if !seen[m] {
				seen[m] = true
				markers = append(markers, m)
			}
		}
	}
	return markers, scanner.Err()
}

type markerRule struct {
	description func(pidStr string) string
	match       func(line, pidStr string) bool
}

var markerRules = []markerRule{
	{
		description: func(pidStr string) string {
			return "daemon.started with test PID " + pidStr
		},
		match: func(line, pidStr string) bool {
			return strings.Contains(line, `"pid":"`+pidStr+`"`) &&
				strings.Contains(line, "daemon.started")
		},
	},
	{
		description: func(string) string {
			return `event:"test" entry`
		},
		match: func(line, _ string) bool {
			return strings.Contains(line, `"event":"test"`)
		},
	},
	{
		description: func(string) string {
			return `daemon.started with version:"dev"`
		},
		match: func(line, _ string) bool {
			return strings.Contains(line, `"version":"dev"`) &&
				strings.Contains(line, "daemon.started")
		},
	},
}

// testMarkers returns marker descriptions if the line looks like
// test pollution. Checks for known patterns that only appear in
// test-generated log entries.
func testMarkers(line, pidStr string) []string {
	var out []string
	for _, rule := range markerRules {
		if rule.match(line, pidStr) {
			out = append(out, rule.description(pidStr))
		}
	}
	return out
}

func newFileSnapshot(name, path string) fileSnapshot {
	return fileSnapshot{
		name: name,
		path: path,
		size: fileSize(path),
	}
}

func newRuntimeSnapshot(prodDataDir string, pid int) runtimeSnapshot {
	snapshot := runtimeSnapshot{
		path: filepath.Join(
			prodDataDir, "runtime", fmt.Sprintf("daemon.%d.json", pid),
		),
	}
	if info, err := os.Stat(snapshot.path); err == nil {
		snapshot.exists = true
		snapshot.size = info.Size()
		snapshot.mtime = info.ModTime()
	}
	return snapshot
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func formatViolations(violations []string) string {
	if len(violations) == 0 {
		return ""
	}
	return "PROD LOG BARRIER FAILED:\n  " +
		strings.Join(violations, "\n  ")
}
