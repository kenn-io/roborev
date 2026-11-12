package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type recordingArchiveClient struct {
	*katatest.FakeClient
	refs []string
}

func (f *recordingArchiveClient) ArchiveRubric(_ context.Context, _, ref, _ string, _ []byte) error {
	f.refs = append(f.refs, ref)
	return nil
}

func TestArchiveRubricRefsAndLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []string
		refs     []string
	}{
		{"deduplicate across messages", []string{"kata#abc4 demo#def5", "kata#abc4"}, []string{"abc4", "def5"}},
		{"issue cap", []string{"kata#0001 kata#0002 kata#0003 kata#0004 kata#0005 kata#0006 kata#0007 kata#0008 kata#0009 kata#0010 kata#0011"}, []string{"0001", "0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009", "0010"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &recordingArchiveClient{FakeClient: &katatest.FakeClient{}}
			archiveRubricMessages(t.Context(), fake, "demo", tc.messages, "rubric.md", []byte("rubric\n"), func(err error) { assert.NoError(t, err) })
			assert.Equal(t, tc.refs, fake.refs)
		})
	}
}

func TestPrepareRubricArchiveEligibility(t *testing.T) {
	data := []byte("rubric\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	base := storage.ReviewJob{JobType: storage.JobTypeReview, RubricSHA256: &digest}
	tests := []struct {
		name    string
		job     storage.ReviewJob
		enabled bool
		data    []byte
		want    bool
	}{
		{"local", base, true, data, true},
		{"off", base, false, data, false},
		{"CI", storage.ReviewJob{JobType: storage.JobTypeReview, Source: storage.JobSourceCI, RubricSHA256: &digest}, true, data, false},
		{"CI branch", storage.ReviewJob{JobType: storage.JobTypeReview, Source: "ci_branch", RubricSHA256: &digest}, true, data, false},
		{"dirty", storage.ReviewJob{JobType: storage.JobTypeDirty, RubricSHA256: &digest}, true, data, false},
		{"synthesis", storage.ReviewJob{JobType: storage.JobTypeSynthesis, RubricSHA256: &digest}, true, data, false},
		{"supplied prompt", storage.ReviewJob{JobType: storage.JobTypeReview, Source: storage.JobSourceReviewPrompt, RubricSHA256: &digest}, true, data, false},
		{"prebuilt", storage.ReviewJob{JobType: storage.JobTypeReview, PromptPrebuilt: true, RubricSHA256: &digest}, true, data, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, prepareRubricArchive(&tc.job, tc.enabled, tc.data)) })
	}
	large := []byte(strings.Repeat("x", rubricArchiveMaxBytes+1))
	largeDigest := fmt.Sprintf("%x", sha256.Sum256(large))
	base.RubricSHA256 = &largeDigest
	assert.False(t, prepareRubricArchive(&base, true, large))
}

func TestArchiveRubricCommitAndRange(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := repo.CommitFile("a", "a", "first\n\nCloses: kata#abc4")
	head := repo.CommitFile("b", "b", "second\n\nCloses: demo#def5 other#zzzz")
	fake := &katatest.FakeClient{BindingResult: kata.Binding{Project: "demo"}}
	data := []byte("rubric\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	job := storage.ReviewJob{RepoPath: repo.Root, GitRef: base + ".." + head, ReviewType: "custom"}
	report := func(err error) { assert.NoError(t, err) }
	archiveRubric(t.Context(), fake, job, repo.Root, data, digest, report)
	name := fmt.Sprintf("roborev-rubric.custom.%s.md", digest[:16])
	assert.Equal(t, data, fake.Archives["def5"][name])
	assert.Len(t, fake.Archives, 1)
	job.GitRef = base
	archiveRubric(t.Context(), fake, job, repo.Root, data, digest, report)
	assert.Equal(t, data, fake.Archives["abc4"][name])
	newData := []byte("changed\n")
	newDigest := fmt.Sprintf("%x", sha256.Sum256(newData))
	archiveRubric(t.Context(), fake, job, repo.Root, newData, newDigest, report)
	assert.Len(t, fake.Archives["abc4"], 2)
	assert.Equal(t, newData, fake.Archives["abc4"][fmt.Sprintf("roborev-rubric.custom.%s.md", newDigest[:16])])
}

type blockedArchiveClient struct {
	*katatest.FakeClient
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	data     []byte
}

func (f *blockedArchiveClient) ArchiveRubric(ctx context.Context, _, _, _ string, data []byte) error {
	f.data = append([]byte(nil), data...)
	close(f.started)
	defer close(f.finished)
	select {
	case <-f.release:
		return errors.New("synthetic archive failure")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestProcessJobArchiveDoesNotDelayCompletion(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.KataRubrics.Mode = "current"
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	fake := &blockedArchiveClient{FakeClient: &katatest.FakeClient{BindingResult: kata.Binding{Project: "demo"}}, started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	sha := tc.GitRepo.CommitFile("file.txt", "change", "change\n\nCloses: demo#abc4")
	worktree := filepath.Join(t.TempDir(), "worktree")
	tc.GitRepo.RunGit("worktree", "add", "--detach", worktree, sha)
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".kata.toml"), []byte("[project]\nname = 'main'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".kata.toml"), []byte("[project]\nname = 'demo'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".roborev.toml"), []byte("[review.types.custom]\ntemplate = 'rubric.md'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "rubric.md"), []byte("check correctness\n"), 0o600))
	var archivePath string
	tc.Pool.archiveClient = func(path string) kata.Client {
		archivePath = path
		binding, err := kata.NewCLIClient(path).Binding(t.Context())
		require.NoError(t, err)
		fake.BindingResult = binding
		return fake
	}
	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: "test", ReviewType: "custom", WorktreePath: worktree})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	id, events := tc.Broadcaster.Subscribe("")
	defer tc.Broadcaster.Unsubscribe(id)
	tc.Pool.processJob(testWorkerID, claimed)
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.Equal(t, worktree, archivePath)
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, review)
	var completed bool
	for len(events) > 0 {
		if (<-events).Type == "review.completed" {
			completed = true
		}
	}
	assert.True(t, completed)
	// Archive startup includes external Git subprocesses.
	require.Eventually(t, func() bool {
		select {
		case <-fake.started:
			return true
		default:
			return false
		}
	}, 10*time.Second, 10*time.Millisecond)
	stored, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, claimed.RepoPath, stored.RepoPath)
	require.NotNil(t, stored.RubricSHA256)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(fake.data)), *stored.RubricSHA256)
	close(fake.release)
	<-fake.finished
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
}

func TestArchiveRubricReportsBindingAndAttachmentFailures(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.CommitFile("file", "change", "change\n\nCloses: kata#abc4")
	job := storage.ReviewJob{RepoPath: repo.Root, GitRef: sha, ReviewType: "custom"}
	data := []byte("rubric\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	for _, fake := range []*katatest.FakeClient{
		{BindingErr: kata.ErrNoBinding},
		{BindingResult: kata.Binding{Project: "demo"}, ArchiveErr: kata.ErrUnavailable},
	} {
		var failures []error
		archiveRubric(t.Context(), fake, job, repo.Root, data, digest, func(err error) { failures = append(failures, err) })
		require.Len(t, failures, 1)
		assert.Error(t, failures[0])
	}
}

func TestArchiveRubricDeadlineStopsPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &blockedArchiveClient{FakeClient: &katatest.FakeClient{}, started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
		ctx, cancel := context.WithTimeout(t.Context(), rubricArchiveTimeout)
		defer cancel()
		var failures []error
		done := make(chan struct{})
		go func() {
			archiveRubricMessages(ctx, fake, "demo", []string{"kata#abc4 kata#def5"}, "rubric.md", []byte("rubric"), func(err error) { failures = append(failures, err) })
			close(done)
		}()
		<-fake.started
		time.Sleep(rubricArchiveTimeout)
		<-done
		require.Len(t, failures, 1)
		assert.ErrorIs(t, failures[0], context.DeadlineExceeded)
	})
}
