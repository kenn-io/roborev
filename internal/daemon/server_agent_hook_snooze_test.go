package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/testutil"
)

func TestAgentHookSnoozeAPIUpdatesResolveMetadata(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo := testutil.NewTestRepo(t)
	mainRoot, err := gitrepo.MainRoot(context.Background(), repo.Root)
	require.NoError(t, err)
	_, err = db.GetOrCreateRepo(mainRoot)
	require.NoError(t, err)
	until := time.Now().Add(2 * time.Hour).UTC().Round(0)

	setBody, err := json.Marshal(AgentHookSnoozeRequest{
		RepoPath:     mainRoot,
		WorktreePath: repo.Root,
		Branch:       "feature/api",
		Enabled:      true,
		SnoozedUntil: until,
	})
	require.NoError(t, err)
	set := serveHuma(
		t, server, http.MethodPost, "/api/agent-hook/snooze", setBody,
	)
	require.Equal(t, http.StatusOK, set.Code, set.Body.String())

	resolvePath := "/api/repos/resolve?path=" + url.QueryEscape(repo.Root) +
		"&branch=" + url.QueryEscape("feature/api")
	resolved := serveHuma(t, server, http.MethodGet, resolvePath, nil)
	require.Equal(t, http.StatusOK, resolved.Code)
	var metadata ResolveRepoOutput
	require.NoError(t, json.Unmarshal(resolved.Body.Bytes(), &metadata.Body))
	require.NotNil(t, metadata.Body.Repo)
	require.NotNil(t, metadata.Body.Repo.AgentHookSnoozedUntil)
	assert.Equal(until, *metadata.Body.Repo.AgentHookSnoozedUntil)

	other := serveHuma(
		t, server, http.MethodGet,
		"/api/repos/resolve?path="+url.QueryEscape(repo.Root)+"&branch=other",
		nil,
	)
	require.Equal(t, http.StatusOK, other.Code)
	var otherMetadata ResolveRepoOutput
	require.NoError(t, json.Unmarshal(other.Body.Bytes(), &otherMetadata.Body))
	require.NotNil(t, otherMetadata.Body.Repo)
	assert.Nil(otherMetadata.Body.Repo.AgentHookSnoozedUntil)

	offBody, err := json.Marshal(AgentHookSnoozeRequest{
		RepoPath:     mainRoot,
		WorktreePath: repo.Root,
		Branch:       "feature/api",
		Enabled:      false,
	})
	require.NoError(t, err)
	off := serveHuma(
		t, server, http.MethodPost, "/api/agent-hook/snooze", offBody,
	)
	require.Equal(t, http.StatusOK, off.Code, off.Body.String())

	resumed := serveHuma(t, server, http.MethodGet, resolvePath, nil)
	require.Equal(t, http.StatusOK, resumed.Code)
	var resumedMetadata ResolveRepoOutput
	require.NoError(t, json.Unmarshal(resumed.Body.Bytes(), &resumedMetadata.Body))
	require.NotNil(t, resumedMetadata.Body.Repo)
	assert.Nil(resumedMetadata.Body.Repo.AgentHookSnoozedUntil)
}
