package tui

import (
	"fmt"
	"strings"

	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
)

func shortRef(ref string) string {
	if before, after, ok := strings.Cut(ref, ".."); ok {
		return git.ShortSHA(before) + ".." + git.ShortSHA(after)
	}
	return git.ShortSHA(ref)
}

func shortJobRef(job storage.ReviewJob) string {
	if job.CommitID == nil && job.DiffContent == nil {
		if job.GitRef == "prompt" {
			return "run"
		}
		if !strings.Contains(job.GitRef, "..") {
			return job.GitRef
		}
	}
	return shortRef(job.GitRef)
}

func formatAgentLabel(agent, model string) string {
	if model != "" {
		return fmt.Sprintf("%s: %s", agent, model)
	}
	return agent
}
