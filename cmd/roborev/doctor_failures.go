package main

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"time"
)

func checkDoctorFailedJobs(env *doctorEnv) []doctorCheck {
	if !env.daemonUp() {
		return nil
	}
	cutoff := env.now.Add(-doctorRecentWindow)
	jobs, err := env.daemon.FailedJobs(env.ctx, cutoff)
	if err != nil {
		return []doctorCheck{{
			ID: "jobs.failed", Category: "jobs", Status: doctorWarn,
			Summary: "could not list failed jobs", Details: []string{err.Error()},
		}}
	}
	type group struct {
		count  int
		errors map[string]int
	}
	groups := map[string]*group{}
	total := 0
	for _, j := range jobs {
		if j.EnqueuedAt.Before(cutoff) {
			continue
		}
		total++
		name := j.Agent
		if name == "" {
			name = "(unknown agent)"
		}
		g := groups[name]
		if g == nil {
			g = &group{errors: map[string]int{}}
			groups[name] = g
		}
		g.count++
		g.errors[truncateString(doctorFirstLine(deref(j.ErrorData)), 200)]++
	}
	if total == 0 {
		return []doctorCheck{{
			ID: "jobs.failed", Category: "jobs", Status: doctorOK,
			Summary: "no failed jobs queued in the last 7 days",
		}}
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Slice(names, func(i, k int) bool {
		if groups[names[i]].count != groups[names[k]].count {
			return groups[names[i]].count > groups[names[k]].count
		}
		return names[i] < names[k]
	})
	status := doctorInfo
	var details []string
	for _, name := range names {
		g := groups[name]
		if g.count >= doctorRepeatedFailures {
			status = doctorWarn
		}
		msg, n := mostCommon(g.errors)
		details = append(details, fmt.Sprintf("%s: %s; most common (%dx): %s", name, plural(g.count, "failure"), n, msg))
	}
	return []doctorCheck{{
		ID: "jobs.failed", Category: "jobs", Status: status,
		Summary: fmt.Sprintf("%s queued in the last 7 days", plural(total, "failed job")),
		Details: details,
		Fix:     "inspect one with 'roborev show <job-id>'; repeated agent errors usually mean a missing agent, expired auth, or quota limits",
	}}
}

func mostCommon(counts map[string]int) (string, int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	best, bestN := "", 0
	for _, k := range keys {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	if best == "" {
		best = "(no error message)"
	}
	return best, bestN
}

type postCommitLogEntry struct {
	TS      string `json:"ts"`
	Repo    string `json:"repo"`
	Outcome string `json:"outcome"`
	Message string `json:"message"`
}

// checkDoctorEnqueueFailures reads the post-commit hook log. The hook never
// blocks a commit, so a hook that cannot queue reviews fails silently except
// for this log.
func checkDoctorEnqueueFailures(env *doctorEnv) []doctorCheck {
	f, err := os.Open(env.postCommitLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []doctorCheck{{
			ID: "jobs.enqueue_failures", Category: "jobs", Status: doctorWarn,
			Summary: "cannot read the post-commit hook log", Details: []string{err.Error()},
		}}
	}
	defer f.Close()

	cutoff := env.now.Add(-doctorRecentWindow)
	counts := map[string]int{}
	total := 0
	lastForRepo := ""
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var e postCommitLogEntry
			if json.Unmarshal(line, &e) == nil {
				ts, tsErr := time.Parse(time.RFC3339, e.TS)
				if tsErr == nil && !ts.Before(cutoff) {
					if env.repoPath != "" && e.Repo == env.repoPath {
						lastForRepo = e.Outcome
					}
					if e.Outcome == "fail" {
						total++
						counts[truncateString(doctorFirstLine(e.Message), 200)]++
					}
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return []doctorCheck{{
					ID: "jobs.enqueue_failures", Category: "jobs", Status: doctorWarn,
					Summary: "cannot read the post-commit hook log", Details: []string{readErr.Error()},
				}}
			}
			break
		}
	}

	if total == 0 {
		return []doctorCheck{{
			ID: "jobs.enqueue_failures", Category: "jobs", Status: doctorOK,
			Summary: "post-commit hook queued every review in the last 7 days",
		}}
	}
	status := doctorInfo
	if total >= doctorRepeatedFailures || lastForRepo == "fail" {
		status = doctorWarn
	}
	msg, n := mostCommon(counts)
	details := []string{fmt.Sprintf("most common (%dx): %s", n, msg)}
	if lastForRepo == "fail" {
		details = append(details, "the most recent commit in this repository was not queued")
	}
	return []doctorCheck{{
		ID: "jobs.enqueue_failures", Category: "jobs", Status: status,
		Summary: fmt.Sprintf("post-commit hook failed to queue %s in the last 7 days", plural(total, "review")),
		Details: details,
		Fix:     "see " + env.postCommitLog + "; failures usually mean the daemon was down or could not start",
	}}
}
