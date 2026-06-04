// ABOUTME: Compact job metadata handling for tracking source job IDs.
// ABOUTME: Used by worker to mark source jobs as closed when compact jobs complete.

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// CompactMetadata stores source job IDs for a compact job
type CompactMetadata struct {
	SourceJobIDs []int64 `json:"source_job_ids"`
}

// ReadCompactMetadata retrieves source job IDs for a compact job
func ReadCompactMetadata(jobID int64) (*CompactMetadata, error) {
	path := compactMetadataPath(jobID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read metadata file: %w", err)
	}

	var metadata CompactMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("parse metadata JSON: %w", err)
	}

	return &metadata, nil
}

// DeleteCompactMetadata removes the metadata file after processing
func DeleteCompactMetadata(jobID int64) error {
	path := compactMetadataPath(jobID)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete metadata file: %w", err)
	}
	return nil
}

// compactMetadataPath returns the file path for compact job metadata
func compactMetadataPath(jobID int64) string {
	return filepath.Join(config.DataDir(), fmt.Sprintf("compact-%d.json", jobID))
}

// IsValidCompactOutput checks whether compact agent output looks like
// a real response (vs. empty or an obvious error/stack trace).
// Intentionally permissive — we don't try to parse the review content.
func IsValidCompactOutput(output string) bool {
	output = strings.TrimSpace(output)
	if output == "" {
		return false
	}

	// Reject placeholder output from agents that ran but produced no
	// review content (auth errors, empty responses, etc.).
	if output == "No review output generated" {
		return false
	}

	// Reject obvious agent error patterns at line starts
	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.TrimSpace(strings.ToLower(line))
		if strings.HasPrefix(trimmed, "error:") ||
			strings.HasPrefix(trimmed, "exception:") ||
			strings.HasPrefix(trimmed, "traceback") {
			return false
		}
	}

	return !reportsRemainingFindingsWithoutDetails(output)
}

var (
	compactFileLinePattern        = regexp.MustCompile(`(?i)\b[\w./-]+\.(go|py|js|ts|tsx|jsx|java|rb|rs|c|cc|cpp|h|hpp|cs|php|swift|kt|m|mm|sql|yaml|yml|json|toml|md):\d+\b`)
	compactPositiveRemainingCount = regexp.MustCompile(`\b[1-9]\d* (?:verified )?findings? remains?\b`)
	compactFindingHeadingPattern  = regexp.MustCompile(`(?im)^#{1,6}\s*(review findings|verified findings|findings)\b`)
	compactSeverityHeadingPattern = regexp.MustCompile(`(?im)^#{1,6}\s*(?:\*\*)?\s*(critical|high|medium|low)\b`)
)

func reportsRemainingFindingsWithoutDetails(output string) bool {
	lower := strings.ToLower(output)
	if reportsNoRemainingFindings(lower) {
		return false
	}
	if !mentionsRemainingFindings(lower) {
		return false
	}
	return !hasActionableCompactFinding(output, lower)
}

func reportsNoRemainingFindings(lower string) bool {
	if compactPositiveRemainingCount.MatchString(lower) {
		return false
	}
	return slices.ContainsFunc(compactNormalizedStatements(lower), isNoRemainingCompactStatement)
}

func compactNormalizedStatements(lower string) []string {
	parts := strings.FieldsFunc(lower, func(r rune) bool {
		switch r {
		case '\n', '\r', '.', '!', '?', ',', ';', ':':
			return true
		default:
			return false
		}
	})

	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		statement := strings.Trim(part, "-*_`> \t")
		statement = strings.Join(strings.Fields(statement), " ")
		if statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}

func isNoRemainingCompactStatement(statement string) bool {
	switch statement {
	case "all previous findings have been addressed",
		"all findings have been resolved",
		"no issues found",
		"no verified findings remain",
		"no findings remain",
		"no remaining findings",
		"0 findings",
		"0 findings remain",
		"0 verified findings",
		"0 verified findings remain",
		"zero findings",
		"zero findings remain",
		"zero verified findings",
		"zero verified findings remain":
		return true
	default:
		return false
	}
}

func mentionsRemainingFindings(lower string) bool {
	remainingPhrases := []string{
		"findings remain",
		"finding remains",
		"verified findings",
		"verified finding",
		"verdict: fail",
	}
	for _, phrase := range remainingPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func hasActionableCompactFinding(output, lower string) bool {
	if hasStructuredCompactFinding(lower) {
		return true
	}

	return compactFindingHeadingPattern.MatchString(output) &&
		compactSeverityHeadingPattern.MatchString(output) &&
		compactFileLinePattern.MatchString(output)
}

func hasStructuredCompactFinding(lower string) bool {
	hasSeverity := strings.Contains(lower, "**severity**:")
	hasLocation := strings.Contains(lower, "**location**:") ||
		strings.Contains(lower, "**files**:")
	hasDetails := strings.Contains(lower, "**problem**:") ||
		strings.Contains(lower, "**issue**:") ||
		strings.Contains(lower, "**fix**:")
	return hasSeverity && hasLocation && hasDetails
}
