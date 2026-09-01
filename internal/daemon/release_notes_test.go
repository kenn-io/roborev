package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-github/v90/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestReleaseNotesRefreshesEditedGitHubRelease(t *testing.T) {
	t.Parallel()

	const firstETag = `"release-list-v1"`
	requests := 0
	seenConditionalETag := ""
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		seenConditionalETag = request.Header.Get("If-None-Match")
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			w.Header().Set("ETag", firstETag)
			fmt.Fprint(w, `[{
				"tag_name":"v1.2.3","name":"Roborev 1.2.3","body":"Original notes",
				"html_url":"https://example.com/releases/v1.2.3","draft":false,
				"prerelease":false,"published_at":"2026-08-30T12:00:00Z",
				"updated_at":"2026-08-30T12:00:00Z"
			}]`)
			return
		}
		w.Header().Set("ETag", `"release-list-v2"`)
		fmt.Fprint(w, `[{
			"tag_name":"v1.2.3","name":"Roborev 1.2.3","body":"Edited notes",
			"html_url":"https://example.com/releases/v1.2.3","draft":false,
			"prerelease":false,"published_at":"2026-08-30T12:00:00Z",
			"updated_at":"2026-08-31T09:30:00Z"
		}]`)
	}))
	t.Cleanup(githubServer.Close)

	db, err := storage.Open(filepath.Join(t.TempDir(), "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	baseURL := githubServer.URL + "/"
	githubClient, err := github.NewClient(
		github.WithHTTPClient(githubServer.Client()),
		github.WithURLs(&baseURL, &baseURL),
	)
	require.NoError(t, err)
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	server := &Server{
		db: db, releaseNotesClient: githubClient, releaseNotesNow: func() time.Time { return now },
	}

	first, err := server.humaListReleases(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, first.Body.Releases, 1)
	assert.Equal(t, "Original notes", first.Body.Releases[0].Body)
	assert.Equal(t, 1, requests)

	_, err = server.humaListReleases(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, requests, "fresh cache avoids another GitHub request")

	now = now.Add(releaseNotesFreshFor + time.Minute)
	refreshed, err := server.humaListReleases(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, refreshed.Body.Releases, 1)
	assert.Equal(t, firstETag, seenConditionalETag)
	assert.Equal(t, "Edited notes", refreshed.Body.Releases[0].Body)
	assert.Equal(t, time.Date(2026, 8, 31, 9, 30, 0, 0, time.UTC), refreshed.Body.Releases[0].UpdatedAt)
}
