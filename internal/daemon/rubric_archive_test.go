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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestArchiveRubricRefsAndLimit(t *testing.T) {
	fake := &katatest.FakeClient{}
	msgs := []string{"kata#abc4 demo#def5 other#abc4", "kata#abc4"}
	archiveRubricMessages(t.Context(), fake, "demo", msgs, "rubric.md", []byte("rubric\n"), func(err error) { assert.NoError(t, err) })
	assert.Equal(t, []byte("rubric\n"), fake.Archives["abc4"]["rubric.md"])
	assert.Equal(t, []byte("rubric\n"), fake.Archives["def5"]["rubric.md"])
	assert.Len(t, fake.Archives, 2)
}

func TestArchiveRubricIssueCap(t *testing.T) {
	fake := &katatest.FakeClient{}
	archiveRubricMessages(t.Context(), fake, "demo", []string{"kata#0001 kata#0002 kata#0003 kata#0004 kata#0005 kata#0006 kata#0007 kata#0008 kata#0009 kata#0010 kata#0011"}, "rubric.md", []byte("rubric\n"), func(err error) { assert.NoError(t, err) })
	assert.Len(t, fake.Archives, 10)
	assert.NotContains(t, fake.Archives, "0011")
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
		{"digest mismatch", base, true, []byte("other"), false},
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
	archiveRubric(t.Context(), fake, job, data, digest, report)
	name := fmt.Sprintf("roborev-rubric.custom.%s.md", digest[:16])
	assert.Equal(t, data, fake.Archives["def5"][name])
	assert.Len(t, fake.Archives, 1)
	job.GitRef = base
	archiveRubric(t.Context(), fake, job, data, digest, report)
	archiveRubric(t.Context(), fake, job, data, digest, report)
	assert.Equal(t, data, fake.Archives["abc4"][name])
	assert.Len(t, fake.Archives["abc4"], 1)
	newData := []byte("changed\n")
	newDigest := fmt.Sprintf("%x", sha256.Sum256(newData))
	archiveRubric(t.Context(), fake, job, newData, newDigest, report)
	assert.Len(t, fake.Archives["abc4"], 2)
	assert.Equal(t, newData, fake.Archives["abc4"][fmt.Sprintf("roborev-rubric.custom.%s.md", newDigest[:16])])
}

type blockedArchiveClient struct {
	*katatest.FakeClient
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	name     string
	data     []byte
}

func (f *blockedArchiveClient) ArchiveRubric(ctx context.Context, _, name string, data []byte) error {
	f.name = name
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
	tc.Pool.archiveClient = func(string) kata.Client { return fake }
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte("[review.types.custom]\ntemplate = 'rubric.md'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, "rubric.md"), []byte("check correctness\n"), 0o600))
	sha := tc.GitRepo.CommitFile("file.txt", "change", "change\n\nCloses: kata#abc4")
	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: "test", ReviewType: "custom"})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	id, events := tc.Broadcaster.Subscribe("")
	defer tc.Broadcaster.Unsubscribe(id)
	tc.Pool.processJob(testWorkerID, claimed)
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
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
	<-fake.started
	stored, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.RubricSHA256)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(fake.data)), *stored.RubricSHA256)
	assert.Equal(t, "roborev-rubric.custom."+(*stored.RubricSHA256)[:16]+".md", fake.name)
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
		{BindingResult: kata.Binding{Project: "demo"}, ArchiveErr: errors.New("docbank_not_configured")},
		{BindingResult: kata.Binding{Project: "demo"}, ArchiveErr: errors.New("unknown issue")},
		{BindingResult: kata.Binding{Project: "demo"}, ArchiveErr: context.DeadlineExceeded},
	} {
		var failures []error
		archiveRubric(t.Context(), fake, job, data, digest, func(err error) { failures = append(failures, err) })
		require.Len(t, failures, 1)
		assert.Error(t, failures[0])
	}
}
