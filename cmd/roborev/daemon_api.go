package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"

	"go.kenn.io/roborev/internal/storage"
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/jobs?id=%d", a.baseURL, jobID), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := a.client.Do(req)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/review?job_id=%d", a.baseURL, jobID), nil)
	if err != nil {
		return nil, fmt.Errorf("create %s request: %w", label, err)
	}

	resp, err := a.client.Do(req)
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
