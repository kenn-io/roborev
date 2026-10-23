package mcpserver

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strconv"

	"go.kenn.io/roborev/internal/katacontext"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/storage"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

func (b *HTTPBackend) KataIssues(ctx context.Context, in KataReadInput) (KataIssuesResult, error) {
	query := url.Values{"job_id": {strconv.FormatInt(in.JobID, 10)}, "review_uuid": {in.ReviewUUID}}
	var out KataIssuesResult
	err := b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.KataPublicationsRaw(ctx, nil, roborevclient.WithQuery(query))
	}, &out)
	return out, err
}

func (b *HTTPBackend) KataContext(ctx context.Context, in KataReadInput, runID string) (katacontext.KataReplyContext, error) {
	raw, err := json.Marshal(struct {
		KataReadInput
		RunID   string `json:"run_id"`
		Purpose string `json:"purpose"`
	}{in, runID, "mcp"})
	if err != nil {
		return katacontext.KataReplyContext{}, err
	}
	var out katacontext.KataReplyContext
	err = b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.KataConsumeContextRaw(ctx, nil, roborevclient.WithBody(raw))
	}, &out)
	return out, err
}

func (b *HTTPBackend) KataReply(ctx context.Context, in kataresponse.Request) (storage.KataResponseStatus, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return storage.KataResponseStatus{}, err
	}
	var out storage.KataResponseStatus
	err = b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.KataResponseRaw(ctx, nil, roborevclient.WithBody(raw))
	}, &out)
	return out, err
}
