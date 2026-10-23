package katacontext

// KataFindingPublication exposes identifiers and delivery state only. Frozen
// review text, endpoint, credential and ledger payloads stay in the daemon.
type KataFindingPublication struct {
	ID          string `json:"id" doc:"Immutable finding selector"`
	Issue       string `json:"issue,omitempty"`
	IssueUID    string `json:"issue_uid,omitempty"`
	Comment     string `json:"comment,omitempty" doc:"Validated root comment UID"`
	State       string `json:"state"`
	Policy      string `json:"policy"`
	OperationID string `json:"operation_id"`
	LastError   string `json:"last_error,omitempty"`
}

// KataPublication describes one frozen publication, including issue-only
// publications for failed jobs and historical prose reviews.
type KataPublication struct {
	ReviewUUID  string                   `json:"review_uuid,omitempty"`
	OperationID string                   `json:"operation_id"`
	State       string                   `json:"state"`
	Policy      string                   `json:"policy"`
	Issue       string                   `json:"issue,omitempty"`
	IssueUID    string                   `json:"issue_uid,omitempty"`
	LastError   string                   `json:"last_error,omitempty"`
	Findings    []KataFindingPublication `json:"findings"`
}
