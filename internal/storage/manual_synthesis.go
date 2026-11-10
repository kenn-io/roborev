package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"uuid"
)

// The mode stays in the synthesis snapshot when the gate opens, so reruns
// and synchronized history retain the caller's choice.
const panelSynthesisHeld = `COALESCE(json_extract(NULLIF(panel_member_config_json, ''), '$.hold_synthesis'), 0)`

const panelVotingMembersTerminal = `NOT EXISTS (
	SELECT 1 FROM review_jobs m
	WHERE m.panel_run_uuid = review_jobs.panel_run_uuid
	  AND m.panel_role = 'member'
	  AND COALESCE(m.non_voting, 0) = 0
	  AND m.status NOT IN ('done','failed','canceled','skipped','applied','rebased')
)`

var ErrPanelSynthesisNotReady = errors.New("synthesis requires a held, uncanceled panel with all voting members finished")

// StartPanelSynthesis opens a held gate once. Replays return the same job;
// they never rerun a finished synthesis or revive a canceled panel.
func (db *DB) StartPanelSynthesis(ctx context.Context, run uuid.UUID) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `
		UPDATE review_jobs
		SET updated_at = CASE WHEN claim_blocked = 1 THEN ? ELSE updated_at END,
		    claim_blocked = 0
		WHERE panel_run_uuid = ? AND panel_role = 'synthesis'
		  AND status != 'canceled'
		  AND `+panelSynthesisHeld+` = 1
		  AND `+panelVotingMembersTerminal+`
		RETURNING id`, time.Now().Format(time.RFC3339), run).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Distinguish a missing panel from one that cannot be released.
		if err := db.QueryRowContext(ctx, `SELECT id FROM review_jobs
			WHERE panel_run_uuid = ? AND panel_role = 'synthesis'`, run).Scan(&id); err != nil {
			return 0, err
		}
		return 0, ErrPanelSynthesisNotReady
	}
	if err != nil {
		return 0, fmt.Errorf("start panel synthesis: %w", err)
	}
	return id, nil
}
