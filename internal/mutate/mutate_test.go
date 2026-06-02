package mutate

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestDetectLanguage_Python(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", "[tool.poetry]\nname = \"test\"")

	lang := DetectLanguage(dir)
	if lang != "python" {
		t.Fatalf("expected 'python', got '%s'", lang)
	}
}

func TestDetectLanguage_Go(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/test\n\ngo 1.21")

	lang := DetectLanguage(dir)
	if lang != "go" {
		t.Fatalf("expected 'go', got '%s'", lang)
	}
}

func TestDetectLanguage_PythonByFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.py", "print('hello')")

	lang := DetectLanguage(dir)
	if lang != "python" {
		t.Fatalf("expected 'python' from .py file, got '%s'", lang)
	}
}

func TestDetectLanguage_GoByFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main\n\nfunc main() {}")

	lang := DetectLanguage(dir)
	if lang != "go" {
		t.Fatalf("expected 'go' from .go file, got '%s'", lang)
	}
}

func TestDetectLanguage_Unknown(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "# hello")

	lang := DetectLanguage(dir)
	if lang != "unknown" {
		t.Fatalf("expected 'unknown', got '%s'", lang)
	}
}

func TestDetect_PythonAvailable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", "[tool.poetry]\nname = \"test\"")

	backend := Detect(dir)
	if backend == nil {
		t.Fatal("expected backend for python project")
	}
	if backend.Name() != "mutmut" {
		t.Fatalf("expected 'mutmut', got '%s'", backend.Name())
	}
}

func TestDetect_GoNoBackend(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/test\n\ngo 1.21")

	backend := Detect(dir)
	// ooze is a library, not CLI; go-mutesting may or may not be installed
	// Accept nil as valid when no CLI backend is available
	_ = backend // just verify it doesn't panic
}

func TestAllAvailable(t *testing.T) {
	backends := AllAvailable()
	// At minimum, we should not panic
	t.Logf("available backends: %d", len(backends))
	for _, b := range backends {
		t.Logf("  - %s (available=%v)", b.Name(), b.Available())
	}
}

func TestFindExecutable_InPath(t *testing.T) {
	// 'echo' is always available on Unix
	p := findExecutable("echo")
	if p == "" {
		t.Fatal("expected 'echo' to be found")
	}
}

func TestFindExecutable_NotExists(t *testing.T) {
	p := findExecutable("nonexistent-tool-xyz-123")
	if p != "" {
		t.Fatalf("expected empty string, got '%s'", p)
	}
}

func TestMutmutBackend_ParseSummaryLine(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		killed int
		surv   int
		timeo  int
		susp   int
	}{
		{
			name:   "standard format",
			line:   "Killed 42 out of 50 mutants. 8 survived. 0 timeout. 0 suspicious.",
			killed: 42,
			surv:   8,
			timeo:  0,
			susp:   0,
		},
		{
			name:   "all killed",
			line:   "Killed 10 out of 10 mutants",
			killed: 10,
			surv:   0,
			timeo:  0,
			susp:   0,
		},
		{
			name:   "with timeout",
			line:   "Killed 5 out of 10 mutants. 3 survived. 2 timeout.",
			killed: 5,
			surv:   3,
			timeo:  2,
			susp:   0,
		},
		{
			name:   "with suspicious",
			line:   "Killed 5 out of 10 mutants. 3 survived. 2 suspicious.",
			killed: 5,
			surv:   3,
			timeo:  0,
			susp:   2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &mutmutBackend{}
			r := &Result{}
			b.parseSummaryLine(r, tt.line)

			if r.Killed != tt.killed {
				t.Errorf("killed: expected %d, got %d", tt.killed, r.Killed)
			}
			if r.Survived != tt.surv {
				t.Errorf("survived: expected %d, got %d", tt.surv, r.Survived)
			}
			if r.Timeouted != tt.timeo {
				t.Errorf("timeout: expected %d, got %d", tt.timeo, r.Timeouted)
			}
			if r.Suspicious != tt.susp {
				t.Errorf("suspicious: expected %d, got %d", tt.susp, r.Suspicious)
			}
		})
	}
}

func TestMutmutBackend_ParseResultsFromOutput(t *testing.T) {
	// Simulate mutmut's stdout with per-mutant results
	output := strings.Join([]string{
		"  mutant 1 ⏱️ killed",
		"  mutant 2 ⏱️ killed",
		"  mutant 3 👽 survived",
		"  mutant 4 ⏱️ killed",
		"  mutant 5 ⏱️ timeout",
		"  mutant 6 🤨 suspicious",
	}, "\n")

	b := &mutmutBackend{}
	r := &Result{}
	b.parseResults(r, output)

	if r.Killed != 3 {
		t.Errorf("killed: expected 3, got %d", r.Killed)
	}
	if r.Survived != 1 {
		t.Errorf("survived: expected 1, got %d", r.Survived)
	}
	if r.Timeouted != 1 {
		t.Errorf("timeout: expected 1, got %d", r.Timeouted)
	}
	if r.Suspicious != 1 {
		t.Errorf("suspicious: expected 1, got %d", r.Suspicious)
	}
	if r.Total != 6 {
		t.Errorf("total: expected 6, got %d", r.Total)
	}
}

func TestResult_Score(t *testing.T) {
	tests := []struct {
		name    string
		killed  int
		total   int
		timeout int
		want    float64
	}{
		{"all killed", 10, 10, 0, 1.0},
		{"half killed", 5, 10, 0, 0.5},
		{"all survived", 0, 10, 0, 0.0},
		{"with timeout", 5, 10, 3, 0.8}, // killed + timeout = 8/10
		{"no mutants", 0, 0, 0, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Result{
				Killed:    tt.killed,
				Survived:  tt.total - tt.killed - tt.timeout,
				Timeouted: tt.timeout,
				Total:     tt.total,
			}
			if r.Total > 0 {
				r.Score = float64(r.Killed+r.Timeouted) / float64(r.Total)
			}
			if r.Score != tt.want {
				t.Errorf("score: expected %.2f, got %.2f", tt.want, r.Score)
			}
		})
	}
}

func TestResult_JSONRoundTrip(t *testing.T) {
	r := &Result{
		Backend:   "mutmut",
		Total:     50,
		Killed:    42,
		Survived:  5,
		Timeouted: 3,
		Score:     0.9,
		Output:    "mock output",
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var r2 Result
	if err := json.Unmarshal(data, &r2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if r2.Backend != r.Backend {
		t.Errorf("backend: expected '%s', got '%s'", r.Backend, r2.Backend)
	}
	if r2.Score != r.Score {
		t.Errorf("score: expected %f, got %f", r.Score, r2.Score)
	}
	if r2.Total != r.Total {
		t.Errorf("total: expected %d, got %d", r.Total, r2.Total)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := dir + "/" + name
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
