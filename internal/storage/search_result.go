package storage

import "time"

// SearchCoverage reports point-in-time mirror and embedding completeness.
type SearchCoverage struct {
	MirrorComplete       bool   `json:"mirror_complete"`
	MirrorBacklog        *int64 `json:"mirror_backlog,omitempty"`
	EmbeddingsConfigured bool   `json:"embeddings_configured"`
	VectorState          string `json:"vector_state"`
	EmbeddingBacklog     int64  `json:"embedding_backlog"`
	Skipped              int64  `json:"skipped"`
}

// SearchHit identifies the canonical review member chosen for one group.
type SearchHit struct {
	JobID         int64     `json:"job_id"`
	JobUUID       string    `json:"job_uuid,omitempty"`
	ReviewID      int64     `json:"review_id"`
	ReviewUUID    string    `json:"review_uuid,omitempty"`
	RepoName      string    `json:"repo_name"`
	RepoPath      string    `json:"repo_path"`
	GitRef        string    `json:"git_ref"`
	CommitSHA     string    `json:"commit_sha,omitempty"`
	CommitSubject string    `json:"commit_subject,omitempty"`
	Branch        string    `json:"branch,omitempty"`
	ReviewType    string    `json:"review_type"`
	PanelRole     string    `json:"panel_role,omitempty"`
	Agent         string    `json:"agent"`
	Verdict       string    `json:"verdict,omitempty"`
	Closed        bool      `json:"closed"`
	FinishedAt    time.Time `json:"finished_at"`
	Score         float64   `json:"score"`
	MatchedIn     []string  `json:"matched_in"`
	Excerpt       string    `json:"excerpt"`
}

// SearchResponse is the transport-neutral review-search response.
type SearchResponse struct {
	Query          string         `json:"query"`
	Mode           string         `json:"mode"`
	Degraded       bool           `json:"degraded"`
	DegradedReason string         `json:"degraded_reason,omitempty"`
	Bounded        bool           `json:"bounded"`
	BoundedReason  string         `json:"bounded_reason,omitempty"`
	Partial        bool           `json:"partial"`
	Coverage       SearchCoverage `json:"coverage"`
	Hits           []SearchHit    `json:"hits"`
}
