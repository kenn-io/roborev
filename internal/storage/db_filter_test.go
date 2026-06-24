package storage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobCounts(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")

	for i := range 3 {
		sha := fmt.Sprintf("queued%d", i)
		commit := createCommit(t, db, repo.ID, sha)
		enqueueJob(t, db, repo.ID, commit.ID, sha)
	}

	commit := createCommit(t, db, repo.ID, "done1")
	job := enqueueJob(t, db, repo.ID, commit.ID, "done1")
	_, _ = db.ClaimJob("drain1")
	_, _ = db.ClaimJob("drain2")
	_, _ = db.ClaimJob("drain3")
	claimed, _ := db.ClaimJob("w1")
	if claimed != nil {
		assert.Equal(t, claimed.ID, job.ID)
		db.CompleteJob(claimed.ID, "codex", "p", "o")
	}

	commit2 := createCommit(t, db, repo.ID, "fail1")
	enqueueJob(t, db, repo.ID, commit2.ID, "fail1")
	claimed2, _ := db.ClaimJob("w2")
	if claimed2 != nil {
		db.FailJob(claimed2.ID, "", "err")
	}

	queued, running, done, failed, _, _, _, _, err := db.GetJobCounts()
	require.NoError(t, err, "GetJobCounts failed: %v")

	assert.Equal(t, 0, queued)
	assert.Equal(t, 3, running)
	assert.Equal(t, 1, done)
	assert.Equal(t, 1, failed)
}

func TestCountStalledJobs(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _, _ := createJobChain(t, db, "/tmp/test-repo", "recent1")
	_, _ = db.ClaimJob("worker-1")

	count, err := db.CountStalledJobs(30 * time.Minute)
	require.NoError(t, err, "CountStalledJobs failed: %v")

	assert.Equal(t, 0, count)

	commit2 := createCommit(t, db, repo.ID, "stalled1")
	job2 := enqueueJob(t, db, repo.ID, commit2.ID, "stalled1")
	backdateJobStart(t, db, job2.ID, 1*time.Hour)

	count, err = db.CountStalledJobs(30 * time.Minute)
	require.NoError(t, err, "CountStalledJobs failed: %v")

	assert.Equal(t, 1, count)

	commit3 := createCommit(t, db, repo.ID, "stalled2")
	job3 := enqueueJob(t, db, repo.ID, commit3.ID, "stalled2")

	tzMinus7 := time.FixedZone("UTC-7", -7*60*60)
	backdateJobStartWithOffset(t, db, job3.ID, 1*time.Hour, tzMinus7)

	count, err = db.CountStalledJobs(30 * time.Minute)
	require.NoError(t, err, "CountStalledJobs failed: %v")

	assert.Equal(t, 2, count)

	count, err = db.CountStalledJobs(2 * time.Hour)
	require.NoError(t, err, "CountStalledJobs failed: %v")

	assert.Equal(t, 0, count)
}

func TestListReposWithReviewCounts(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	t.Run("empty database", func(t *testing.T) {
		repos, totalCount, err := db.ListReposWithReviewCounts()
		require.NoError(t, err, "ListReposWithReviewCounts failed: %v")

		assert.Empty(t, repos)
		assert.Equal(t, 0, totalCount)
	})

	repo1 := createRepo(t, db, "/tmp/repo1")
	repo2 := createRepo(t, db, "/tmp/repo2")
	_ = createRepo(t, db, "/tmp/repo3")

	for i := range 3 {
		sha := fmt.Sprintf("repo1-sha%d", i)
		commit := createCommit(t, db, repo1.ID, sha)
		enqueueJob(t, db, repo1.ID, commit.ID, sha)
	}

	for i := range 2 {
		sha := fmt.Sprintf("repo2-sha%d", i)
		commit := createCommit(t, db, repo2.ID, sha)
		enqueueJob(t, db, repo2.ID, commit.ID, sha)
	}

	t.Run("repos with varying job counts", func(t *testing.T) {
		repos, totalCount, err := db.ListReposWithReviewCounts()
		require.NoError(t, err, "ListReposWithReviewCounts failed: %v")

		assert.Len(t, repos, 3)

		assert.Equal(t, 5, totalCount)

		repoMap := make(map[string]int)
		for _, r := range repos {
			repoMap[r.Name] = r.Count
		}

		assert.Equal(t, 3, repoMap["repo1"])
		assert.Equal(t, 2, repoMap["repo2"])
		assert.Equal(t, 0, repoMap["repo3"])
	})

	t.Run("counts include all job statuses", func(t *testing.T) {
		claimed, _ := db.ClaimJob("worker-1")
		if claimed != nil {
			db.CompleteJob(claimed.ID, "codex", "prompt", "output")
		}

		claimed2, _ := db.ClaimJob("worker-1")
		if claimed2 != nil {
			db.FailJob(claimed2.ID, "", "test error")
		}

		repos, totalCount, err := db.ListReposWithReviewCounts()
		require.NoError(t, err, "ListReposWithReviewCounts failed: %v")

		assert.Equal(t, 5, totalCount)

		repoMap := make(map[string]int)
		for _, r := range repos {
			repoMap[r.Name] = r.Count
		}

		assert.Equal(t, 3, repoMap["repo1"])
	})
}

func TestListJobsWithRepoFilter(t *testing.T) {
	// seedTwoRepos is shared setup: creates repo1 (3 jobs) and repo2 (2 jobs).
	type twoRepos struct {
		db    *DB
		repo1 *Repo
		repo2 *Repo
	}
	seedTwoRepos := func(t *testing.T) twoRepos {
		t.Helper()
		db := openTestDB(t)
		repo1, _ := seedJobs(t, db, "/tmp/repo1", 3)
		repo2, _ := seedJobs(t, db, "/tmp/repo2", 2)
		return twoRepos{db: db, repo1: repo1, repo2: repo2}
	}

	tests := []struct {
		name      string
		setup     func(t *testing.T) (*DB, string, string, int, int) // returns db, statusFilter, repoFilter, limit, offset
		wantCount int
		verify    func(t *testing.T, jobs []ReviewJob)
	}{
		{
			name: "no filter returns all jobs",
			setup: func(t *testing.T) (*DB, string, string, int, int) {
				s := seedTwoRepos(t)
				return s.db, "", "", 50, 0
			},
			wantCount: 5,
		},
		{
			name: "repo filter returns only matching jobs",
			setup: func(t *testing.T) (*DB, string, string, int, int) {
				s := seedTwoRepos(t)
				return s.db, "", s.repo1.RootPath, 50, 0
			},
			wantCount: 3,
			verify: func(t *testing.T, jobs []ReviewJob) {
				for _, job := range jobs {
					assert.Equal(t, "repo1", job.RepoName)
				}
			},
		},
		{
			name: "limit parameter works",
			setup: func(t *testing.T) (*DB, string, string, int, int) {
				s := seedTwoRepos(t)
				return s.db, "", "", 2, 0
			},
			wantCount: 2,
		},
		{
			name: "limit=0 returns all jobs",
			setup: func(t *testing.T) (*DB, string, string, int, int) {
				s := seedTwoRepos(t)
				return s.db, "", "", 0, 0
			},
			wantCount: 5,
		},
		{
			name: "repo filter with limit",
			setup: func(t *testing.T) (*DB, string, string, int, int) {
				s := seedTwoRepos(t)
				return s.db, "", s.repo1.RootPath, 2, 0
			},
			wantCount: 2,
			verify: func(t *testing.T, jobs []ReviewJob) {
				for _, job := range jobs {
					assert.Equal(t, "repo1", job.RepoName)
				}
			},
		},
		{
			name: "status and repo filter combined",
			setup: func(t *testing.T) (*DB, string, string, int, int) {
				s := seedTwoRepos(t)
				// Complete one job in repo1 so we can filter by status=done.
				claimed := claimJob(t, s.db, "worker-1")
				err := s.db.CompleteJob(claimed.ID, "codex", "prompt", "output")
				require.NoError(t, err, "CompleteJob failed")
				return s.db, "done", s.repo1.RootPath, 50, 0
			},
			wantCount: 1,
			verify: func(t *testing.T, jobs []ReviewJob) {
				for _, job := range jobs {
					assert.Equal(t, JobStatusDone, job.Status)
				}
			},
		},
		{
			name: "offset pagination returns disjoint pages",
			setup: func(t *testing.T) (*DB, string, string, int, int) {
				// This case is handled specially below via its verify func.
				s := seedTwoRepos(t)
				return s.db, "", "", 0, 0
			},
			wantCount: -1, // skip simple count check; verify does full pagination check
			verify: func(t *testing.T, _ []ReviewJob) {
				// Re-seed a fresh DB for this pagination test to ensure isolation.
				db := openTestDB(t)
				seedJobs(t, db, "/tmp/repo1", 3)
				seedJobs(t, db, "/tmp/repo2", 2)

				jobs1, err := db.ListJobs("", "", 2, 0)
				require.NoError(t, err, "ListJobs page 1 failed")
				assert.Len(t, jobs1, 2)

				jobs2, err := db.ListJobs("", "", 2, 2)
				require.NoError(t, err, "ListJobs page 2 failed")
				assert.Len(t, jobs2, 2)

				// Pages must be disjoint.
				for _, j1 := range jobs1 {
					for _, j2 := range jobs2 {
						assert.NotEqual(t, j1.ID, j2.ID,
							"page 1 and page 2 should not overlap")
					}
				}

				jobs3, err := db.ListJobs("", "", 2, 4)
				require.NoError(t, err, "ListJobs page 3 failed")
				assert.Len(t, jobs3, 1)

				db.Close()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, statusFilter, repoFilter, limit, offset := tt.setup(t)
			defer db.Close()

			jobs, err := db.ListJobs(statusFilter, repoFilter, limit, offset)
			require.NoError(t, err, "ListJobs failed")

			if tt.wantCount >= 0 {
				assert.Len(t, jobs, tt.wantCount)
			}
			if tt.verify != nil {
				tt.verify(t, jobs)
			}
		})
	}
}

func TestListJobsHydratesOutputPrefix(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/output-prefix-list")
	const outputPrefix = "## refactor Analysis\n\n**Files:**\n- main.go\n\n---\n\n"
	_, err := db.EnqueueJob(EnqueueOpts{
		RepoID:       repo.ID,
		GitRef:       "refactor",
		Agent:        "test",
		Prompt:       "analyze main.go",
		OutputPrefix: outputPrefix,
	})
	require.NoError(t, err, "EnqueueJob failed")

	jobs, err := db.ListJobs("", repo.RootPath, 0, 0)
	require.NoError(t, err, "ListJobs failed")
	require.Len(t, jobs, 1)
	assert.Equal(t, outputPrefix, jobs[0].OutputPrefix)
}

func TestListJobsWithRepoPaths(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()

	repo1, _ := seedJobs(t, db, "/tmp/repo1", 3)
	repo2, _ := seedJobs(t, db, "/tmp/repo2", 2)
	repo3, _ := seedJobs(t, db, "/tmp/repo3", 4) // excluded by the filters below

	// IN clause scopes to the two named repos (3 + 2), excluding repo3.
	jobs, err := db.ListJobs("", "", 0, 0,
		WithRepoPaths([]string{repo1.RootPath, repo2.RootPath}))
	require.NoError(t, err)
	assert.Len(jobs, 5)
	for _, job := range jobs {
		assert.Contains([]string{"repo1", "repo2"}, job.RepoName)
	}

	// A single-element set behaves like the positional equality filter.
	jobs, err = db.ListJobs("", "", 0, 0, WithRepoPaths([]string{repo1.RootPath}))
	require.NoError(t, err)
	assert.Len(jobs, 3)

	// The positional repoFilter takes precedence over WithRepoPaths, so the
	// two never compound into an empty result.
	jobs, err = db.ListJobs("", repo1.RootPath, 0, 0,
		WithRepoPaths([]string{repo2.RootPath}))
	require.NoError(t, err)
	assert.Len(jobs, 3)
	for _, job := range jobs {
		assert.Equal("repo1", job.RepoName)
	}

	// CountJobStats honors the same multi-repo scoping. Complete repo1's
	// oldest job (claimed first) so exactly one done job exists.
	claimed := claimJob(t, db, "worker-1")
	require.Equal(t, "repo1", claimed.RepoName)
	require.NoError(t, db.CompleteJob(claimed.ID, "codex", "prompt", "output"))

	inScope, err := db.CountJobStats("",
		WithRepoPaths([]string{repo1.RootPath, repo2.RootPath}))
	require.NoError(t, err)
	assert.Equal(1, inScope.Done, "repo1's done job counted when repo1 is in scope")

	outOfScope, err := db.CountJobStats("", WithRepoPaths([]string{repo3.RootPath}))
	require.NoError(t, err)
	assert.Equal(0, outOfScope.Done, "repo1's done job excluded when only repo3 is in scope")
}

func TestListJobsWithGitRefFilter(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/repo-gitref")

	refs := []string{"abc123", "def456", "abc123..def456", "dirty"}
	for _, ref := range refs {
		commit := createCommit(t, db, repo.ID, ref)
		enqueueJob(t, db, repo.ID, commit.ID, ref)
	}

	t.Run("git_ref filter returns matching job", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0, WithGitRef("abc123"))
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 1)
		assert.False(t, len(jobs) > 0 && jobs[0].GitRef != "abc123")
	})

	t.Run("git_ref filter with range ref", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0, WithGitRef("abc123..def456"))
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 1)
		assert.False(t, len(jobs) > 0 && jobs[0].GitRef != "abc123..def456")
	})

	t.Run("git_ref filter with no match returns empty", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0, WithGitRef("nonexistent"))
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Empty(t, jobs)
	})

	t.Run("empty git_ref filter returns all jobs", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0)
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 4)
	})

	t.Run("git_ref filter combined with repo filter", func(t *testing.T) {
		jobs, err := db.ListJobs("", repo.RootPath, 50, 0, WithGitRef("def456"))
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 1)
	})
}

func TestListJobsWithBranchAndClosedFilters(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/repo-branch-addr")
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	branches := []string{"main", "main", "feature"}
	for i, br := range branches {
		sha := fmt.Sprintf("sha%d", i)
		commit, err := db.GetOrCreateCommit(repo.ID, sha, "Author", "Subject", time.Now())
		require.NoError(t, err, "GetOrCreateCommit failed: %v")

		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: sha, Branch: br, Agent: "codex"})
		require.NoError(t, err, "EnqueueJob failed: %v")

		db.ClaimJob("w")
		db.CompleteJob(job.ID, "codex", "", fmt.Sprintf("output %d", i))

		if i == 0 {
			db.MarkReviewClosedByJobID(job.ID, true)
		}
	}

	t.Run("branch filter", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0, WithBranch("main"))
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 2)
	})

	t.Run("closed=false filter", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0, WithClosed(false))
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 2)
	})

	t.Run("closed=true filter", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0, WithClosed(true))
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 1)
	})

	t.Run("branch + closed combined", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0, WithBranch("main"), WithClosed(false))
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 1)
	})
}

func TestWithBranchOrEmpty(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/repo-branch-empty")
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	for i, br := range []string{"main", "feature", ""} {
		sha := fmt.Sprintf("sha-be-%d", i)
		commit, err := db.GetOrCreateCommit(repo.ID, sha, "Author", "Subject", time.Now())
		require.NoError(t, err, "GetOrCreateCommit failed: %v")

		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: sha, Branch: br, Agent: "codex"})
		require.NoError(t, err, "EnqueueJob failed: %v")

		db.ClaimJob("w")
		db.CompleteJob(job.ID, "codex", "", fmt.Sprintf("output %d", i))
	}

	t.Run("WithBranch strict excludes branchless", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0, WithBranch("main"))
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 1)
	})

	t.Run("WithBranchOrEmpty includes branchless", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0, WithBranchOrEmpty("main"))
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 2)
	})
}

func TestListJobsAndGetJobByIDReturnAgentic(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repoPath := filepath.Join(t.TempDir(), "agentic-test-repo")
	repo, err := db.GetOrCreateRepo(repoPath)
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:  repo.ID,
		Agent:   "test-agent",
		Prompt:  "Review this code",
		Agentic: true,
	})
	require.NoError(t, err, "EnqueuePromptJob failed: %v")

	assert.True(t, job.Agentic, "EnqueuePromptJob should return job with Agentic=true")

	t.Run("ListJobs returns agentic field", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0)
		require.NoError(t, err, "ListJobs failed: %v")

		assert.NotEmpty(t, jobs)

		var found bool
		for _, j := range jobs {
			if j.ID == job.ID {
				found = true
				assert.True(t, j.Agentic)
				break
			}
		}
		assert.True(t, found)
	})

	t.Run("GetJobByID returns agentic field", func(t *testing.T) {
		fetchedJob, err := db.GetJobByID(job.ID)
		require.NoError(t, err, "GetJobByID failed: %v")

		assert.True(t, fetchedJob.Agentic)
	})

	t.Run("non-agentic job returns Agentic=false", func(t *testing.T) {
		nonAgenticJob, err := db.EnqueueJob(EnqueueOpts{
			RepoID: repo.ID,
			Agent:  "test-agent",
			Prompt: "Another review",
		})
		require.NoError(t, err, "EnqueuePromptJob failed: %v")

		fetchedJob, err := db.GetJobByID(nonAgenticJob.ID)
		require.NoError(t, err, "GetJobByID failed: %v")

		assert.False(t, fetchedJob.Agentic)

		jobs, err := db.ListJobs("", "", 50, 0)
		require.NoError(t, err, "ListJobs failed: %v")

		var found bool
		for _, j := range jobs {
			if j.ID == nonAgenticJob.ID {
				found = true
				assert.False(t, j.Agentic)
				break
			}
		}
		assert.True(t, found)
	})
}

func TestListReposWithReviewCountsByBranch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo1 := createRepo(t, db, "/tmp/repo1")
	repo2 := createRepo(t, db, "/tmp/repo2")
	repo1Identity := "https://github.com/test/repo1.git"
	repo2Identity := "https://github.com/test/repo2.git"
	require.NoError(t, db.SetRepoIdentity(repo1.ID, repo1Identity))
	require.NoError(t, db.SetRepoIdentity(repo2.ID, repo2Identity))

	commit1 := createCommit(t, db, repo1.ID, "abc123")
	commit2 := createCommit(t, db, repo1.ID, "def456")
	commit3 := createCommit(t, db, repo2.ID, "ghi789")

	job1 := enqueueJob(t, db, repo1.ID, commit1.ID, "abc123")
	job2 := enqueueJob(t, db, repo1.ID, commit2.ID, "def456")
	job3 := enqueueJob(t, db, repo2.ID, commit3.ID, "ghi789")

	setJobBranch(t, db, job1.ID, "main")
	setJobBranch(t, db, job3.ID, "main")
	setJobBranch(t, db, job2.ID, "feature")

	t.Run("filter by main branch", func(t *testing.T) {
		repos, totalCount, err := db.ListReposWithReviewCounts(WithRepoBranch("main"))
		require.NoError(t, err, "ListReposWithReviewCounts(branch=main) failed: %v")

		assert.Len(t, repos, 2)
		assert.Equal(t, 2, totalCount)
		identities := map[string]string{}
		for _, repo := range repos {
			identities[repo.Name] = repo.Identity
		}
		assert.Equal(t, repo1Identity, identities["repo1"])
		assert.Equal(t, repo2Identity, identities["repo2"])
	})

	t.Run("filter by feature branch", func(t *testing.T) {
		repos, totalCount, err := db.ListReposWithReviewCounts(WithRepoBranch("feature"))
		require.NoError(t, err, "ListReposWithReviewCounts(branch=feature) failed: %v")

		assert.Len(t, repos, 1)
		assert.Equal(t, 1, totalCount)
		assert.Equal(t, repo1Identity, repos[0].Identity)
	})

	t.Run("filter by (none) branch", func(t *testing.T) {
		commit4 := createCommit(t, db, repo1.ID, "jkl012")
		enqueueJob(t, db, repo1.ID, commit4.ID, "jkl012")

		repos, totalCount, err := db.ListReposWithReviewCounts(WithRepoBranch("(none)"))
		require.NoError(t, err, "ListReposWithReviewCounts(branch=(none)) failed: %v")

		assert.Len(t, repos, 1)
		assert.Equal(t, 1, totalCount)
	})

	t.Run("empty filter returns all", func(t *testing.T) {
		repos, _, err := db.ListReposWithReviewCounts()
		require.NoError(t, err, "ListReposWithReviewCounts() failed: %v")

		assert.Len(t, repos, 2)
	})
}

func TestListBranchesWithCounts(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo1 := createRepo(t, db, "/tmp/repo1")
	repo2 := createRepo(t, db, "/tmp/repo2")

	commit1 := createCommit(t, db, repo1.ID, "abc123")
	commit2 := createCommit(t, db, repo1.ID, "def456")
	commit3 := createCommit(t, db, repo1.ID, "ghi789")
	commit4 := createCommit(t, db, repo2.ID, "jkl012")
	commit5 := createCommit(t, db, repo2.ID, "mno345")

	job1 := enqueueJob(t, db, repo1.ID, commit1.ID, "abc123")
	job2 := enqueueJob(t, db, repo1.ID, commit2.ID, "def456")
	job3 := enqueueJob(t, db, repo1.ID, commit3.ID, "ghi789")
	job4 := enqueueJob(t, db, repo2.ID, commit4.ID, "jkl012")
	job5 := enqueueJob(t, db, repo2.ID, commit5.ID, "mno345")

	setJobBranch(t, db, job1.ID, "main")
	setJobBranch(t, db, job2.ID, "main")
	setJobBranch(t, db, job4.ID, "main")
	setJobBranch(t, db, job3.ID, "feature")

	t.Run("list all branches", func(t *testing.T) {
		result, err := db.ListBranchesWithCounts(nil)
		require.NoError(t, err, "ListBranchesWithCounts failed: %v")

		assert.Len(t, result.Branches, 3)
		assert.Equal(t, 5, result.TotalCount)
		assert.Equal(t, 1, result.NullsRemaining)
	})

	t.Run("filter by single repo", func(t *testing.T) {
		result, err := db.ListBranchesWithCounts([]string{repo1.RootPath})
		require.NoError(t, err, "ListBranchesWithCounts failed: %v")

		assert.Len(t, result.Branches, 2)
		assert.Equal(t, 3, result.TotalCount)
	})

	t.Run("filter by multiple repos", func(t *testing.T) {
		result, err := db.ListBranchesWithCounts([]string{repo1.RootPath, repo2.RootPath})
		require.NoError(t, err, "ListBranchesWithCounts failed: %v")

		assert.Len(t, result.Branches, 3)
		assert.Equal(t, 5, result.TotalCount)
	})

	t.Run("no nulls when all have branches", func(t *testing.T) {
		setJobBranch(t, db, job5.ID, "develop")
		result, err := db.ListBranchesWithCounts(nil)
		require.NoError(t, err, "ListBranchesWithCounts failed: %v")

		assert.Equal(t, 0, result.NullsRemaining)
	})
}

func TestListJobsVerdictForBranchRangeReview(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "range-verdict-repo"))

	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "abc123..def456", Agent: "codex"})
	require.NoError(t, err, "EnqueueRangeJob failed: %v")

	_, err = db.ClaimJob("worker-0")
	require.NoError(t, err, "ClaimJob failed: %v")

	err = db.CompleteJob(job.ID, "codex", "review prompt", "- Medium — Bug in line 42\nSummary: found issues.")
	require.NoError(t, err, "CompleteJob failed: %v")

	jobs, err := db.ListJobs("", "", 50, 0)
	require.NoError(t, err, "ListJobs failed: %v")

	var found bool
	for _, j := range jobs {
		if j.ID == job.ID {
			found = true
			assert.NotNil(t, j.Verdict)
			assert.Equal(t, "F", *j.Verdict)
			break
		}
	}
	assert.True(t, found)
}

func TestListJobsUsesStoredVerdictBoolWhenPresent(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "stored-verdict-list-repo"))

	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "storedverdict123", Agent: "codex"})
	require.NoError(t, err, "EnqueueJob failed: %v")

	_, err = db.ClaimJob("worker-0")
	require.NoError(t, err, "ClaimJob failed: %v")

	err = db.CompleteJob(job.ID, "codex", "review prompt", "No issues found.\n## Verdict: PASS")
	require.NoError(t, err, "CompleteJob failed: %v")

	_, err = db.Exec(`UPDATE reviews SET verdict_bool = 0 WHERE job_id = ?`, job.ID)
	require.NoError(t, err, "force stored verdict_bool failed: %v")

	jobs, err := db.ListJobs("", "", 50, 0)
	require.NoError(t, err, "ListJobs failed: %v")

	require.Len(t, jobs, 1)
	require.NotNil(t, jobs[0].Verdict)
	assert.Equal(t, "F", *jobs[0].Verdict)
}

func TestListJobsWithJobTypeFilter(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, commit, reviewJob := createJobChain(t, db, "/tmp/repo-jobtype", "jt-sha")

	_, err := db.EnqueueJob(EnqueueOpts{
		RepoID:      repo.ID,
		CommitID:    commit.ID,
		GitRef:      "jt-sha",
		Agent:       "codex",
		JobType:     JobTypeFix,
		ParentJobID: reviewJob.ID,
	})
	require.NoError(t, err, "EnqueueJob fix failed: %v")

	tests := []struct {
		name          string
		opts          []ListJobsOption
		expectedLen   int
		expectedTypes []string
	}{
		{"filter by fix returns only fix jobs", []ListJobsOption{WithJobType("fix")}, 1, []string{JobTypeFix}},
		{"filter by review returns only review jobs", []ListJobsOption{WithJobType("review")}, 1, []string{JobTypeReview}},
		{"no filter returns all jobs", nil, 2, nil},
		{"nonexistent type returns empty", []ListJobsOption{WithJobType("nonexistent")}, 0, nil},
		{"exclude fix returns only non-fix jobs", []ListJobsOption{WithExcludeJobType("fix")}, 1, []string{JobTypeReview}},
		{"exclude review returns only non-review jobs", []ListJobsOption{WithExcludeJobType("review")}, 1, []string{JobTypeFix}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobs, err := db.ListJobs("", "", 50, 0, tt.opts...)
			require.NoError(t, err, "ListJobs failed: %v")

			if len(jobs) != tt.expectedLen {
				assert.Len(t, jobs, tt.expectedLen, "Expected %d jobs, got %d", tt.expectedLen, len(jobs))
			}
			if tt.expectedTypes != nil {
				for i, typ := range tt.expectedTypes {
					assert.Equal(t, jobs[i].JobType, typ)
				}
			}
		})
	}
}

func TestListJobsWithHideClassifyJobs(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Plain queued review — should always be visible.
	repo, _, _ := createJobChain(t, db, "/tmp/repo-hide-classify", "review-sha")

	// Running classify row (auto-design router decision in flight). The
	// (repo_id, commit_id, review_type='design') partial unique index on
	// source='auto_design' rows means each auto_design row needs a
	// distinct commit.
	classifyCommit := createCommit(t, db, repo.ID, "classify-sha")
	var classifyID int64
	require.NoError(t, db.QueryRow(`
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, status, job_type, review_type, source, worker_id, started_at, enqueued_at, updated_at)
		VALUES (?, ?, 'classify-sha', 'running', 'classify', 'design', 'auto_design', 'w1', datetime('now'), datetime('now'), datetime('now'))
		RETURNING id
	`, repo.ID, classifyCommit.ID).Scan(&classifyID))

	// Skipped design row (classifier or heuristic decided no design
	// review needed).
	skippedCommit := createCommit(t, db, repo.ID, "skipped-sha")
	var skippedID int64
	require.NoError(t, db.QueryRow(`
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, status, job_type, review_type, source, skip_reason, enqueued_at, finished_at, updated_at)
		VALUES (?, ?, 'skipped-sha', 'skipped', 'review', 'design', 'auto_design', 'trivial diff', datetime('now'), datetime('now'), datetime('now'))
		RETURNING id
	`, repo.ID, skippedCommit.ID).Scan(&skippedID))

	// Skipped row that did NOT come from the auto-design router (source
	// set to empty string). A future pipeline could adopt
	// status='skipped' for some other reason; this filter must leave
	// such rows alone.
	otherSkippedCommit := createCommit(t, db, repo.ID, "other-skipped-sha")
	var otherSkippedID int64
	require.NoError(t, db.QueryRow(`
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, status, job_type, review_type, source, skip_reason, enqueued_at, finished_at, updated_at)
		VALUES (?, ?, 'other-skipped-sha', 'skipped', 'review', '', '', 'unrelated reason', datetime('now'), datetime('now'), datetime('now'))
		RETURNING id
	`, repo.ID, otherSkippedCommit.ID).Scan(&otherSkippedID))

	// Skipped row with source IS NULL (the column is nullable, and
	// EnqueueJob never sets it for plain reviews). This case is
	// load-bearing: a NULL-naive predicate would silently drop these
	// rows from results.
	nullSkippedCommit := createCommit(t, db, repo.ID, "null-skipped-sha")
	var nullSkippedID int64
	require.NoError(t, db.QueryRow(`
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, status, job_type, review_type, skip_reason, enqueued_at, finished_at, updated_at)
		VALUES (?, ?, 'null-skipped-sha', 'skipped', 'review', '', 'unrelated reason', datetime('now'), datetime('now'), datetime('now'))
		RETURNING id
	`, repo.ID, nullSkippedCommit.ID).Scan(&nullSkippedID))

	// Classify-typed row that did NOT come from the auto-design router
	// (source=''). A future pipeline could use the classify job_type
	// for unrelated routing; the filter must leave such rows alone.
	otherClassifyCommit := createCommit(t, db, repo.ID, "other-classify-sha")
	var otherClassifyID int64
	require.NoError(t, db.QueryRow(`
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, status, job_type, review_type, source, worker_id, started_at, enqueued_at, updated_at)
		VALUES (?, ?, 'other-classify-sha', 'running', 'classify', 'review', '', 'w2', datetime('now'), datetime('now'), datetime('now'))
		RETURNING id
	`, repo.ID, otherClassifyCommit.ID).Scan(&otherClassifyID))

	t.Run("default returns all rows", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0)
		require.NoError(t, err)
		assert.Len(t, jobs, 6,
			"default should include both classify variants, all three skipped variants, and the plain review row")
	})

	t.Run("hide_classify_jobs only hides auto_design rows", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0, WithHideClassifyJobs())
		require.NoError(t, err)
		// Should keep: plain review, source='' skipped, source IS NULL
		// skipped, non-auto_design classify. Should hide: classify row
		// with source='auto_design', skipped row with source='auto_design'.
		assert.Len(t, jobs, 4, "should hide only auto-design router rows")
		var sawOtherSkipped, sawNullSkipped, sawOtherClassify bool
		for _, j := range jobs {
			assert.NotEqual(t, classifyID, j.ID,
				"auto_design classify row should be hidden")
			assert.NotEqual(t, skippedID, j.ID,
				"auto_design skipped row should be hidden")
			switch j.ID {
			case otherSkippedID:
				sawOtherSkipped = true
			case nullSkippedID:
				sawNullSkipped = true
			case otherClassifyID:
				sawOtherClassify = true
			}
		}
		assert.True(t, sawOtherSkipped,
			"skipped rows with source='' must remain visible")
		assert.True(t, sawNullSkipped,
			"skipped rows with source IS NULL must remain visible")
		assert.True(t, sawOtherClassify,
			"classify rows with source != 'auto_design' must remain visible")
	})
}

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"plain", "plain"},
		{"100%", "100!%"},
		{"under_score", "under!_score"},
		{"has!bang", "has!!bang"},
		{`C:\Users\foo`, `C:\Users\foo`},
		{"combo!_%", "combo!!!_!%"},
	}
	for _, tt := range tests {
		got := escapeLike(tt.input)
		assert.Equal(t, tt.want, got, "escapeLike(%q) = %q, want %q", tt.input, got, tt.want)

	}
}

func TestPrefixFilterWithSpecialChars(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	other := filepath.Join(base, "other")
	wsPrefix := filepath.ToSlash(workspace)
	otherPrefix := filepath.ToSlash(other)

	createRepo(t, db, filepath.Join(workspace, "repo_one"))
	createRepo(t, db, filepath.Join(workspace, "repo%two"))
	createRepo(t, db, filepath.Join(other, "repo"))

	repo1, _ := db.GetRepoByPath(filepath.Join(workspace, "repo_one"))
	commit1 := createCommit(t, db, repo1.ID, "sha1")
	enqueueJob(t, db, repo1.ID, commit1.ID, "sha1")

	repo2, _ := db.GetRepoByPath(filepath.Join(workspace, "repo%two"))
	commit2 := createCommit(t, db, repo2.ID, "sha2")
	enqueueJob(t, db, repo2.ID, commit2.ID, "sha2")

	repo3, _ := db.GetRepoByPath(filepath.Join(other, "repo"))
	commit3 := createCommit(t, db, repo3.ID, "sha3")
	enqueueJob(t, db, repo3.ID, commit3.ID, "sha3")

	t.Run("prefix with underscore matches correctly", func(t *testing.T) {
		jobs, err := db.ListJobs(
			"", "", 50, 0, WithRepoPrefix(wsPrefix),
		)
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 2)
	})

	t.Run("prefix filter excludes non-matching", func(t *testing.T) {
		jobs, err := db.ListJobs(
			"", "", 50, 0, WithRepoPrefix(otherPrefix),
		)
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 1)
	})

	for range 3 {
		claimed := claimJob(t, db, "w1")
		if err := db.CompleteJob(claimed.ID, "codex", "p", "o"); err != nil {
			require.NoError(t, err, "CompleteJob failed: %v")
		}
	}

	t.Run("CountJobStats with special-char prefix", func(t *testing.T) {
		stats, err := db.CountJobStats(
			"", WithRepoPrefix(wsPrefix),
		)
		require.NoError(t, err, "CountJobStats failed: %v")

		assert.Equal(t, 2, stats.Done)
		assert.Equal(t, 2, stats.Open)
	})

	t.Run("ListReposWithReviewCounts with special-char prefix", func(t *testing.T) {
		repos, total, err := db.ListReposWithReviewCounts(
			WithRepoPathPrefix(wsPrefix),
		)
		require.NoError(t, err, "ListReposWithReviewCounts failed: %v")

		assert.Len(t, repos, 2)
		assert.Equal(t, 2, total)
	})

	t.Run("backslash prefix matches normalized Windows path", func(t *testing.T) {
		createRepo(t, db, `C:\Users\dev\workspace\project-a`)
		rA, _ := db.GetRepoByPath(`C:\Users\dev\workspace\project-a`)
		cA := createCommit(t, db, rA.ID, "win-a")
		enqueueJob(t, db, rA.ID, cA.ID, "win-a")
		claimed := claimJob(t, db, "w2")
		require.NoError(t, db.CompleteJob(claimed.ID, "codex", "p", "o"))

		jobs, err := db.ListJobs(
			"", "", 50, 0, WithRepoPrefix(`C:\Users\dev\workspace`),
		)
		require.NoError(t, err, "ListJobs with backslash prefix should not error: %v")
		assert.Len(t, jobs, 1)

		stats, err := db.CountJobStats(
			"", WithRepoPrefix(`C:\Users\dev\workspace`),
		)
		require.NoError(t, err, "CountJobStats with backslash prefix should not error: %v")
		assert.Equal(t, 1, stats.Open)

		repos, total, err := db.ListReposWithReviewCounts(
			WithRepoPathPrefix(`C:\Users\dev\workspace`),
		)
		require.NoError(t, err, "ListReposWithReviewCounts with backslash prefix should not error: %v")
		assert.Len(t, repos, 1)
		assert.Equal(t, 1, total)
	})
}

func TestRootPrefixMatchesAllRepos(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	base := t.TempDir()
	basePrefix := filepath.ToSlash(base)

	createRepo(t, db, filepath.Join(base, "a", "repo1"))
	createRepo(t, db, filepath.Join(base, "b", "repo2"))

	r1, _ := db.GetRepoByPath(filepath.Join(base, "a", "repo1"))
	c1 := createCommit(t, db, r1.ID, "r1")
	enqueueJob(t, db, r1.ID, c1.ID, "r1")

	r2, _ := db.GetRepoByPath(filepath.Join(base, "b", "repo2"))
	c2 := createCommit(t, db, r2.ID, "r2")
	enqueueJob(t, db, r2.ID, c2.ID, "r2")

	t.Run("parent prefix returns all repos via ListJobs", func(t *testing.T) {
		jobs, err := db.ListJobs(
			"", "", 50, 0, WithRepoPrefix(basePrefix),
		)
		require.NoError(t, err, "ListJobs failed: %v")

		assert.Len(t, jobs, 2)
	})

	t.Run("parent prefix returns all repos via ListReposWithReviewCounts", func(t *testing.T) {
		repos, total, err := db.ListReposWithReviewCounts(
			WithRepoPathPrefix(basePrefix),
		)
		require.NoError(t, err, "ListReposWithReviewCounts failed: %v")

		assert.Len(t, repos, 2)
		assert.Equal(t, 2, total)
	})
}

func TestListReposWithCombinedPrefixAndBranch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	wsPrefix := filepath.ToSlash(ws)

	r1 := createRepo(t, db, filepath.Join(ws, "repo-a"))
	r2 := createRepo(t, db, filepath.Join(ws, "repo-b"))
	r3 := createRepo(t, db, filepath.Join(base, "other", "repo-c"))

	c1 := createCommit(t, db, r1.ID, "a1")
	c2 := createCommit(t, db, r1.ID, "a2")
	c3 := createCommit(t, db, r1.ID, "a3")
	j1 := enqueueJob(t, db, r1.ID, c1.ID, "a1")
	j2 := enqueueJob(t, db, r1.ID, c2.ID, "a2")
	j3 := enqueueJob(t, db, r1.ID, c3.ID, "a3")
	setJobBranch(t, db, j1.ID, "main")
	setJobBranch(t, db, j2.ID, "main")
	setJobBranch(t, db, j3.ID, "feature")

	c4 := createCommit(t, db, r2.ID, "b1")
	j4 := enqueueJob(t, db, r2.ID, c4.ID, "b1")
	setJobBranch(t, db, j4.ID, "main")

	c5 := createCommit(t, db, r3.ID, "c1")
	j5 := enqueueJob(t, db, r3.ID, c5.ID, "c1")
	setJobBranch(t, db, j5.ID, "main")

	t.Run("prefix + branch filters both", func(t *testing.T) {
		repos, total, err := db.ListReposWithReviewCounts(
			WithRepoPathPrefix(wsPrefix),
			WithRepoBranch("main"),
		)
		require.NoError(t, err, "ListReposWithReviewCounts failed: %v")

		assert.Len(t, repos, 2)
		assert.Equal(t, 3, total)
	})

	t.Run("prefix + feature branch", func(t *testing.T) {
		repos, total, err := db.ListReposWithReviewCounts(
			WithRepoPathPrefix(wsPrefix),
			WithRepoBranch("feature"),
		)
		require.NoError(t, err, "ListReposWithReviewCounts failed: %v")

		assert.Len(t, repos, 1)
		assert.Equal(t, 1, total)
	})

	t.Run("prefix only returns all branches", func(t *testing.T) {
		repos, total, err := db.ListReposWithReviewCounts(
			WithRepoPathPrefix(wsPrefix),
		)
		require.NoError(t, err, "ListReposWithReviewCounts failed: %v")

		assert.Len(t, repos, 2)
		assert.Equal(t, 4, total)
	})
}

func TestListJobsWithBeforeCursor(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, jobs := seedJobs(t, db, "/tmp/cursor-repo", 5)

	t.Run("before cursor returns only older jobs", func(t *testing.T) {
		// Use the middle job's ID as cursor
		cursor := jobs[2].ID
		result, err := db.ListJobs("", "", 50, 0, WithBeforeCursor(cursor))
		require.NoError(t, err, "ListJobs failed")

		for _, j := range result {
			assert.Less(t, j.ID, cursor,
				"all returned job IDs should be less than cursor")
		}
	})

	t.Run("cursor at lowest ID returns empty", func(t *testing.T) {
		cursor := jobs[0].ID
		result, err := db.ListJobs("", "", 50, 0, WithBeforeCursor(cursor))
		require.NoError(t, err, "ListJobs failed")

		assert.Empty(t, result)
	})

	t.Run("cursor above highest ID returns all jobs", func(t *testing.T) {
		cursor := jobs[len(jobs)-1].ID + 100
		result, err := db.ListJobs("", "", 50, 0, WithBeforeCursor(cursor))
		require.NoError(t, err, "ListJobs failed")

		assert.Len(t, result, 5)
	})

	t.Run("cursor with limit returns correct page", func(t *testing.T) {
		cursor := jobs[4].ID
		result, err := db.ListJobs("", "", 2, 0, WithBeforeCursor(cursor))
		require.NoError(t, err, "ListJobs failed")

		assert.Len(t, result, 2)
		for _, j := range result {
			assert.Less(t, j.ID, cursor)
		}
		// Results ordered by ID DESC, so first result has highest ID
		assert.Greater(t, result[0].ID, result[1].ID)
	})

	t.Run("cursor combined with other filters", func(t *testing.T) {
		// Set branch on some jobs for combined filtering
		setJobBranch(t, db, jobs[0].ID, "main")
		setJobBranch(t, db, jobs[1].ID, "main")
		setJobBranch(t, db, jobs[2].ID, "feature")
		setJobBranch(t, db, jobs[3].ID, "main")
		setJobBranch(t, db, jobs[4].ID, "main")

		cursor := jobs[4].ID
		result, err := db.ListJobs("", "", 50, 0,
			WithBeforeCursor(cursor), WithBranch("main"))
		require.NoError(t, err, "ListJobs failed")

		for _, j := range result {
			assert.Less(t, j.ID, cursor)
			assert.Equal(t, "main", j.Branch)
		}
		// jobs[0], jobs[1], jobs[3] have branch=main and ID < jobs[4].ID
		assert.Len(t, result, 3)
	})
}
