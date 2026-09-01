package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"uuid"
)

const (
	ReviewUnitJob   = "job"
	ReviewUnitPanel = "panel"
)

// ExperimentAssignmentInput contains the immutable definition and assignment
// selected before a review unit is enqueued.
type ExperimentAssignmentInput struct {
	ExperimentID        string
	DefinitionHash      string
	DefinitionJSON      string
	Arm                 string
	SubjectHash         string
	EffectiveConfigHash string
	EffectiveConfigJSON string
}

// ExperimentAssignment is the structured attribution projected with reviews
// and exports. DefinitionJSON is intentionally not exposed.
type ExperimentAssignment struct {
	ID                  string `json:"id"`
	Arm                 string `json:"arm"`
	SubjectHash         string `json:"subject_hash"`
	DefinitionHash      string `json:"definition_hash"`
	EffectiveConfigHash string `json:"effective_config_hash"`
}

type SyncableExperimentDefinition struct {
	ExperimentID    string
	DefinitionHash  string
	DefinitionJSON  string
	FirstSeenAt     time.Time
	SourceMachineID uuid.UUID
}

type SyncableExperimentAssignment struct {
	ReviewUnitKind      string
	ReviewUnitUUID      uuid.UUID
	ExperimentID        string
	Arm                 string
	SubjectHash         string
	EffectiveConfigHash string
	EffectiveConfigJSON string
	AssignedAt          time.Time
	SourceMachineID     uuid.UUID
}

type experimentStore interface {
	execer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func validateExperimentAssignment(
	kind string, unitUUID uuid.UUID, experimentID, arm, subjectHash,
	effectiveConfigHash, effectiveConfigJSON string,
) error {
	if kind != ReviewUnitJob && kind != ReviewUnitPanel {
		return fmt.Errorf("invalid experiment review unit kind %q", kind)
	}
	if unitUUID == uuid.Nil() || experimentID == "" || subjectHash == "" ||
		effectiveConfigHash == "" || effectiveConfigJSON == "" {
		return errors.New("incomplete experiment assignment")
	}
	if arm != "default" && arm != "experiment" {
		return fmt.Errorf("invalid experiment arm %q", arm)
	}
	return nil
}

func insertExperimentAssignmentTx(
	ctx context.Context,
	exec experimentStore,
	kind string,
	unitUUID uuid.UUID,
	assignment *ExperimentAssignmentInput,
	machineID uuid.UUID,
	now time.Time,
) error {
	if assignment == nil {
		return nil
	}
	if assignment.DefinitionHash == "" || assignment.DefinitionJSON == "" {
		return errors.New("incomplete experiment assignment")
	}
	if err := validateExperimentAssignment(
		kind, unitUUID, assignment.ExperimentID, assignment.Arm,
		assignment.SubjectHash, assignment.EffectiveConfigHash,
		assignment.EffectiveConfigJSON,
	); err != nil {
		return err
	}
	nowText := now.Format(time.RFC3339)
	if _, err := exec.ExecContext(ctx, `
		INSERT INTO experiment_definitions (
			experiment_id, definition_hash, definition_json, first_seen_at, source_machine_id
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(experiment_id) DO NOTHING`,
		assignment.ExperimentID, assignment.DefinitionHash, assignment.DefinitionJSON, nowText, machineID,
	); err != nil {
		return fmt.Errorf("store experiment definition: %w", err)
	}
	var storedHash string
	if err := exec.QueryRowContext(ctx,
		`SELECT definition_hash FROM experiment_definitions WHERE experiment_id = ?`,
		assignment.ExperimentID,
	).Scan(&storedHash); err != nil {
		return fmt.Errorf("read experiment definition: %w", err)
	}
	if storedHash != assignment.DefinitionHash {
		return fmt.Errorf("experiment definition conflict for %q", assignment.ExperimentID)
	}

	var existingID, existingArm, existingSubject, existingConfig, existingConfigJSON string
	err := exec.QueryRowContext(ctx, `
		SELECT experiment_id, arm, subject_hash, effective_config_hash,
		       effective_config_json
		FROM experiment_assignments
		WHERE review_unit_kind = ? AND review_unit_uuid = ?
		LIMIT 1`, kind, unitUUID).Scan(
		&existingID, &existingArm, &existingSubject, &existingConfig,
		&existingConfigJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read experiment assignment: %w", err)
	}
	if err == nil && (existingID != assignment.ExperimentID ||
		existingArm != assignment.Arm || existingSubject != assignment.SubjectHash ||
		existingConfig != assignment.EffectiveConfigHash ||
		existingConfigJSON != assignment.EffectiveConfigJSON) {
		return fmt.Errorf("conflicting experiment assignment for %s/%s", kind, unitUUID)
	}
	if _, err := exec.ExecContext(ctx, `
		INSERT INTO experiment_assignments (
			review_unit_kind, review_unit_uuid, experiment_id, arm, subject_hash,
			effective_config_hash, effective_config_json, assigned_at, source_machine_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(review_unit_kind, review_unit_uuid, experiment_id) DO NOTHING`,
		kind, unitUUID, assignment.ExperimentID, assignment.Arm, assignment.SubjectHash,
		assignment.EffectiveConfigHash, assignment.EffectiveConfigJSON, nowText, machineID,
	); err != nil {
		return fmt.Errorf("store experiment assignment: %w", err)
	}
	return nil
}

// GetExperimentAssignments returns immutable attribution for one review unit.
func (db *DB) GetExperimentAssignments(kind string, unitUUID uuid.UUID) ([]ExperimentAssignment, error) {
	if unitUUID == uuid.Nil() {
		return nil, nil
	}
	rows, err := db.Query(`
		SELECT a.experiment_id, a.arm, a.subject_hash, d.definition_hash,
		       a.effective_config_hash
		FROM experiment_assignments a
		JOIN experiment_definitions d ON d.experiment_id = a.experiment_id
		WHERE a.review_unit_kind = ? AND a.review_unit_uuid = ?
		ORDER BY a.experiment_id`, kind, unitUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var assignments []ExperimentAssignment
	for rows.Next() {
		var assignment ExperimentAssignment
		if err := rows.Scan(
			&assignment.ID, &assignment.Arm, &assignment.SubjectHash,
			&assignment.DefinitionHash, &assignment.EffectiveConfigHash,
		); err != nil {
			return nil, err
		}
		assignments = append(assignments, assignment)
	}
	return assignments, rows.Err()
}

func (db *DB) GetExperimentAssignmentsForJobUUID(jobUUID uuid.UUID) ([]ExperimentAssignment, error) {
	if jobUUID == uuid.Nil() {
		return nil, nil
	}
	var panelRunUUID sql.Null[uuid.UUID]
	if err := db.QueryRow(`SELECT NULLIF(panel_run_uuid, '') FROM review_jobs WHERE uuid = ?`,
		jobUUID).Scan(&panelRunUUID); err != nil {
		return nil, err
	}
	if panelRunUUID.Valid {
		return db.GetExperimentAssignments(ReviewUnitPanel, panelRunUUID.V)
	}
	return db.GetExperimentAssignments(ReviewUnitJob, jobUUID)
}

// GetExperimentAssignmentInputForJobUUID returns the persisted assignment and
// immutable definition needed when the same review is re-enqueued. Nil means
// the review was not enrolled.
func (db *DB) GetExperimentAssignmentInputForJobUUID(jobUUID uuid.UUID) (*ExperimentAssignmentInput, error) {
	if jobUUID == uuid.Nil() {
		return nil, nil
	}
	var panelRunUUID sql.Null[uuid.UUID]
	if err := db.QueryRow(`SELECT NULLIF(panel_run_uuid, '') FROM review_jobs WHERE uuid = ?`,
		jobUUID).Scan(&panelRunUUID); err != nil {
		return nil, err
	}
	kind, unitUUID := ReviewUnitJob, jobUUID
	if panelRunUUID.Valid {
		kind, unitUUID = ReviewUnitPanel, panelRunUUID.V
	}
	var assignment ExperimentAssignmentInput
	err := db.QueryRow(`
		SELECT a.experiment_id, d.definition_hash, d.definition_json, a.arm,
		       a.subject_hash, a.effective_config_hash, a.effective_config_json
		FROM experiment_assignments a
		JOIN experiment_definitions d ON d.experiment_id = a.experiment_id
		WHERE a.review_unit_kind = ? AND a.review_unit_uuid = ?
		ORDER BY a.experiment_id
		LIMIT 1`, kind, unitUUID).Scan(
		&assignment.ExperimentID, &assignment.DefinitionHash,
		&assignment.DefinitionJSON, &assignment.Arm, &assignment.SubjectHash,
		&assignment.EffectiveConfigHash, &assignment.EffectiveConfigJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &assignment, nil
}

func (db *DB) attachExperimentAssignments(job *ReviewJob) error {
	if job == nil {
		return nil
	}
	if job.UUID == nil {
		return nil
	}
	kind, unitUUID := ReviewUnitJob, *job.UUID
	if job.PanelRunUUID != nil {
		kind, unitUUID = ReviewUnitPanel, *job.PanelRunUUID
	}
	assignments, err := db.GetExperimentAssignments(kind, unitUUID)
	if err != nil {
		return err
	}
	job.Experiments = assignments
	return nil
}

func (db *DB) attachPanelExperimentAssignments(members []*ReviewJob, synthesis *ReviewJob) error {
	if synthesis == nil || synthesis.PanelRunUUID == nil {
		return nil
	}
	assignments, err := db.GetExperimentAssignments(ReviewUnitPanel, *synthesis.PanelRunUUID)
	if err != nil {
		return err
	}
	synthesis.Experiments = assignments
	for _, member := range members {
		member.Experiments = append([]ExperimentAssignment(nil), assignments...)
	}
	return nil
}

func (db *DB) attachExperimentAssignmentsToJobs(jobs []ReviewJob) error {
	type cacheKey struct {
		kind string
		uuid uuid.UUID
	}
	cache := make(map[cacheKey][]ExperimentAssignment)
	for i := range jobs {
		if jobs[i].UUID == nil {
			continue
		}
		kind, unitUUID := ReviewUnitJob, *jobs[i].UUID
		if jobs[i].PanelRunUUID != nil {
			kind, unitUUID = ReviewUnitPanel, *jobs[i].PanelRunUUID
		}
		key := cacheKey{kind: kind, uuid: unitUUID}
		assignments, ok := cache[key]
		if !ok {
			var err error
			assignments, err = db.GetExperimentAssignments(kind, unitUUID)
			if err != nil {
				return err
			}
			cache[key] = assignments
		}
		jobs[i].Experiments = append([]ExperimentAssignment(nil), assignments...)
	}
	return nil
}

func (db *DB) GetExperimentDefinitionsToSync(machineID uuid.UUID) ([]SyncableExperimentDefinition, error) {
	rows, err := db.Query(`
		SELECT d.experiment_id, d.definition_hash, d.definition_json,
		       d.first_seen_at, d.source_machine_id
		FROM experiment_definitions d
		WHERE d.synced_at IS NULL
		  AND (d.source_machine_id = ? OR EXISTS (
		      SELECT 1
		      FROM experiment_assignments a
		      WHERE a.experiment_id = d.experiment_id
		        AND a.source_machine_id = ?
		        AND a.synced_at IS NULL
		  ))
		ORDER BY d.experiment_id`, machineID, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var definitions []SyncableExperimentDefinition
	for rows.Next() {
		var definition SyncableExperimentDefinition
		var firstSeen string
		if err := rows.Scan(&definition.ExperimentID, &definition.DefinitionHash,
			&definition.DefinitionJSON, &firstSeen, &definition.SourceMachineID); err != nil {
			return nil, err
		}
		definition.FirstSeenAt = parseSQLiteTime(firstSeen)
		definitions = append(definitions, definition)
	}
	return definitions, rows.Err()
}

func (db *DB) GetExperimentAssignmentsToSync(machineID uuid.UUID) ([]SyncableExperimentAssignment, error) {
	rows, err := db.Query(`
		SELECT review_unit_kind, review_unit_uuid, experiment_id, arm, subject_hash,
		       effective_config_hash, effective_config_json, assigned_at, source_machine_id
		FROM experiment_assignments
		WHERE source_machine_id = ? AND synced_at IS NULL
		ORDER BY assigned_at, review_unit_kind, review_unit_uuid`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var assignments []SyncableExperimentAssignment
	for rows.Next() {
		var assignment SyncableExperimentAssignment
		var assignedAt string
		if err := rows.Scan(&assignment.ReviewUnitKind, &assignment.ReviewUnitUUID,
			&assignment.ExperimentID, &assignment.Arm, &assignment.SubjectHash,
			&assignment.EffectiveConfigHash, &assignment.EffectiveConfigJSON,
			&assignedAt, &assignment.SourceMachineID); err != nil {
			return nil, err
		}
		assignment.AssignedAt = parseSQLiteTime(assignedAt)
		assignments = append(assignments, assignment)
	}
	return assignments, rows.Err()
}

func (db *DB) MarkExperimentDefinitionSynced(id string) error {
	_, err := db.Exec(`UPDATE experiment_definitions SET synced_at = ? WHERE experiment_id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (db *DB) MarkExperimentAssignmentSynced(assignment SyncableExperimentAssignment) error {
	_, err := db.Exec(`
		UPDATE experiment_assignments SET synced_at = ?
		WHERE review_unit_kind = ? AND review_unit_uuid = ? AND experiment_id = ?`,
		time.Now().UTC().Format(time.RFC3339), assignment.ReviewUnitKind,
		assignment.ReviewUnitUUID, assignment.ExperimentID)
	return err
}

func (db *DB) UpsertPulledExperimentDefinition(definition SyncableExperimentDefinition) error {
	_, err := db.Exec(`
		INSERT INTO experiment_definitions (
			experiment_id, definition_hash, definition_json, first_seen_at,
			source_machine_id, synced_at
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(experiment_id) DO NOTHING`,
		definition.ExperimentID, definition.DefinitionHash, definition.DefinitionJSON,
		definition.FirstSeenAt.Format(time.RFC3339), definition.SourceMachineID,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	var storedHash string
	if err := db.QueryRow(`SELECT definition_hash FROM experiment_definitions WHERE experiment_id = ?`,
		definition.ExperimentID).Scan(&storedHash); err != nil {
		return err
	}
	if storedHash != definition.DefinitionHash {
		return fmt.Errorf("experiment definition conflict for %q", definition.ExperimentID)
	}
	return nil
}

func (db *DB) UpsertPulledExperimentAssignment(assignment SyncableExperimentAssignment) error {
	if err := validateExperimentAssignment(
		assignment.ReviewUnitKind, assignment.ReviewUnitUUID,
		assignment.ExperimentID, assignment.Arm, assignment.SubjectHash,
		assignment.EffectiveConfigHash, assignment.EffectiveConfigJSON,
	); err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var existingID, existingArm, existingSubject, existingConfig, existingConfigJSON string
	err = tx.QueryRowContext(ctx, `
		SELECT experiment_id, arm, subject_hash, effective_config_hash,
		       effective_config_json
		FROM experiment_assignments
		WHERE review_unit_kind = ? AND review_unit_uuid = ?
		LIMIT 1`, assignment.ReviewUnitKind, assignment.ReviewUnitUUID).Scan(
		&existingID, &existingArm, &existingSubject, &existingConfig,
		&existingConfigJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (existingID != assignment.ExperimentID ||
		existingArm != assignment.Arm || existingSubject != assignment.SubjectHash ||
		existingConfig != assignment.EffectiveConfigHash ||
		existingConfigJSON != assignment.EffectiveConfigJSON) {
		return fmt.Errorf("conflicting experiment assignment for %s/%s",
			assignment.ReviewUnitKind, assignment.ReviewUnitUUID)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO experiment_assignments (
			review_unit_kind, review_unit_uuid, experiment_id, arm, subject_hash,
			effective_config_hash, effective_config_json, assigned_at, source_machine_id, synced_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(review_unit_kind, review_unit_uuid, experiment_id) DO NOTHING`,
		assignment.ReviewUnitKind, assignment.ReviewUnitUUID, assignment.ExperimentID,
		assignment.Arm, assignment.SubjectHash, assignment.EffectiveConfigHash,
		assignment.EffectiveConfigJSON, assignment.AssignedAt.Format(time.RFC3339), assignment.SourceMachineID,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	return tx.Commit()
}
