package mutate

import (
	"testing"
)

func TestDetectLanguage_JavaScript(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name": "test"}`)

	lang := DetectLanguage(dir)
	if lang != "javascript" {
		t.Fatalf("expected 'javascript', got '%s'", lang)
	}
}

func TestDetectLanguage_JavaScriptByFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "app.js", "console.log('hello')")

	lang := DetectLanguage(dir)
	if lang != "javascript" {
		t.Fatalf("expected 'javascript' from .js file, got '%s'", lang)
	}
}

func TestDetectLanguage_TypeScriptByFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "app.ts", "const x: number = 1;")

	lang := DetectLanguage(dir)
	if lang != "javascript" {
		t.Fatalf("expected 'javascript' from .ts file, got '%s'", lang)
	}
}

func TestDetectLanguage_JSXByFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Component.jsx", "export default () => <div/>;")

	lang := DetectLanguage(dir)
	if lang != "javascript" {
		t.Fatalf("expected 'javascript' from .jsx file, got '%s'", lang)
	}
}

func TestDetectLanguage_TypeScriptConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "tsconfig.json", "{}")

	lang := DetectLanguage(dir)
	if lang != "javascript" {
		t.Fatalf("expected 'javascript' from tsconfig.json, got '%s'", lang)
	}
}

func TestDetectLanguage_MJSFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "module.mjs", "export default {};")

	lang := DetectLanguage(dir)
	if lang != "javascript" {
		t.Fatalf("expected 'javascript' from .mjs file, got '%s'", lang)
	}
}

func TestDetectLanguage_JSBeforePython(t *testing.T) {
	// When both JS and Python indicators exist, JS takes priority
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name": "test"}`)
	writeFile(t, dir, "pyproject.toml", "[tool.poetry]\nname = \"test\"")

	lang := DetectLanguage(dir)
	if lang != "javascript" {
		t.Fatalf("expected 'javascript' (priority over python), got '%s'", lang)
	}
}

func TestDetect_JavaScriptBackendAvailable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name": "test"}`)

	backend := Detect(dir)
	// Stryker may or may not be available (depends on npx + network)
	// Just verify it doesn't panic and returns the right type if available
	if backend != nil && backend.Name() != "stryker" {
		t.Fatalf("expected 'stryker' backend, got '%s'", backend.Name())
	}
}

func TestStrykerBackend_Name(t *testing.T) {
	b := &strykerBackend{}
	if b.Name() != "stryker" {
		t.Fatalf("expected 'stryker', got '%s'", b.Name())
	}
}

func TestStrykerBackend_ParseScoreLine(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		score float64
	}{
		{
			name:  "standard score",
			line:  "[Stryker] info    [Report] Mutation score: 85.71%",
			score: 0.8571,
		},
		{
			name:  "high score",
			line:  "[Stryker] info    [Report] Mutation score: 100.00%",
			score: 1.0,
		},
		{
			name:  "low score",
			line:  "[Stryker] info    [Report] Mutation score: 0.00%",
			score: 0.0,
		},
		{
			name:  "score without prefix",
			line:  "Mutation score: 72.50%",
			score: 0.725,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &strykerBackend{}
			r := &Result{}
			b.parseScoreLine(r, tt.line)

			// Allow small floating-point delta
			if r.Score < tt.score-0.001 || r.Score > tt.score+0.001 {
				t.Errorf("score: expected %.4f, got %.4f", tt.score, r.Score)
			}
		})
	}
}

func TestStrykerBackend_ParseMutantLine(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		total   int
		score   float64
	}{
		{
			name:  "standard tested line",
			line:  "[Stryker] info    [Mutant] 42/50 tested (84.00% score)",
			total: 50,
			score: 0.84,
		},
		{
			name:  "all tested",
			line:  "[Stryker] info    [Mutant] 100/100 tested (100.00% score)",
			total: 100,
			score: 1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &strykerBackend{}
			r := &Result{}
			b.parseMutantLine(r, tt.line)

			if r.Total != tt.total {
				t.Errorf("total: expected %d, got %d", tt.total, r.Total)
			}
			if r.Score < tt.score-0.001 || r.Score > tt.score+0.001 {
				t.Errorf("score: expected %.4f, got %.4f", tt.score, r.Score)
			}
		})
	}
}

func TestStrykerBackend_ParseCategoryLines(t *testing.T) {
	output := "Killed: 35\nSurvived: 7\nTimeout: 2\nNo coverage: 3\n"

	b := &strykerBackend{}
	r := &Result{}
	b.parseResults(r, output)

	if r.Killed != 35 {
		t.Errorf("killed: expected 35, got %d", r.Killed)
	}
	if r.Survived != 7 {
		t.Errorf("survived: expected 7, got %d", r.Survived)
	}
	if r.Timeouted != 2 {
		t.Errorf("timeout: expected 2, got %d", r.Timeouted)
	}
	if r.Suspicious != 3 {
		t.Errorf("suspicious: expected 3 (no coverage), got %d", r.Suspicious)
	}
	if r.Total != 47 {
		t.Errorf("total: expected 47, got %d", r.Total)
	}
	expectedScore := float64(35+2) / 47.0
	if r.Score < expectedScore-0.01 || r.Score > expectedScore+0.01 {
		t.Errorf("score: expected ~%.4f, got %.4f", expectedScore, r.Score)
	}
}

func TestStrykerBackend_ParseFullOutput(t *testing.T) {
	// Simulate realistic Stryker output with score line + category lines
	output := `[Stryker] info    [Mutant] 42/50 tested (84.00% score)
[Stryker] info    [Report] Mutation score: 84.00%
Killed: 38
Survived: 8
Timeout: 4
No coverage: 0
[Stryker] info    Done in 2 minutes 30 seconds.`

	b := &strykerBackend{}
	r := &Result{}
	b.parseResults(r, output)

	// Score line takes priority (parsed first, sets score)
	if r.Score < 0.839 || r.Score > 0.841 {
		t.Errorf("score: expected ~0.84, got %.4f", r.Score)
	}
	// Category lines should set individual counts
	if r.Killed != 38 {
		t.Errorf("killed: expected 38, got %d", r.Killed)
	}
	if r.Survived != 8 {
		t.Errorf("survived: expected 8, got %d", r.Survived)
	}
	if r.Timeouted != 4 {
		t.Errorf("timeout: expected 4, got %d", r.Timeouted)
	}
	if r.Total != 50 {
		t.Errorf("total: expected 50, got %d", r.Total)
	}
}
