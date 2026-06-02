// Package mutate provides pluggable mutation testing backends.
// Supports Python (mutmut) with Go (ooze) and other languages
// addable via the Backend interface.
package mutate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Backend runs mutation testing for a specific language.
type Backend interface {
	// Name returns a human-readable name (e.g. "mutmut").
	Name() string

	// Available returns true if the backend's tool is installed.
	Available() bool

	// Run executes mutation testing and returns results.
	// If files is non-empty, only those files are mutated.
	Run(files []string) (*Result, error)
}

// Result holds the outcome of a mutation testing run.
type Result struct {
	Backend    string  `json:"backend"`
	Total      int     `json:"total"`
	Killed     int     `json:"killed"`
	Survived   int     `json:"survived"`
	Timeouted  int     `json:"timeouted"`
	Suspicious int     `json:"suspicious"`
	Score      float64 `json:"score"` // 0.0 to 1.0
	Output     string  `json:"output"`
}

// Detect returns the best available backend for the given directory.
// It checks file extensions to determine the primary language and
// picks the first available backend.
func Detect(dir string) Backend {
	// Check for JavaScript/TypeScript project
	if hasJSProject(dir) {
		js := &strykerBackend{}
		if js.Available() {
			return js
		}
	}

	// Check for Python project
	if hasPythonProject(dir) {
		py := &mutmutBackend{}
		if py.Available() {
			return py
		}
	}

	// Check for Go project
	if hasGoProject(dir) {
		goBackend := &goBackend{}
		if goBackend.Available() {
			return goBackend
		}
	}

	return nil
}

// DetectLanguage returns the detected primary language for the directory.
func DetectLanguage(dir string) string {
	if hasJSProject(dir) {
		return "javascript"
	}
	if hasPythonProject(dir) {
		return "python"
	}
	if hasGoProject(dir) {
		return "go"
	}
	return "unknown"
}

// AllAvailable returns all installed and usable backends.
func AllAvailable() []Backend {
	var backends []Backend

	js := &strykerBackend{}
	if js.Available() {
		backends = append(backends, js)
	}

	py := &mutmutBackend{}
	if py.Available() {
		backends = append(backends, py)
	}

	goBackend := &goBackend{}
	if goBackend.Available() {
		backends = append(backends, goBackend)
	}

	return backends
}

func hasPythonProject(dir string) bool {
	indicators := []string{
		"pyproject.toml", "setup.cfg", "setup.py",
		"requirements.txt", "Pipfile",
	}
	for _, f := range indicators {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	// Check for .py files
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".py") {
			return true
		}
	}
	return false
}

func hasGoProject(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return true
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// findExecutable looks for a command in PATH and common install locations.
func findExecutable(name string) string {
	// Check PATH first
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	// Check common install locations
	home, _ := os.UserHomeDir()
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		if home != "" {
			gopath = filepath.Join(home, "go")
		}
	}
	candidates := []string{
		filepath.Join(home, ".local/bin", name),
		filepath.Join(home, ".local/share/uv/python", name),
		filepath.Join(home, "Library/Python/3.13/bin", name),
	}
	if gopath != "" {
		candidates = append(candidates, filepath.Join(gopath, "bin", name))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}
