package searchindex

import "time"

// HealthSnapshot is a sanitized, point-in-time view of reconciliation.
type HealthSnapshot struct {
	Indexed              int64
	MirrorComplete       bool
	MirrorBacklog        *int64
	EmbeddingsConfigured bool
	VectorState          string
	Embedded             int64
	Skipped              int64
	EmbeddingBacklog     int64
	Generation           string
	ActiveGeneration     string
	LastSuccessAt        *time.Time
	LastProgressAt       *time.Time
	RatePerSecond        *float64
	ETASeconds           *int64
	LastError            string
	LastErrorStatus      int
}

func cloneHealth(value HealthSnapshot) HealthSnapshot {
	value.LastSuccessAt = cloneTime(value.LastSuccessAt)
	value.LastProgressAt = cloneTime(value.LastProgressAt)
	if value.MirrorBacklog != nil {
		backlog := *value.MirrorBacklog
		value.MirrorBacklog = &backlog
	}
	if value.RatePerSecond != nil {
		rate := *value.RatePerSecond
		value.RatePerSecond = &rate
	}
	if value.ETASeconds != nil {
		eta := *value.ETASeconds
		value.ETASeconds = &eta
	}
	return value
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
