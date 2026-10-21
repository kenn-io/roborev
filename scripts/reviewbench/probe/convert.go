package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// pullRequest is ReviewBench's pr.json.
type pullRequest struct {
	Repo     string `json:"repo"`
	PRNumber int    `json:"pr_number"`
	Base     string `json:"base"`
	Head     string `json:"head"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

// findingsFile is ReviewBench's judging input format.
type findingsFile struct {
	PR       findingsPR `json:"pr"`
	Agent    string     `json:"agent"`
	Findings []finding  `json:"findings"`
	Usage    usage      `json:"usage"`
}

type findingsPR struct {
	Repo     string `json:"repo"`
	PRNumber int    `json:"pr_number"`
	Base     string `json:"base"`
	Head     string `json:"head"`
}

type finding struct {
	Producer  string `json:"producer"`
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Message   string `json:"message"`
}

type usage struct {
	TimeInMS int64 `json:"time_in_ms"`
}

// reviewFinding reads a finding from any stored roborev document version:
// version 3 has a file and lines, older versions only a location string.
type reviewFinding struct {
	Problem   string  `json:"problem"`
	Fix       string  `json:"fix"`
	File      *string `json:"file"`
	StartLine *int    `json:"start_line"`
	EndLine   *int    `json:"end_line"`
	Location  *string `json:"location"`
}

// convert turns a roborev review document into a ReviewBench findings file.
// It returns how many findings were dropped because they name no file: the
// judge only matches findings within the same file.
func convert(pr pullRequest, producer string, document []byte, changed map[string][][2]int, elapsed time.Duration) (findingsFile, int, error) {
	var doc struct {
		Findings []reviewFinding `json:"findings"`
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		return findingsFile{}, 0, fmt.Errorf("read review document: %w", err)
	}
	out := findingsFile{
		PR:       findingsPR{Repo: pr.Repo, PRNumber: pr.PRNumber, Base: pr.Base, Head: pr.Head},
		Agent:    producer,
		Findings: []finding{},
		Usage:    usage{TimeInMS: elapsed.Milliseconds()},
	}
	dropped := 0
	for _, f := range doc.Findings {
		file, start, end := locate(f)
		file = repoRelative(file)
		if file == "" {
			dropped++
			continue
		}
		if start <= 0 {
			// A finding about a whole file points at its first changed line,
			// where a reader of the diff would look first.
			start = 1
			if ranges := changed[file]; len(ranges) > 0 {
				start = ranges[0][0]
			}
		}
		if end < start {
			end = start
		}
		message := strings.TrimSpace(f.Problem)
		if fix := strings.TrimSpace(f.Fix); fix != "" {
			message += "\n\nFix: " + fix
		}
		out.Findings = append(out.Findings, finding{
			Producer: producer, File: file, StartLine: start, EndLine: end, Message: message,
		})
	}
	return out, dropped, nil
}

var locationPattern = regexp.MustCompile(`^(.+?):(\d+)(?:[-:](\d+))?\b`)

// locate returns a finding's file and line range. Older documents only have
// a free-text location such as "pkg/a.go:12" or "pkg/a.go:12-14".
func locate(f reviewFinding) (string, int, int) {
	if f.File != nil {
		start, end := 0, 0
		if f.StartLine != nil {
			start = *f.StartLine
		}
		if f.EndLine != nil {
			end = *f.EndLine
		}
		return *f.File, start, end
	}
	if f.Location == nil {
		return "", 0, 0
	}
	location := strings.Trim(strings.TrimSpace(*f.Location), "`")
	if m := locationPattern.FindStringSubmatch(location); m != nil {
		start, _ := strconv.Atoi(m[2])
		end := start
		if m[3] != "" {
			end, _ = strconv.Atoi(m[3])
		}
		return m[1], start, end
	}
	if strings.ContainsAny(location, " \t") {
		return "", 0, 0
	}
	return location, 0, 0
}

func repoRelative(file string) string {
	file = strings.ReplaceAll(strings.TrimSpace(file), `\`, "/")
	for strings.HasPrefix(file, "./") {
		file = file[2:]
	}
	file = strings.TrimLeft(file, "/")
	for _, prefix := range []string{"a/", "b/"} {
		file = strings.TrimPrefix(file, prefix)
	}
	return file
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// changedLines maps each file in a unified diff to the head-side line ranges
// of its hunks.
func changedLines(diff []byte) map[string][][2]int {
	changed := make(map[string][][2]int)
	var file string
	scanner := bufio.NewScanner(bytes.NewReader(diff))
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if name, ok := strings.CutPrefix(line, "+++ "); ok {
			file = ""
			if name != "/dev/null" {
				file = strings.TrimPrefix(name, "b/")
			}
			continue
		}
		if m := hunkHeader.FindStringSubmatch(line); m != nil && file != "" {
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			if count > 0 {
				changed[file] = append(changed[file], [2]int{start, start + count - 1})
			}
		}
	}
	return changed
}

func gitDiff(ctx context.Context, repo, base, head string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "diff", base+"..."+head)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s...%s: %w", base, head, err)
	}
	return out, nil
}
