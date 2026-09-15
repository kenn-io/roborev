package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"

	"go.kenn.io/roborev/internal/storage"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

var errReviewNotFound = errors.New("review not found")

type daemonReviewAPI struct {
	baseURL string
	client  *http.Client
}

func newDaemonReviewAPI(baseURL string, client *http.Client) daemonReviewAPI {
	return daemonReviewAPI{baseURL: baseURL, client: client}
}

func (a daemonReviewAPI) getJob(ctx context.Context, jobID int64) (*storage.ReviewJob, error) {
	resp, err := newDaemonAPI(a.baseURL, a.client).ListJobsRaw(ctx, &generated.ListJobsRequestOptions{Query: &generated.ListJobsQuery{ID: &jobID}})
	if err != nil {
		return nil, fmt.Errorf("fetch job: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch job (%d): %s", resp.StatusCode, body)
	}

	var result struct {
		Jobs []storage.ReviewJob `json:"jobs"`
	}
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("parse job: %w", err)
	}
	if len(result.Jobs) == 0 {
		return nil, fmt.Errorf("%w: %d", ErrJobNotFound, jobID)
	}
	return &result.Jobs[0], nil
}

func (a daemonReviewAPI) getReview(ctx context.Context, jobID int64, label string) (*storage.Review, error) {
	resp, err := newDaemonAPI(a.baseURL, a.client).GetReviewRaw(ctx, &generated.GetReviewRequestOptions{Query: &generated.GetReviewQuery{JobID: &jobID}})
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", label, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: job %d", errReviewNotFound, jobID)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch %s (%d): %s", label, resp.StatusCode, body)
	}

	var review storage.Review
	if err := json.UnmarshalRead(resp.Body, &review); err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	return &review, nil
}

func newDaemonAPI(baseURL string, httpClient *http.Client) *roborevclient.Client {
	api, err := roborevclient.NewWithHTTPClient(baseURL, httpClient)
	if err != nil {
		panic(fmt.Sprintf("create daemon API client: %v", err))
	}
	return api
}
