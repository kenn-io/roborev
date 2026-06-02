package lint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStringOrSlice_Array(t *testing.T) {
	var s StringOrSlice
	err := json.Unmarshal([]byte(`["CWE-94", "CWE-78"]`), &s)
	if err != nil {
		t.Fatalf("unmarshal array: %v", err)
	}
	if len(s) != 2 {
		t.Fatalf("expected 2 items, got %d", len(s))
	}
	if s[0] != "CWE-94" || s[1] != "CWE-78" {
		t.Fatalf("unexpected values: %v", s)
	}
}

func TestStringOrSlice_String(t *testing.T) {
	var s StringOrSlice
	err := json.Unmarshal([]byte(`"CWE-94: Improper Control"`), &s)
	if err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if len(s) != 1 {
		t.Fatalf("expected 1 item, got %d", len(s))
	}
	if s[0] != "CWE-94: Improper Control" {
		t.Fatalf("unexpected value: %s", s[0])
	}
}

func TestStringOrSlice_Empty(t *testing.T) {
	var s StringOrSlice
	err := json.Unmarshal([]byte(`[]`), &s)
	if err != nil {
		t.Fatalf("unmarshal empty array: %v", err)
	}
	if len(s) != 0 {
		t.Fatalf("expected 0 items, got %d", len(s))
	}
}

func TestStringOrSlice_Invalid(t *testing.T) {
	var s StringOrSlice
	err := json.Unmarshal([]byte(`123`), &s)
	if err == nil {
		t.Fatal("expected error for non-string/non-array input")
	}
}

func TestReport_ParseEmpty(t *testing.T) {
	raw := `{"results": [], "errors": []}`
	var report Report
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("parse empty report: %v", err)
	}
	if len(report.Results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(report.Results))
	}
	if report.HasFindings() {
		t.Fatal("expected no findings")
	}
}

func TestReport_ParseWithFindings(t *testing.T) {
	raw := `{
		"results": [
			{
				"check_id": "test.rule",
				"path": "main.go",
				"start": {"line": 10, "col": 1, "offset": 100},
				"end":   {"line": 10, "col": 5, "offset": 105},
				"severity": "ERROR",
				"extra": {
					"message": "test finding",
					"severity": "ERROR",
					"metadata": {
						"cwe": "CWE-94",
						"owasp": ["A03:2021", "A05:2025"],
						"category": "security",
						"technology": "go",
						"confidence": "HIGH",
						"vulnerability_class": ["code-injection"]
					}
				}
			},
			{
				"check_id": "test.warning",
				"path": "helper.go",
				"start": {"line": 20, "col": 1, "offset": 200},
				"end":   {"line": 20, "col": 5, "offset": 205},
				"severity": "WARNING",
				"extra": {
					"message": "test warning",
					"severity": "WARNING",
					"metadata": {
						"category": "maintainability",
						"confidence": "MEDIUM"
					}
				}
			}
		],
		"errors": []
	}`

	var report Report
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(report.Results))
	}
	if !report.HasFindings() {
		t.Fatal("expected findings")
	}

	// Verify first finding
	f := report.Results[0]
	if f.CheckID != "test.rule" {
		t.Fatalf("expected check_id 'test.rule', got '%s'", f.CheckID)
	}
	if f.Path != "main.go" {
		t.Fatalf("expected path 'main.go', got '%s'", f.Path)
	}
	if f.Start.Line != 10 {
		t.Fatalf("expected line 10, got %d", f.Start.Line)
	}
	if f.Severity != "ERROR" {
		t.Fatalf("expected ERROR, got %s", f.Severity)
	}
	if f.Extra.Message != "test finding" {
		t.Fatalf("unexpected message: %s", f.Extra.Message)
	}

	// Verify metadata with StringOrSlice
	meta := f.Extra.Metadata
	if len(meta.CWE) != 1 || meta.CWE[0] != "CWE-94" {
		t.Fatalf("unexpected CWE: %v", meta.CWE)
	}
	if len(meta.OWASP) != 2 {
		t.Fatalf("expected 2 OWASP entries, got %d", len(meta.OWASP))
	}
	if meta.Category != "security" {
		t.Fatalf("expected category 'security', got '%s'", meta.Category)
	}
	if meta.Confidence != "HIGH" {
		t.Fatalf("expected confidence 'HIGH', got '%s'", meta.Confidence)
	}
	if len(meta.VulnerabilityClass) != 1 || meta.VulnerabilityClass[0] != "code-injection" {
		t.Fatalf("unexpected vulnerability_class: %v", meta.VulnerabilityClass)
	}

	// Second finding has no cwe/owasp — should get empty slices
	f2 := report.Results[1]
	if len(f2.Extra.Metadata.CWE) != 0 {
		t.Fatalf("expected empty CWE, got %v", f2.Extra.Metadata.CWE)
	}
}

func TestReport_CountBySeverity(t *testing.T) {
	raw := `{
		"results": [
			{"check_id": "a", "path": "a.go", "start": {"line":1},"end":{"line":1}, "severity": "ERROR", "extra": {"message": "x", "severity": "ERROR"}},
			{"check_id": "b", "path": "b.go", "start": {"line":1},"end":{"line":1}, "severity": "ERROR", "extra": {"message": "x", "severity": "ERROR"}},
			{"check_id": "c", "path": "c.go", "start": {"line":1},"end":{"line":1}, "severity": "WARNING", "extra": {"message": "x", "severity": "WARNING"}},
			{"check_id": "d", "path": "d.go", "start": {"line":1},"end":{"line":1}, "severity": "INFO", "extra": {"message": "x", "severity": "INFO"}}
		],
		"errors": []
	}`

	var report Report
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("parse: %v", err)
	}

	counts := report.CountBySeverity()
	if counts["ERROR"] != 2 {
		t.Fatalf("expected 2 ERROR, got %d", counts["ERROR"])
	}
	if counts["WARNING"] != 1 {
		t.Fatalf("expected 1 WARNING, got %d", counts["WARNING"])
	}
	if counts["INFO"] != 1 {
		t.Fatalf("expected 1 INFO, got %d", counts["INFO"])
	}
}

func TestReport_FilterBySeverity(t *testing.T) {
	raw := `{
		"results": [
			{"check_id": "a", "path": "a.go", "start": {"line":1},"end":{"line":1}, "severity": "ERROR", "extra": {"message": "x", "severity": "ERROR"}},
			{"check_id": "b", "path": "b.go", "start": {"line":1},"end":{"line":1}, "severity": "WARNING", "extra": {"message": "x", "severity": "WARNING"}},
			{"check_id": "c", "path": "c.go", "start": {"line":1},"end":{"line":1}, "severity": "INFO", "extra": {"message": "x", "severity": "INFO"}}
		],
		"errors": []
	}`

	var report Report
	json.Unmarshal([]byte(raw), &report)

	filtered := report.FilterBySeverity([]string{"ERROR"})
	if len(filtered.Results) != 1 {
		t.Fatalf("expected 1 ERROR finding, got %d", len(filtered.Results))
	}
	if filtered.Results[0].CheckID != "a" {
		t.Fatalf("expected finding 'a', got '%s'", filtered.Results[0].CheckID)
	}

	// Case-insensitive
	filtered2 := report.FilterBySeverity([]string{"error", "warning"})
	if len(filtered2.Results) != 2 {
		t.Fatalf("expected 2 filtered findings, got %d", len(filtered2.Results))
	}

	// Empty filter returns all
	filtered3 := report.FilterBySeverity(nil)
	if len(filtered3.Results) != 3 {
		t.Fatalf("expected 3 findings with nil filter, got %d", len(filtered3.Results))
	}
}

func TestEventFilesJSON(t *testing.T) {
	// Verify that Semgrep output written by lint.Run can be round-tripped
	// We write a known-good JSON and parse it back.
	tmpDir := t.TempDir()
	jsonPath := filepath.Join(tmpDir, "semgrep.json")

	// Simulate what semgrep --json writes
	simulated := `{
		"results": [
			{
				"check_id": "go.lang.security.audit.dangerous-exec-command.dangerous-exec-command",
				"path": "cmd/roborev/comment.go",
				"start": {"line": 100, "col": 18, "offset": 2480},
				"end":   {"line": 100, "col": 54, "offset": 2516},
				"severity": "ERROR",
				"extra": {
					"message": "Detected non-static command inside Command.",
					"severity": "ERROR",
					"metadata": {
						"cwe": "CWE-94",
						"owasp": ["A03:2021 - Injection"],
						"category": "security",
						"confidence": "HIGH"
					}
				}
			},
			{
				"check_id": "go.best-practices.error-handling.error-handling",
				"path": "internal/daemon/hooks.go",
				"start": {"line": 307, "col": 13, "offset": 8942},
				"end":   {"line": 307, "col": 37, "offset": 8966},
				"severity": "WARNING",
				"extra": {
					"message": "Unhandled error.",
					"severity": "WARNING",
					"metadata": {
						"cwe": ["CWE-252", "CWE-391"],
						"owasp": "A10:2021",
						"category": "best-practice",
						"confidence": "MEDIUM"
					}
				}
			}
		],
		"errors": []
	}`

	if err := os.WriteFile(jsonPath, []byte(simulated), 0644); err != nil {
		t.Fatalf("write simulated output: %v", err)
	}

	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read simulated output: %v", err)
	}

	var report Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("unmarshal simulated output: %v", err)
	}

	if len(report.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(report.Results))
	}

	// First finding has string CWE — should parse as 1-element slice
	r1 := report.Results[0]
	if len(r1.Extra.Metadata.CWE) != 1 {
		t.Fatalf("expected 1 CWE (string input), got %d", len(r1.Extra.Metadata.CWE))
	}

	// Second finding has array CWE and string OWASP
	r2 := report.Results[1]
	if len(r2.Extra.Metadata.CWE) != 2 {
		t.Fatalf("expected 2 CWEs (array input), got %d", len(r2.Extra.Metadata.CWE))
	}
	if len(r2.Extra.Metadata.OWASP) != 1 {
		t.Fatalf("expected 1 OWASP (string input), got %d", len(r2.Extra.Metadata.OWASP))
	}
	if r2.Extra.Metadata.OWASP[0] != "A10:2021" {
		t.Fatalf("expected OWASP 'A10:2021', got '%s'", r2.Extra.Metadata.OWASP[0])
	}
}
