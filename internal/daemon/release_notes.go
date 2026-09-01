package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/go-github/v90/github"

	"go.kenn.io/roborev/internal/storage"
)

const (
	releaseNotesFreshFor = time.Hour
	releaseNotesLimit    = 10
)

type ReleaseNotesResponse struct {
	Releases  []storage.ReleaseNote `json:"releases"`
	FetchedAt time.Time             `json:"fetched_at"`
	Stale     bool                  `json:"stale"`
}

type releaseNotesOutput struct {
	Body ReleaseNotesResponse
}

func (s *Server) humaListReleases(ctx context.Context, _ *struct{}) (*releaseNotesOutput, error) {
	cache, err := s.db.GetReleaseNotesCache(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error500InternalServerError("read cached release notes")
	}
	now := s.releaseNotesNow()
	if cache != nil && now.Sub(cache.FetchedAt) < releaseNotesFreshFor {
		return releaseNotesCacheOutput(cache, false), nil
	}

	releases, etag, notModified, fetchErr := s.fetchReleaseNotes(ctx, cache)
	if fetchErr != nil {
		if cache != nil {
			return releaseNotesCacheOutput(cache, true), nil
		}
		return nil, huma.Error502BadGateway("fetch release notes from GitHub")
	}
	if notModified {
		if err := s.db.TouchReleaseNotesCache(ctx, now); err != nil {
			return nil, huma.Error500InternalServerError("update release notes cache")
		}
		cache.FetchedAt = now
		return releaseNotesCacheOutput(cache, false), nil
	}

	cache = &storage.ReleaseNotesCache{ETag: etag, Releases: releases, FetchedAt: now}
	if err := s.db.SaveReleaseNotesCache(ctx, *cache); err != nil {
		return nil, huma.Error500InternalServerError("save release notes cache")
	}
	return releaseNotesCacheOutput(cache, false), nil
}

func releaseNotesCacheOutput(cache *storage.ReleaseNotesCache, stale bool) *releaseNotesOutput {
	return &releaseNotesOutput{Body: ReleaseNotesResponse{
		Releases: cache.Releases, FetchedAt: cache.FetchedAt, Stale: stale,
	}}
}

func (s *Server) fetchReleaseNotes(
	ctx context.Context,
	cache *storage.ReleaseNotesCache,
) ([]storage.ReleaseNote, string, bool, error) {
	path := fmt.Sprintf("repos/kenn-io/roborev/releases?per_page=%d", releaseNotesLimit)
	req, err := s.releaseNotesClient.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", false, fmt.Errorf("create release notes request: %w", err)
	}
	if cache != nil && cache.ETag != "" {
		req.Header.Set("If-None-Match", cache.ETag)
	}

	var remote []*github.RepositoryRelease
	resp, err := s.releaseNotesClient.Do(req, &remote)
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return nil, "", true, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("list GitHub releases: %w", err)
	}

	releases := make([]storage.ReleaseNote, 0, len(remote))
	for _, release := range remote {
		if release == nil || release.GetDraft() || release.PublishedAt == nil {
			continue
		}
		publishedAt := release.PublishedAt.Time
		updatedAt := publishedAt
		if release.UpdatedAt != nil {
			updatedAt = release.UpdatedAt.Time
		}
		name := strings.TrimSpace(release.GetName())
		if name == "" {
			name = release.GetTagName()
		}
		releases = append(releases, storage.ReleaseNote{
			TagName: release.GetTagName(), Name: name, Body: release.GetBody(),
			HTMLURL: release.GetHTMLURL(), PublishedAt: publishedAt,
			UpdatedAt: updatedAt, Prerelease: release.GetPrerelease(),
		})
	}
	return releases, resp.Header.Get("ETag"), false, nil
}
