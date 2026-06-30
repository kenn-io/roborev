package git

// Benchmark comparing the cost of the git operations on the post-commit
// enqueue hot path (issue #916) done two ways:
//
//   - "exec": spawning a `git` subprocess per operation, exactly as the
//     daemon does today. On Windows each spawn costs ~250-750ms on a large
//     repo, and the enqueue handler runs several in sequence.
//   - "gogit": the same operation via the in-process go-git library, which
//     avoids the process-spawn overhead entirely.
//
// The point is to measure, on a real large repository on Windows, whether
// moving these calls to go-git is actually faster, and by how much. Nothing
// in production imports go-git for these paths; this file is test-only.
//
// Run against a large repo (the interesting case is Windows):
//
//	# PowerShell
//	$env:ROBOREV_BENCH_REPO = "C:\path\to\large\repo"
//	go test -run x -bench . -benchmem ./internal/git/
//
//	# bash
//	ROBOREV_BENCH_REPO=/path/to/large/repo \
//	  go test -run x -bench . -benchmem ./internal/git/
//
// To exercise the worktree case from the issue (the extra git-common-dir
// lookup), point ROBOREV_BENCH_REPO at a linked worktree of the large repo.
//
// If ROBOREV_BENCH_REPO is unset it falls back to the current repo, which is
// small; the spawn-vs-library gap only becomes dramatic on a large history.

import (
	"os"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// benchRepoPath returns the repo to benchmark against, skipping the benchmark
// if it is not a usable git repo.
func benchRepoPath(b *testing.B) string {
	b.Helper()
	path := os.Getenv("ROBOREV_BENCH_REPO")
	if path == "" {
		path = "."
	}
	if _, err := openGoGit(path); err != nil {
		b.Skipf("ROBOREV_BENCH_REPO=%q is not a usable git repo: %v", path, err)
	}
	return path
}

// openGoGit opens path with the options needed to behave like git on the
// enqueue path: detect the .git dir from a subdirectory and follow a linked
// worktree's commondir (the worktree case from issue #916).
func openGoGit(path string) (*gogit.Repository, error) {
	return gogit.PlainOpenWithOptions(path, &gogit.PlainOpenOptions{
		DetectDotGit:          true,
		EnableDotGitCommonDir: true,
	})
}

// execGitOut runs git via newGitCmd -- the same production helper the daemon
// uses -- so the measured spawn cost includes its GIT_* env stripping and
// Windows console-hiding, and an inherited GIT_DIR/GIT_WORK_TREE can't redirect
// the benchmark at the wrong repository.
func execGitOut(b *testing.B, dir string, args ...string) string {
	b.Helper()
	cmd := newGitCmd(args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		b.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// hasParent reports whether HEAD~1 resolves, so the IsAncestor benchmark can
// skip single-commit repos.
func hasParent(path string) bool {
	repo, err := openGoGit(path)
	if err != nil {
		return false
	}
	_, err = repo.ResolveRevision(plumbing.Revision("HEAD~1"))
	return err == nil
}

func BenchmarkSpawnResolveHEAD(b *testing.B) {
	path := benchRepoPath(b)

	b.Run("exec", func(b *testing.B) {
		for b.Loop() {
			_ = execGitOut(b, path, "rev-parse", "HEAD")
		}
	})

	b.Run("gogit", func(b *testing.B) {
		for b.Loop() {
			repo, err := openGoGit(path)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := repo.ResolveRevision(plumbing.Revision("HEAD")); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkSpawnCurrentBranch(b *testing.B) {
	path := benchRepoPath(b)

	b.Run("exec", func(b *testing.B) {
		for b.Loop() {
			_ = execGitOut(b, path, "symbolic-ref", "HEAD")
		}
	})

	b.Run("gogit", func(b *testing.B) {
		for b.Loop() {
			repo, err := openGoGit(path)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := repo.Head(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkSpawnWorktreeRoot(b *testing.B) {
	path := benchRepoPath(b)

	b.Run("exec", func(b *testing.B) {
		for b.Loop() {
			_ = execGitOut(b, path, "rev-parse", "--show-toplevel")
		}
	})

	b.Run("gogit", func(b *testing.B) {
		for b.Loop() {
			repo, err := openGoGit(path)
			if err != nil {
				b.Fatal(err)
			}
			wt, err := repo.Worktree()
			if err != nil {
				b.Fatal(err)
			}
			_ = wt.Filesystem.Root()
		}
	})
}

func BenchmarkSpawnCommitInfo(b *testing.B) {
	path := benchRepoPath(b)
	const rs = "\x1e"

	b.Run("exec", func(b *testing.B) {
		for b.Loop() {
			_ = execGitOut(b, path, "log", "-1",
				"--format=%H"+rs+"%an"+rs+"%s"+rs+"%aI"+rs+"%b", "HEAD")
		}
	})

	b.Run("gogit", func(b *testing.B) {
		for b.Loop() {
			repo, err := openGoGit(path)
			if err != nil {
				b.Fatal(err)
			}
			h, err := repo.ResolveRevision(plumbing.Revision("HEAD"))
			if err != nil {
				b.Fatal(err)
			}
			c, err := repo.CommitObject(*h)
			if err != nil {
				b.Fatal(err)
			}
			// Touch the fields GetCommitInfo reads so the work isn't
			// optimized away.
			_, _, _, _ = c.Author.Name, c.Author.When, c.Message, c.Hash
		}
	})
}

func BenchmarkSpawnIsAncestor(b *testing.B) {
	path := benchRepoPath(b)
	if !hasParent(path) {
		b.Skip("repo has no HEAD~1; need at least two commits")
	}

	b.Run("exec", func(b *testing.B) {
		for b.Loop() {
			cmd := newGitCmd("merge-base", "--is-ancestor", "HEAD~1", "HEAD")
			cmd.Dir = path
			if err := cmd.Run(); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("gogit", func(b *testing.B) {
		for b.Loop() {
			repo, err := openGoGit(path)
			if err != nil {
				b.Fatal(err)
			}
			anc, err := repo.ResolveRevision(plumbing.Revision("HEAD~1"))
			if err != nil {
				b.Fatal(err)
			}
			desc, err := repo.ResolveRevision(plumbing.Revision("HEAD"))
			if err != nil {
				b.Fatal(err)
			}
			ancCommit, err := repo.CommitObject(*anc)
			if err != nil {
				b.Fatal(err)
			}
			descCommit, err := repo.CommitObject(*desc)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := ancCommit.IsAncestor(descCommit); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkSpawnPatchID measures the show|patch-id pipeline. go-git has no
// stable patch-id implementation, so there is no library variant: this stays
// a subprocess regardless and is reported for reference.
func BenchmarkSpawnPatchID(b *testing.B) {
	path := benchRepoPath(b)
	sha := execGitOut(b, path, "rev-parse", "HEAD")
	for b.Loop() {
		_ = GetPatchID(path, sha)
	}
}

// BenchmarkEnqueueSequence runs the full per-commit enqueue sequence the
// daemon performs, both ways. The "exec" variant spawns a process per call
// (today's behavior); the "gogit" variant opens the repo once and runs every
// operation in-process, which is the realistic shape of a go-git refactor.
// patch-id stays a subprocess in both since go-git cannot produce it.
func BenchmarkEnqueueSequence(b *testing.B) {
	path := benchRepoPath(b)
	const rs = "\x1e"
	ancestor := hasParent(path)

	b.Run("exec", func(b *testing.B) {
		for b.Loop() {
			_ = execGitOut(b, path, "rev-parse", "--show-toplevel")
			_ = execGitOut(b, path, "rev-parse", "--git-common-dir")
			_ = execGitOut(b, path, "symbolic-ref", "HEAD")
			sha := execGitOut(b, path, "rev-parse", "HEAD")
			_ = execGitOut(b, path, "log", "-1",
				"--format=%H"+rs+"%an"+rs+"%s"+rs+"%aI"+rs+"%b", "HEAD")
			_ = GetPatchID(path, sha)
			if ancestor {
				cmd := newGitCmd("merge-base", "--is-ancestor", "HEAD~1", "HEAD")
				cmd.Dir = path
				_ = cmd.Run()
			}
		}
	})

	b.Run("gogit", func(b *testing.B) {
		for b.Loop() {
			repo, err := openGoGit(path)
			if err != nil {
				b.Fatal(err)
			}
			wt, err := repo.Worktree()
			if err != nil {
				b.Fatal(err)
			}
			_ = wt.Filesystem.Root()
			head, err := repo.Head()
			if err != nil {
				b.Fatal(err)
			}
			_ = head.Name().Short()
			sha := head.Hash()
			c, err := repo.CommitObject(sha)
			if err != nil {
				b.Fatal(err)
			}
			_, _, _ = c.Author.Name, c.Author.When, c.Message
			// patch-id has no go-git equivalent; still a subprocess.
			_ = GetPatchID(path, sha.String())
			if ancestor {
				anc, err := repo.ResolveRevision(plumbing.Revision("HEAD~1"))
				if err != nil {
					b.Fatal(err)
				}
				ancCommit, err := repo.CommitObject(*anc)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := ancCommit.IsAncestor(c); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}
