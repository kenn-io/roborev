package storage

import (
	"database/sql"
	"errors"
)

// FailedReviewAttemptState is the durable work associated with an unresolved
// review failure. All fields are read in the same SQLite snapshot.
type FailedReviewAttemptState struct {
	Attempt        ReviewAttempt
	Panel          *CIPanel
	Synthesis      *ReviewJob
	HasLiveMembers bool
	Members        []BatchReviewResult
}

// GetFailedReviewAttemptStates reads only the monitored repositories. A read
// transaction keeps delivery or retry finalization from producing an impossible
// combination of an old failed attempt and its newly posted or retired panel.
func (db *DB) GetFailedReviewAttemptStates(repos []string) ([]FailedReviewAttemptState, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var states []FailedReviewAttemptState
	for _, repo := range repos {
		attempts, err := getFailedReviewAttempts(tx, repo)
		if err != nil {
			return nil, err
		}
		for _, attempt := range attempts {
			state := FailedReviewAttemptState{Attempt: attempt}
			if attempt.State == "pending" {
				if err := readFailedReviewWork(tx, &state); err != nil {
					return nil, err
				}
			}
			states = append(states, state)
		}
	}
	return states, tx.Commit()
}

func readFailedReviewWork(q querier, state *FailedReviewAttemptState) error {
	a := state.Attempt
	panel, err := getActiveCIPanelByPRSHA(q, a.GithubRepo, a.PRNumber, a.HeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	state.Panel = panel
	var synth ReviewJob
	err = q.QueryRow(`SELECT status, agent, COALESCE(error, ''), claim_blocked
		FROM review_jobs WHERE panel_run_uuid = ? AND panel_role = 'synthesis'`, panel.PanelRunUUID).
		Scan(&synth.Status, &synth.Agent, &synth.Error, &synth.ClaimBlocked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	state.Synthesis = &synth
	if synth.Status == JobStatusQueued && synth.ClaimBlocked {
		return q.QueryRow(`SELECT EXISTS (SELECT 1 FROM review_jobs
			WHERE panel_run_uuid = ? AND panel_role = 'member' AND status IN ('queued', 'running'))`,
			panel.PanelRunUUID).Scan(&state.HasLiveMembers)
	}
	if synth.Status == JobStatusDone || synth.Status == JobStatusFailed {
		state.Members, err = getPanelMemberReviews(q, panel.PanelRunUUID)
	}
	return err
}
