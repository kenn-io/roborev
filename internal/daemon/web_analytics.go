package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/storage"
)

type WebAnalyticsInput struct {
	Since   string   `query:"since" doc:"Inclusive RFC 3339 finished-at bound; omit with an explicit until for all time"`
	Until   string   `query:"until" doc:"Exclusive RFC 3339 finished-at bound"`
	Project []string `query:"project,explode" doc:"Exact displayed project names (repeatable)"`
	Source  []string `query:"source,explode" doc:"Exact stored source values (repeatable)"`
	Agent   string   `query:"agent" doc:"Exact agent filter for attempt metrics"`
	Model   string   `query:"model" doc:"Exact model filter for attempt metrics"`
	Bucket  string   `query:"bucket" doc:"UTC time bucket: hour, day, week, or month"`
}

type WebAnalyticsOutput struct {
	Body *storage.AnalyticsSnapshot
}

func (s *Server) humaGetWebAnalytics(
	ctx context.Context, input *WebAnalyticsInput,
) (*WebAnalyticsOutput, error) {
	_ = ctx
	now := time.Now().UTC()
	until := now
	var since time.Time
	var err error
	if input.Until != "" {
		until, err = parseAnalyticsTime(input.Until, "until")
		if err != nil {
			return nil, err
		}
	}
	if input.Since != "" {
		since, err = parseAnalyticsTime(input.Since, "since")
		if err != nil {
			return nil, err
		}
	} else if input.Until == "" {
		since = until.Add(-30 * 24 * time.Hour)
	}
	if !since.IsZero() && !since.Before(until) {
		return nil, huma.Error400BadRequest("since must be before until")
	}

	bucket := storage.AnalyticsBucket(input.Bucket)
	if bucket == "" {
		bucket = automaticAnalyticsBucket(since, until)
	} else if !isWebAnalyticsBucket(bucket) {
		return nil, huma.Error400BadRequest("invalid analytics bucket")
	}
	opts := storage.AnalyticsOptions{
		Since: since, Until: until, Projects: input.Project, Sources: input.Source, Bucket: bucket,
	}
	if input.Agent != "" {
		opts.Agents = []string{input.Agent}
	}
	if input.Model != "" {
		opts.Models = []string{input.Model}
	}
	snapshot, err := s.db.GetAnalytics(opts)
	if err != nil {
		if errors.Is(err, storage.ErrAnalyticsRangeTooLarge) {
			return nil, huma.Error400BadRequest(
				"analytics range is too large for the selected time bucket",
			)
		}
		return nil, huma.Error500InternalServerError(fmt.Sprintf("get analytics: %v", err))
	}
	return &WebAnalyticsOutput{Body: snapshot}, nil
}

func isWebAnalyticsBucket(bucket storage.AnalyticsBucket) bool {
	switch bucket {
	case storage.AnalyticsBucketHour, storage.AnalyticsBucketDay,
		storage.AnalyticsBucketWeek, storage.AnalyticsBucketMonth:
		return true
	default:
		return false
	}
}

func parseAnalyticsTime(raw, name string) (time.Time, error) {
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, huma.Error400BadRequest(name + " must be RFC3339")
	}
	return value.UTC(), nil
}

func automaticAnalyticsBucket(since, until time.Time) storage.AnalyticsBucket {
	if since.IsZero() {
		return storage.AnalyticsBucketMonth
	}
	span := until.Sub(since)
	switch {
	case span <= 48*time.Hour:
		return storage.AnalyticsBucketHour
	case span <= 120*24*time.Hour:
		return storage.AnalyticsBucketDay
	case span <= 2*365*24*time.Hour:
		return storage.AnalyticsBucketWeek
	default:
		return storage.AnalyticsBucketMonth
	}
}
