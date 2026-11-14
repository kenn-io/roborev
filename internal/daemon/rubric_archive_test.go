package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
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

	"go.kenn.io/roborev/internal/agent"
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

type aliasArchiveClient struct {
	*recordingArchiveClient
}

func (f *aliasArchiveClient) ResolveArchiveIssue(_ context.Context, _, ref string) (kata.ArchiveIssue, error) {
	if ref == "01arz3ndektsv4rrffq69g5fav" || ref == "abc4" {
		return kata.ArchiveIssue{UID: "01arz3ndektsv4rrffq69g5fav", ShortID: "abc4"}, nil
	}
	return kata.ArchiveIssue{}, errors.New("unknown issue")
}

func TestArchiveRubricDeduplicatesAliases(t *testing.T) {
	fake := &aliasArchiveClient{recordingArchiveClient: &recordingArchiveClient{FakeClient: &katatest.FakeClient{}}}
	archiveRubricMessages(t.Context(), fake, "demo", []string{"kata#abc4 kata#01arz3ndektsv4rrffq69g5fav"}, "rubric.md", []byte("rubric"), func(err error) { assert.NoError(t, err) })
	assert.Equal(t, []string{"01arz3ndektsv4rrffq69g5fav"}, fake.refs)
}

func TestArchiveRubricStopsForGlobalFailure(t *testing.T) {
	for _, failure := range []error{kata.ErrUnavailable, kata.ErrDaemonUnavailable, kata.ErrDaemonAPITooOld, kata.ErrDocbankNotConfigured} {
		fake := &katatest.FakeClient{ArchiveErr: failure}
		var failures []error
		archiveRubricMessages(t.Context(), fake, "demo", []string{"kata#abc4 kata#def5"}, "rubric.md", []byte("rubric"), func(err error) { failures = append(failures, err) })
		require.Len(t, failures, 1)
		require.ErrorIs(t, failures[0], failure)
		assert.Equal(t, []string{"abc4"}, fake.ArchiveRefs)
	}
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
	const agentName = "rubric-archive-structured"
	agent.RegisterForTest(t, &structuredWorkerTestAgent{
		name:   agentName,
		result: jsontext.Value(`{"schema_version":3,"summary":"No findings.","verdict":"pass","findings":[]}`),
	})
	cfg := config.DefaultConfig()
	cfg.KataRubrics.Mode = "current"
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	sha := tc.GitRepo.CommitFile("file.txt", "change", "change\n\nCloses: demo#abc4")
	worktree := filepath.Join(t.TempDir(), "worktree")
	tc.GitRepo.RunGit("worktree", "add", "--detach", worktree, sha)
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".kata.toml"), []byte("[project]\nname = 'main'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".kata.toml"), []byte("[project]\nname = 'demo'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".roborev.toml"), []byte("[review.types.custom]\ntemplate = 'rubric.md'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "rubric.md"), []byte("check correctness\n"), 0o600))
	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: agentName, ReviewType: "custom", WorktreePath: worktree})
	require.NoError(t, err)
	id, events := tc.Broadcaster.Subscribe("")
	defer tc.Broadcaster.Unsubscribe(id)

	synctest.Test(t, func(t *testing.T) {
		fake := &blockedArchiveClient{FakeClient: &katatest.FakeClient{}, started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
		var archivePath string
		tc.Pool.archiveClient = func(path string) kata.Client {
			archivePath = path
			binding, bindingErr := kata.NewCLIClient(path).Binding(t.Context())
			if bindingErr != nil {
				fake.BindingErr = bindingErr
			} else {
				fake.BindingResult = binding
			}
			return fake
		}
		tc.Pool.Start()
		released := false
		defer func() {
			if !released {
				close(fake.release)
			}
			tc.Pool.Stop()
		}()
		// Archive startup runs external Git subprocesses and reads SQLite.
		<-fake.started
		tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
		assert.Equal(t, worktree, archivePath)
		review, err := tc.DB.GetReviewByJobID(job.ID)
		require.NoError(t, err)
		require.NotNil(t, review)
		assert.Contains(t, review.Output, "No findings.")
		require.NotNil(t, review.VerdictBool)
		assert.Equal(t, 1, *review.VerdictBool)
		var completed bool
		for len(events) > 0 {
			event := <-events
			if event.Type == "review.completed" {
				completed = true
				assert.False(t, event.SuppressHooks)
				assert.False(t, event.SuppressRepoHooks)
			}
		}
		assert.True(t, completed)
		stored, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, tc.Repo.RootPath, stored.RepoPath)
		require.NotNil(t, stored.RubricSHA256)
		assert.Equal(t, []byte("check correctness\n"), fake.data)
		assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(fake.data)), *stored.RubricSHA256)

		stopped := make(chan struct{})
		go func() { tc.Pool.Stop(); close(stopped) }()
		synctest.Wait()
		var stoppedEarly bool
		select {
		case <-stopped:
			stoppedEarly = true
		default:
		}
		assert.False(t, stoppedEarly, "pool stopped while archive was blocked")
		close(fake.release)
		released = true
		<-fake.finished
		<-stopped
		tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	})
}

type panelArchiveRecord struct {
	ref, name string
	data      []byte
}

type panelArchiveClient struct {
	*katatest.FakeClient
	records chan panelArchiveRecord
}

func (f *panelArchiveClient) ArchiveRubric(_ context.Context, _, ref, name string, data []byte) error {
	f.records <- panelArchiveRecord{ref: ref, name: name, data: append([]byte(nil), data...)}
	return nil
}

func TestPanelMembersArchiveDistinctRubrics(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	const agentName = "panel-rubric-structured"
	agent.RegisterForTest(t, &structuredWorkerTestAgent{
		name:   agentName,
		result: jsontext.Value(`{"schema_version":3,"summary":"No findings.","verdict":"pass","findings":[]}`),
	})
	cfg := config.DefaultConfig()
	cfg.KataRubrics.Mode = "current"
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	tc.GitRepo.CommitFile("file", "change", "change\n\nCloses: demo#abc4")
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte("[review.types.alpha]\ntemplate = 'alpha.md'\n[review.types.beta]\ntemplate = 'beta.md'\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, "alpha.md"), []byte("  alpha checks  \n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, "beta.md"), []byte("beta checks\n\n"), 0o600))
	_, members, _ := enqueuePanelRun(t, tc, "pair", []memberSpec{
		{name: "alpha", agent: agentName, reviewType: "alpha"},
		{name: "beta", agent: agentName, reviewType: "beta"},
	})

	synctest.Test(t, func(t *testing.T) {
		fake := &panelArchiveClient{FakeClient: &katatest.FakeClient{BindingResult: kata.Binding{Project: "demo"}}, records: make(chan panelArchiveRecord)}
		tc.Pool.archiveClient = func(string) kata.Client { return fake }
		for i, expected := range []struct{ reviewType, content string }{{"alpha", "alpha checks\n"}, {"beta", "beta checks\n"}} {
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, members[i].ID, claimed.ID)
			tc.Pool.processJob(testWorkerID, claimed)
			// Attachment discovery runs external Git subprocesses.
			record := <-fake.records
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(expected.content)))
			assert.Equal(t, "abc4", record.ref)
			assert.Equal(t, fmt.Sprintf("roborev-rubric.%s.%s.md", expected.reviewType, digest[:16]), record.name)
			assert.Equal(t, []byte(expected.content), record.data)
			stored, err := tc.DB.GetJobByID(claimed.ID)
			require.NoError(t, err)
			require.NotNil(t, stored.RubricSHA256)
			assert.Equal(t, digest, *stored.RubricSHA256)
			tc.assertJobStatus(t, claimed.ID, storage.JobStatusDone)
		}
		tc.Pool.wg.Wait()
	})
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
