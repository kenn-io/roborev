package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HistoryCommit is one commit returned by PathLog or BlameLines.
type HistoryCommit struct {
	SHA     string
	Author  string
	Date    string // YYYY-MM-DD, author date
	Subject string
}

// PathLog lists the commits reachable from rev that changed path, newest
// first. A positive startLine limits the history to the lines
// startLine..endLine of path as it exists at rev. maxCount <= 0 returns every
// commit. Pathspec magic is disabled, so path names one file literally.
func PathLog(
	ctx context.Context, repoPath, rev, path string, startLine, endLine, maxCount int,
) ([]HistoryCommit, error) {
	args := []string{
		"--literal-pathspecs", "log", "--no-patch", "--date=short",
		"--format=%H%x00%an%x00%ad%x00%s",
	}
	if maxCount > 0 {
		args = append(args, "-n", strconv.Itoa(maxCount))
	}
	if startLine > 0 {
		args = append(args, fmt.Sprintf("-L%d,%d:%s", startLine, endLine, path), rev)
	} else {
		args = append(args, rev, "--", path)
	}
	cmd := newGitCmdContext(ctx, args...)
	cmd.Dir = repoPath
	out, err := gitOutput(cmd)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	var commits []HistoryCommit
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimRight(line, "\r\n"), "\x00")
		if len(fields) != 4 {
			continue
		}
		commits = append(commits, HistoryCommit{
			SHA: fields[0], Author: fields[1], Date: fields[2], Subject: fields[3],
		})
	}
	return commits, nil
}

// BlameHunk is a run of consecutive lines last changed by one commit.
type BlameHunk struct {
	StartLine int
	EndLine   int
	Commit    HistoryCommit
}

// BlameLines blames lines startLine..endLine of path as it exists at rev and
// merges consecutive lines from the same commit into one hunk.
func BlameLines(
	ctx context.Context, repoPath, rev, path string, startLine, endLine int,
) ([]BlameHunk, error) {
	cmd := newGitCmdContext(ctx,
		"--literal-pathspecs", "blame", "--porcelain",
		"-L", fmt.Sprintf("%d,%d", startLine, endLine), rev, "--", path,
	)
	cmd.Dir = repoPath
	out, err := gitOutput(cmd)
	if err != nil {
		return nil, fmt.Errorf("git blame: %w", err)
	}
	return parseBlamePorcelain(out), nil
}

// parseBlamePorcelain reads `git blame --porcelain`. Commit metadata appears
// only the first time a commit is seen, so it is cached by SHA.
func parseBlamePorcelain(out []byte) []BlameHunk {
	commits := make(map[string]*HistoryCommit)
	var hunks []BlameHunk
	var current *HistoryCommit
	var line int
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(nil, len(out)+1)
	for scanner.Scan() {
		text := scanner.Text()
		if strings.HasPrefix(text, "\t") {
			if current == nil {
				continue
			}
			if n := len(hunks); n > 0 && hunks[n-1].Commit.SHA == current.SHA && hunks[n-1].EndLine == line-1 {
				hunks[n-1].EndLine = line
			} else {
				hunks = append(hunks, BlameHunk{StartLine: line, EndLine: line, Commit: *current})
			}
			continue
		}
		key, value, _ := strings.Cut(text, " ")
		if current == nil || isFullObjectID(key) {
			fields := strings.Fields(value)
			if !isFullObjectID(key) || len(fields) < 2 {
				continue
			}
			line, _ = strconv.Atoi(fields[1])
			if commits[key] == nil {
				commits[key] = &HistoryCommit{SHA: key}
			}
			current = commits[key]
			continue
		}
		switch key {
		case "author":
			current.Author = value
		case "author-time":
			if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
				current.Date = time.Unix(seconds, 0).UTC().Format(time.DateOnly)
			}
		case "summary":
			current.Subject = value
		}
	}
	return hunks
}
