package storage

import (
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// dbTime is the shared nullable timestamp representation for Bun rows.
// PostgreSQL returns native time.Time values while SQLite returns text.
type dbTime struct {
	Time  time.Time
	Valid bool
}

func (t *dbTime) Scan(value any) error {
	switch value := value.(type) {
	case nil:
		*t = dbTime{}
		return nil
	case time.Time:
		t.Time = value
		t.Valid = true
		return nil
	case string:
		return t.scanString(value)
	case []byte:
		return t.scanString(string(value))
	default:
		return fmt.Errorf("scan database time from %T", value)
	}
}

func (t *dbTime) scanString(value string) error {
	if value == "" {
		*t = dbTime{}
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			t.Time = parsed
			t.Valid = true
			return nil
		}
	}
	// Legacy SQLite readers tolerate non-empty, unrecognized timestamp text:
	// parseSQLiteTime logs the value and returns zero time. Preserve that
	// behavior during the staged Bun conversion so old databases remain
	// readable while native PostgreSQL timestamps still use the time.Time path.
	t.Time = parseSQLiteTime(value)
	t.Valid = true
	return nil
}

func (t dbTime) Value() (driver.Value, error) {
	if !t.Valid {
		return nil, nil
	}
	return t.Time.UTC().Format(time.RFC3339Nano), nil
}

func dbTimeFromValue(value time.Time) dbTime {
	return dbTime{Time: value, Valid: true}
}

func dbTimeFromPointer(value *time.Time) dbTime {
	if value == nil {
		return dbTime{}
	}
	return dbTime{Time: *value, Valid: true}
}

func (t dbTime) pointer() *time.Time {
	if !t.Valid {
		return nil
	}
	value := t.Time
	return &value
}

// dbRetryTime is the local-only retry gate timestamp. Unlike general
// timestamps, SQLite compares retry_not_before lexicographically, so writes
// must remain fixed-width and UTC.
type dbRetryTime struct {
	Time  time.Time
	Valid bool
}

func dbRetryTimeFromValue(value time.Time) dbRetryTime {
	return dbRetryTime{Time: value, Valid: true}
}

func (t *dbRetryTime) Scan(value any) error {
	var scanned dbTime
	if err := scanned.Scan(value); err != nil {
		return err
	}
	t.Time = scanned.Time
	t.Valid = scanned.Valid
	return nil
}

func (t dbRetryTime) Value() (driver.Value, error) {
	if !t.Valid {
		return nil, nil
	}
	return retryNotBeforeAt(t.Time), nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type repoRow struct {
	bun.BaseModel `bun:"table:repos,alias:r"`
	ID            int64   `bun:"id,pk,autoincrement"`
	RootPath      string  `bun:"root_path"`
	Name          string  `bun:"name"`
	Identity      *string `bun:"identity"`
	CreatedAt     dbTime  `bun:"created_at"`
}

func (row repoRow) toModel() Repo {
	return Repo{
		ID:        row.ID,
		RootPath:  row.RootPath,
		Name:      row.Name,
		CreatedAt: row.CreatedAt.Time,
		Identity:  stringValue(row.Identity),
	}
}

type commitRow struct {
	bun.BaseModel `bun:"table:commits,alias:c"`
	ID            int64  `bun:"id,pk,autoincrement"`
	RepoID        int64  `bun:"repo_id"`
	SHA           string `bun:"sha"`
	Author        string `bun:"author"`
	Subject       string `bun:"subject"`
	Timestamp     dbTime `bun:"timestamp"`
	CreatedAt     dbTime `bun:"created_at"`
}

type pgSyncMetadataRow struct {
	bun.BaseModel `bun:"table:sync_metadata,alias:sm"`
	Key           string `bun:"key,pk"`
	Value         string `bun:"value"`
}

type pgMachineRow struct {
	bun.BaseModel `bun:"table:machines,alias:m"`
	MachineID     string `bun:"machine_id,pk"`
	Name          string `bun:"name"`
	LastSeenAt    dbTime `bun:"last_seen_at"`
}

func (row commitRow) toModel() Commit {
	return Commit{
		ID:        row.ID,
		RepoID:    row.RepoID,
		SHA:       row.SHA,
		Author:    row.Author,
		Subject:   row.Subject,
		Timestamp: row.Timestamp.Time,
		CreatedAt: row.CreatedAt.Time,
	}
}
