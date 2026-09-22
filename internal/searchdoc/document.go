// Package searchdoc projects canonical review data into deterministic search
// documents shared by the lexical and semantic indexes.
package searchdoc

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// RecipeVersion changes whenever semantic content rendering changes.
const RecipeVersion = 2

// Document is the deterministic projection stored in the search sidecar.
type Document struct {
	DocKey      string
	GroupKey    string
	Content     string
	ContentHash string
	Identifiers string
	Source      storage.SearchReviewSource
}

type structuredDocument struct {
	Legacy   *structuredreview.LegacyDocument `json:"legacy"`
	Summary  string                           `json:"summary"`
	Findings []structuredFinding              `json:"findings"`
}

type structuredFinding struct {
	Severity string  `json:"severity"`
	Location *string `json:"location"`
	Problem  string  `json:"problem"`
	Fix      string  `json:"fix"`
}

// Render produces the exact semantic and lexical content for one canonical
// review source.
func Render(source storage.SearchReviewSource) Document {
	content := renderContent(source)
	hash := sha256.Sum256([]byte(content))
	return Document{
		DocKey:      documentKey(source),
		GroupKey:    groupKey(source),
		Content:     content,
		ContentHash: fmt.Sprintf("%x", hash),
		Identifiers: renderIdentifiers(source),
		Source:      source,
	}
}

func documentKey(source storage.SearchReviewSource) string {
	if source.ReviewUUID != "" {
		return source.ReviewUUID
	}
	return "local:" + strconv.FormatInt(source.ReviewID, 10)
}

func groupKey(source storage.SearchReviewSource) string {
	if source.PanelRunUUID != "" {
		return source.PanelRunUUID
	}
	return source.JobUUID
}

func renderContent(source storage.SearchReviewSource) string {
	lines := make([]string, 0, 8+len(source.Responses))
	appendField := func(label string, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			lines = append(lines, label+": "+value)
		}
	}

	appendField("Commit", source.CommitSubject)
	appendField("Review type", source.ReviewType)
	appendField("Verdict", source.Verdict)

	if structured, ok := decodeStructured(source.StructuredOutput); ok {
		if structured.Legacy != nil {
			appendField("Review", structured.Legacy.Markdown)
		}
		appendField("Summary", structured.Summary)
		for _, finding := range structured.Findings {
			findingLine := strings.TrimSpace(finding.Severity)
			if finding.Location != nil && strings.TrimSpace(*finding.Location) != "" {
				findingLine += " " + strings.TrimSpace(*finding.Location)
			}
			appendField("Finding", findingLine)
			appendField("Problem", finding.Problem)
			appendField("Fix", finding.Fix)
		}
	} else {
		appendField("Review", source.Output)
	}

	responses := slices.Clone(source.Responses)
	slices.SortFunc(responses, storage.CompareResponses)
	for _, response := range responses {
		responder := strings.TrimSpace(response.Responder)
		body := strings.TrimSpace(response.Response)
		if body == "" {
			continue
		}
		if responder == "" {
			lines = append(lines, "Response: "+body)
			continue
		}
		lines = append(lines, "Response by "+responder+": "+body)
	}

	return strings.Join(lines, "\n")
}

func decodeStructured(output storage.StructuredOutput) (structuredDocument, bool) {
	if len(output) == 0 {
		return structuredDocument{}, false
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return structuredDocument{}, false
	}
	var document structuredDocument
	if err := json.Unmarshal(raw, &document); err != nil || (document.Legacy == nil && strings.TrimSpace(document.Summary) == "") {
		return structuredDocument{}, false
	}
	return document, true
}

func renderIdentifiers(source storage.SearchReviewSource) string {
	values := []string{
		source.RepoName,
		source.Branch,
		source.GitRef,
		source.CommitSHA,
		source.JobUUID,
		source.ReviewUUID,
		source.ReviewType,
		source.Agent,
	}
	identifiers := values[:0]
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			identifiers = append(identifiers, value)
		}
	}
	return strings.Join(identifiers, "\n")
}
