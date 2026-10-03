package main

import (
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/githook"
)

func checkDoctorRepo(env *doctorEnv) []doctorCheck {
	if env.repoPath == "" {
		return nil
	}
	var out []doctorCheck

	if env.daemonUp() {
		tracked, err := env.daemon.RepoTracked(env.ctx, env.repoPath)
		switch {
		case err != nil:
			out = append(out, doctorCheck{
				ID: "repo.registered", Category: "repo", Status: doctorWarn,
				Summary: "could not check whether the daemon knows this repository",
				Details: []string{err.Error()},
			})
		case tracked:
			out = append(out, doctorCheck{
				ID: "repo.registered", Category: "repo", Status: doctorOK,
				Summary: "repository is registered with the daemon",
			})
		default:
			out = append(out, doctorCheck{
				ID: "repo.registered", Category: "repo", Status: doctorInfo,
				Summary: "repository is not registered yet; it registers on its first review",
				Fix:     "roborev init",
			})
		}
	}

	if githook.NotInstalled(env.ctx, env.repoPath, "post-commit") {
		out = append(out, doctorCheck{
			ID: "repo.hooks", Category: "repo", Status: doctorWarn,
			Summary: "post-commit hook is not installed; commits are not reviewed automatically",
			Fix:     "roborev init",
		})
	} else {
		binary := ""
		if res, err := githook.ResolveRoborevPath(""); err == nil {
			binary = res.Path
		}
		var warnings []string
		for _, w := range daemon.ReadOnlyHookWarnings(env.ctx, env.repoPath, binary) {
			w = strings.TrimPrefix(w, "Warning: ")
			w = strings.Replace(w, " in "+env.repoPath, "", 1)
			w, _, _ = strings.Cut(w, " -- ")
			warnings = append(warnings, w)
		}
		if len(warnings) > 0 {
			out = append(out, doctorCheck{
				ID: "repo.hooks", Category: "repo", Status: doctorWarn,
				Summary: "git hooks need attention", Details: warnings,
				Fix: "roborev init",
			})
		} else {
			out = append(out, doctorCheck{
				ID: "repo.hooks", Category: "repo", Status: doctorOK,
				Summary: "git hooks are installed and current",
			})
		}
	}

	out = append(out, checkDoctorSnapshotDir(env)...)
	return out
}

// checkDoctorSnapshotDir checks the directory used to hand oversized diffs to
// sandboxed agents. Tracked files there block snapshot creation.
func checkDoctorSnapshotDir(env *doctorEnv) []doctorCheck {
	if env.repoErr != nil {
		return nil
	}
	dir, err := config.ResolveSnapshotDir(env.repoPath)
	if err != nil {
		return []doctorCheck{{
			ID: "repo.snapshot_dir", Category: "repo", Status: doctorFail,
			Summary: "snapshot_dir is invalid; reviews of large diffs will fail",
			Details: []string{err.Error()},
			Fix:     "set snapshot_dir in .roborev.toml to a relative path outside .git, or remove it",
		}}
	}
	err = git.ValidateRepoLocalPathNoSymlinks(env.repoPath, dir)
	if err == nil {
		err = git.EnsureNoTrackedFilesUnder(env.repoPath, dir)
	}
	if err != nil {
		return []doctorCheck{{
			ID: "repo.snapshot_dir", Category: "repo", Status: doctorFail,
			Summary: "reviews cannot write snapshots to snapshot_dir; reviews of large diffs will fail",
			Details: []string{err.Error()},
			Fix:     "point snapshot_dir at an unused directory inside the repository that is not a symlink and has no tracked files",
		}}
	}
	return nil
}
