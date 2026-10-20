package review

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// settleFindings checks a live document's finding locations against the
// reviewed code, merges duplicates, and ranks the rest. It runs on every
// agent review and synthesis before the document is stored.
func settleFindings(ctx context.Context, repoPath, gitRef string, doc *structuredreview.Document) {
	if repoPath != "" {
		anchorFindings(reviewedFiles(ctx, repoPath, gitRef), repoPath, doc.Findings)
	}
	doc.Findings = tidyFindings(doc.Findings)
}

// fileReader returns a file's contents as the reviewed code has it. A path
// that names no file returns an error wrapping fs.ErrNotExist.
type fileReader func(path string) ([]byte, error)

// reviewedFiles reads files from the commit a review covered: the end of a
// range, the commit itself, or the working tree for uncommitted changes and
// refs that name no commit, such as a compact job's label.
func reviewedFiles(ctx context.Context, repoPath, gitRef string) fileReader {
	rev := gitRef
	if i := strings.LastIndex(rev, ".."); i >= 0 {
		rev = strings.TrimPrefix(rev[i+2:], ".")
	}
	if rev != "" && rev != "dirty" {
		if sha, err := git.ResolveSHACtx(ctx, repoPath, rev+"^{commit}"); err == nil {
			return func(name string) ([]byte, error) {
				file, err := git.ReadRefFile(repoPath, sha, name)
				if errors.Is(err, git.ErrNotFile) {
					return nil, fs.ErrNotExist
				}
				return file.Data, err
			}
		}
	}
	return func(name string) ([]byte, error) {
		root, err := os.OpenRoot(repoPath)
		if err != nil {
			// Without the checkout nothing can be checked, which is not
			// evidence that the cited file is missing.
			return nil, errors.New("open checkout: " + err.Error())
		}
		defer root.Close()
		data, err := root.ReadFile(name)
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) && !errors.Is(err, fs.ErrPermission) {
			// Missing paths, directories, and paths that escape the
			// checkout all mean the finding names no file in it.
			return nil, fs.ErrNotExist
		}
		return data, err
	}
}

// anchorFindings checks each finding's file and line range against the
// reviewed code. A path is matched after removing prefixes agents often add:
// the checkout's absolute path, "./", and the diff's "a/" and "b/". A finding
// whose file does not exist keeps its text but loses its location. A start
// line past the end of the file drops the range, and an end line past it is
// clamped. Findings are left as they are when a file cannot be read for any
// other reason.
func anchorFindings(read fileReader, repoPath string, findings []structuredreview.Finding) {
	for i := range findings {
		finding := &findings[i]
		if finding.Kind == "" || finding.File == "" {
			continue
		}
		cited := finding.File
		name, lines, err := readCitedFile(read, repoPath, cited)
		if errors.Is(err, fs.ErrNotExist) {
			log.Printf("review: finding cites %q, which is not a file in the reviewed code; dropping its location", cited)
			finding.SetLocation("", 0, 0)
			continue
		}
		if err != nil {
			log.Printf("review: cannot check the location of a finding in %q: %v", cited, err)
			continue
		}
		start, end := finding.StartLine, finding.EndLine
		if start > lines {
			start, end = 0, 0
		}
		finding.SetLocation(name, start, min(end, lines))
	}
}

// readCitedFile resolves a cited path to a file in the reviewed code and
// returns its repository-relative path and line count.
func readCitedFile(read fileReader, repoPath, cited string) (string, int, error) {
	lastErr := fs.ErrNotExist
	for _, name := range pathSpellings(repoPath, cited) {
		data, err := read(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}
		return name, countLines(data), nil
	}
	return "", 0, lastErr
}

// pathSpellings returns the repository-relative spellings worth trying for a
// cited path, or none when the path climbs out of the repository.
func pathSpellings(repoPath, cited string) []string {
	name := strings.ReplaceAll(strings.TrimSpace(cited), `\`, "/")
	if repoPath != "" {
		prefix := strings.TrimSuffix(filepath.ToSlash(repoPath), "/") + "/"
		name = strings.TrimPrefix(name, prefix)
	}
	for strings.HasPrefix(name, "./") {
		name = name[2:]
	}
	name = strings.TrimLeft(name, "/")
	if name == "" || slices.Contains(strings.Split(name, "/"), "..") {
		return nil
	}
	name = path.Clean(name)
	spellings := []string{name}
	if trimmed, ok := strings.CutPrefix(name, "a/"); ok {
		spellings = append(spellings, trimmed)
	} else if trimmed, ok := strings.CutPrefix(name, "b/"); ok {
		spellings = append(spellings, trimmed)
	}
	return spellings
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

// nearbyLines is how far apart two line ranges may be and still describe the
// same place, since reviewers often cite a line or two above or below.
const nearbyLines = 3

// duplicateSimilarity is the share of words two problem descriptions must
// have in common to count as the same problem.
const duplicateSimilarity = 0.45

// tidyFindings merges findings that describe the same problem at the same
// place and orders the rest: most severe first, then those more reviewers
// reported, then the more confident. Findings without a file are never
// merged, because nothing ties them to one place. A merged finding keeps
// the more severe, more confident, or more detailed text and every source.
func tidyFindings(findings []structuredreview.Finding) []structuredreview.Finding {
	type keptFinding struct {
		finding structuredreview.Finding
		words   map[string]struct{}
	}
	var kept []keptFinding
	for _, finding := range findings {
		words := problemWords(finding.Problem)
		twin := slices.IndexFunc(kept, func(other keptFinding) bool {
			return finding.File != "" && samePlace(finding, other.finding) &&
				similarity(words, other.words) >= duplicateSimilarity
		})
		if twin < 0 {
			kept = append(kept, keptFinding{finding, words})
			continue
		}
		sources := mergeSources(kept[twin].finding.Sources, finding.Sources)
		if better(finding, kept[twin].finding) {
			kept[twin] = keptFinding{finding, words}
		}
		kept[twin].finding.Sources = sources
	}
	result := make([]structuredreview.Finding, len(kept))
	for i, k := range kept {
		result[i] = k.finding
	}
	slices.SortStableFunc(result, func(a, b structuredreview.Finding) int {
		return cmp.Or(
			cmp.Compare(config.SeverityRank(b.Severity), config.SeverityRank(a.Severity)),
			cmp.Compare(len(b.Sources), len(a.Sources)),
			cmp.Compare(b.Confidence, a.Confidence),
		)
	})
	return result
}

func samePlace(a, b structuredreview.Finding) bool {
	if a.File != b.File {
		return false
	}
	if a.StartLine == 0 || b.StartLine == 0 {
		return a.StartLine == b.StartLine
	}
	return a.StartLine <= b.EndLine+nearbyLines && b.StartLine <= a.EndLine+nearbyLines
}

func better(a, b structuredreview.Finding) bool {
	if rankA, rankB := config.SeverityRank(a.Severity), config.SeverityRank(b.Severity); rankA != rankB {
		return rankA > rankB
	}
	if a.Confidence != b.Confidence {
		return a.Confidence > b.Confidence
	}
	return len(a.Problem) > len(b.Problem)
}

func mergeSources(a, b []int) []int {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	merged := slices.Concat(a, b)
	slices.Sort(merged)
	return slices.Compact(merged)
}

var wordPattern = regexp.MustCompile(`[a-z0-9_]{3,}`)

func problemWords(text string) map[string]struct{} {
	words := make(map[string]struct{})
	for _, word := range wordPattern.FindAllString(strings.ToLower(text), -1) {
		words[word] = struct{}{}
	}
	return words
}

// similarity is the Jaccard index of two word sets, from 0 to 1.
func similarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for word := range a {
		if _, ok := b[word]; ok {
			shared++
		}
	}
	return float64(shared) / float64(len(a)+len(b)-shared)
}
