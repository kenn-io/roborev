package storage

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetOrCreateRepoByIdentity(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	t.Run("creates placeholder repo for local identity", func(t *testing.T) {
		localIdentity := "local:my-local-project"

		// Create repo by identity
		repoID, err := db.GetOrCreateRepoByIdentity(localIdentity)
		require.NoError(t, err, "GetOrCreateRepoByIdentity failed: %v")

		assert.NotEqual(t, 0, repoID)

		// Verify repo has correct fields
		var rootPath, name, identity string
		err = db.QueryRow(`SELECT root_path, name, identity FROM repos WHERE id = ?`, repoID).Scan(&rootPath, &name, &identity)
		require.NoError(t, err, "Query repo failed: %v")

		assert.Equal(t, rootPath, localIdentity)
		// Name is extracted from identity
		assert.Equal(t, "my-local-project", name)
		assert.Equal(t, identity, localIdentity)
	})

	t.Run("returns same ID on subsequent calls", func(t *testing.T) {
		localIdentity := "local:another-project"

		id1, err := db.GetOrCreateRepoByIdentity(localIdentity)
		require.NoError(t, err, "First GetOrCreateRepoByIdentity failed: %v")

		id2, err := db.GetOrCreateRepoByIdentity(localIdentity)
		require.NoError(t, err, "Second GetOrCreateRepoByIdentity failed: %v")

		assert.Equal(t, id1, id2)
	})

	t.Run("creates different repos for different identities", func(t *testing.T) {
		id1, err := db.GetOrCreateRepoByIdentity("local:project-a")
		require.NoError(t, err, "First GetOrCreateRepoByIdentity failed: %v")

		id2, err := db.GetOrCreateRepoByIdentity("local:project-b")
		require.NoError(t, err, "Second GetOrCreateRepoByIdentity failed: %v")

		assert.NotEqual(t, id1, id2)
	})

	t.Run("works with git URL identities too", func(t *testing.T) {
		gitIdentity := "https://github.com/user/repo.git"

		repoID, err := db.GetOrCreateRepoByIdentity(gitIdentity)
		require.NoError(t, err, "GetOrCreateRepoByIdentity failed: %v")

		// Verify identity is set correctly and name is extracted
		var identity, name string
		err = db.QueryRow(`SELECT identity, name FROM repos WHERE id = ?`, repoID).Scan(&identity, &name)
		require.NoError(t, err, "Query repo failed: %v")

		assert.Equal(t, identity, gitIdentity)
		assert.Equal(t, "repo", name)
	})

	t.Run("reuses single local repo when one exists", func(t *testing.T) {
		// When exactly one local repo has the identity, use it directly (no placeholder)
		singleIdentity := "git@github.com:org/single-clone-repo.git"

		// Create one local clone with the identity
		result, err := db.Exec(`INSERT INTO repos (root_path, name, identity) VALUES (?, ?, ?)`,
			"/home/user/single-clone", "single-clone", singleIdentity)
		require.NoError(t, err, "Insert single clone failed: %v")

		localRepoID, _ := result.LastInsertId()

		// GetOrCreateRepoByIdentity should return the existing local repo, not create a placeholder
		gotID, err := db.GetOrCreateRepoByIdentity(singleIdentity)
		require.NoError(t, err, "GetOrCreateRepoByIdentity failed: %v")

		assert.Equal(t, gotID, localRepoID)

		// Verify no placeholder was created
		var count int
		err = db.QueryRow(`SELECT COUNT(*) FROM repos WHERE root_path = ?`, singleIdentity).Scan(&count)
		require.NoError(t, err, "Count query failed: %v")

		assert.Equal(t, 0, count)
	})

	t.Run("creates placeholder when multiple local clones exist", func(t *testing.T) {
		// When multiple local clones have the same identity, create a placeholder
		sharedIdentity := "git@github.com:org/shared-repo.git"

		// Create two local clones with the same identity
		_, err := db.Exec(`INSERT INTO repos (root_path, name, identity) VALUES (?, ?, ?)`,
			"/home/user/clone-1", "clone-1", sharedIdentity)
		require.NoError(t, err, "Insert clone-1 failed: %v")

		_, err = db.Exec(`INSERT INTO repos (root_path, name, identity) VALUES (?, ?, ?)`,
			"/home/user/clone-2", "clone-2", sharedIdentity)
		require.NoError(t, err, "Insert clone-2 failed: %v")

		// GetOrCreateRepoByIdentity should create a placeholder
		placeholderID, err := db.GetOrCreateRepoByIdentity(sharedIdentity)
		require.NoError(t, err, "GetOrCreateRepoByIdentity should succeed with duplicates, got: %v")

		// Verify it created a placeholder (root_path == identity)
		var rootPath, name string
		err = db.QueryRow(`SELECT root_path, name FROM repos WHERE id = ?`, placeholderID).Scan(&rootPath, &name)
		require.NoError(t, err, "Query placeholder failed: %v")

		assert.Equal(t, rootPath, sharedIdentity)
		assert.Equal(t, "shared-repo", name)

		// Subsequent calls should return the same placeholder
		placeholderID2, err := db.GetOrCreateRepoByIdentity(sharedIdentity)
		require.NoError(t, err, "Second GetOrCreateRepoByIdentity failed: %v")

		assert.Equal(t, placeholderID, placeholderID2)
	})

	t.Run("prefers single local repo over existing placeholder", func(t *testing.T) {
		// This tests the fix for review #2658: when a placeholder exists from
		// a previous sync (e.g., when there were 0 clones), but now there's
		// exactly one local repo, we should prefer the local repo.
		placeholderIdentity := "git@github.com:org/placeholder-then-clone.git"

		// First, create a placeholder (simulates sync with no local clone)
		_, err := db.Exec(`INSERT INTO repos (root_path, name, identity) VALUES (?, ?, ?)`,
			placeholderIdentity, "placeholder-then-clone", placeholderIdentity)
		require.NoError(t, err, "Insert placeholder failed: %v")

		// Now create a single local clone (user cloned the repo after syncing)
		result, err := db.Exec(`INSERT INTO repos (root_path, name, identity) VALUES (?, ?, ?)`,
			"/home/user/new-clone", "new-clone", placeholderIdentity)
		require.NoError(t, err, "Insert local clone failed: %v")

		localRepoID, _ := result.LastInsertId()

		// GetOrCreateRepoByIdentity should return the local repo, not the placeholder
		gotID, err := db.GetOrCreateRepoByIdentity(placeholderIdentity)
		require.NoError(t, err, "GetOrCreateRepoByIdentity failed: %v")

		assert.Equal(t, gotID, localRepoID)
	})
}

func TestExtractRepoNameFromIdentity(t *testing.T) {
	tests := []struct {
		identity string
		expected string
	}{
		// SSH format
		{"git@github.com:org/repo.git", "repo"},
		{"git@github.com:user/my-project.git", "my-project"},
		{"git@gitlab.com:group/subgroup/repo.git", "repo"},

		// HTTPS format
		{"https://github.com/org/repo.git", "repo"},
		{"https://github.com/org/repo", "repo"},
		{"https://gitlab.com/group/subgroup/project.git", "project"},

		// Local format
		{"local:my-project", "my-project"},
		{"local:another-repo", "another-repo"},

		// Edge cases
		{"repo.git", "repo"},
		{"repo", "repo"},
		{"", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.identity, func(t *testing.T) {
			got := ExtractRepoNameFromIdentity(tt.identity)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestSetRepoIdentity(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Create test repo
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	// Set identity
	err = db.SetRepoIdentity(repo.ID, "https://github.com/user/repo.git")
	require.NoError(t, err, "SetRepoIdentity failed: %v")

	// Verify via GetRepoByIdentity
	found, err := db.GetRepoByIdentity("https://github.com/user/repo.git")
	require.NoError(t, err, "GetRepoByIdentity failed: %v")

	assert.NotNil(t, found)
	assert.Equal(t, found.ID, repo.ID)
	assert.Equal(t, "https://github.com/user/repo.git", found.Identity)
}

func TestGetRepoByIdentity_NotFound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	found, err := db.GetRepoByIdentity("nonexistent")
	require.NoError(t, err, "GetRepoByIdentity failed: %v")

	assert.Nil(t, found)
}

func TestGetRepoByIdentity_DuplicateError(t *testing.T) {
	// This test verifies GetRepoByIdentity returns an error if duplicates exist.
	// Multiple repos can share the same identity (e.g., multiple clones of the same remote),
	// but GetRepoByIdentity should return an error when asked to find a unique repo.
	db := openTestDB(t)
	defer db.Close()

	// Create two repos with same identity (simulates two clones of the same remote)
	_, err := db.Exec(`INSERT INTO repos (root_path, name, identity) VALUES ('/path1', 'repo1', 'same-id')`)
	require.NoError(t, err, "Failed to insert repo1: %v")

	_, err = db.Exec(`INSERT INTO repos (root_path, name, identity) VALUES ('/path2', 'repo2', 'same-id')`)
	require.NoError(t, err, "Failed to insert repo2: %v")

	// GetRepoByIdentity should return error for duplicates
	_, err = db.GetRepoByIdentity("same-id")
	require.Error(t, err)
	assert.True(t, regexp.MustCompile(`multiple repos found`).MatchString(err.Error()))
}

func TestCommitsMigration_SameSHADifferentRepos(t *testing.T) {
	// This test creates an old-schema database manually, runs migration,
	// and verifies that the same SHA can now exist in different repos.
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Create database with old schema (sha TEXT UNIQUE NOT NULL)
	rawDB, err := openRawDB(dbPath)
	require.NoError(t, err, "Failed to open raw database: %v")

	// Create old schema with UNIQUE(sha) constraint
	setupLegacySchema(t, rawDB, legacySchemaV1DDL)

	// Insert two repos
	_, err = rawDB.Exec(`INSERT INTO repos (root_path, name) VALUES ('/repo1', 'repo1')`)
	if err != nil {
		rawDB.Close()
		require.NoError(t, err, "Failed to insert repo1: %v")
	}
	_, err = rawDB.Exec(`INSERT INTO repos (root_path, name) VALUES ('/repo2', 'repo2')`)
	if err != nil {
		rawDB.Close()
		require.NoError(t, err, "Failed to insert repo2: %v")
	}

	// Insert a commit in repo1
	_, err = rawDB.Exec(`INSERT INTO commits (repo_id, sha, author, subject, timestamp) VALUES (1, 'abc123', 'Author', 'Subject', '2024-01-01T00:00:00Z')`)
	if err != nil {
		rawDB.Close()
		require.NoError(t, err, "Failed to insert commit in repo1: %v")
	}

	// Verify old schema prevents same SHA in different repo
	_, err = rawDB.Exec(`INSERT INTO commits (repo_id, sha, author, subject, timestamp) VALUES (2, 'abc123', 'Author', 'Subject', '2024-01-01T00:00:00Z')`)
	if err == nil {
		rawDB.Close()
		require.Error(t, err, "Expected error inserting duplicate SHA in old schema, but got none")
	}

	rawDB.Close()

	// Now open with our storage.Open which runs migrations
	db, err := Open(dbPath)
	require.NoError(t, err, "Failed to open database with migrations: %v")

	defer db.Close()

	// After migration, same SHA in different repo should work
	_, err = db.Exec(`INSERT INTO commits (repo_id, sha, author, subject, timestamp) VALUES (2, 'abc123', 'Author', 'Subject', '2024-01-01T00:00:00Z')`)
	require.NoError(t, err, "After migration, same SHA in different repo should succeed: %v")

	// Verify both commits exist
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM commits WHERE sha = 'abc123'`).Scan(&count)
	require.NoError(t, err, "Failed to count commits: %v")

	assert.Equal(t, 2, count)

	// Verify duplicate in same repo is still rejected
	_, err = db.Exec(`INSERT INTO commits (repo_id, sha, author, subject, timestamp) VALUES (1, 'abc123', 'Author', 'Subject', '2024-01-01T00:00:00Z')`)
	require.Error(t, err, "Expected error inserting duplicate SHA in same repo, but got none")
}

func TestDuplicateRepoIdentity_MigrationSuccess(t *testing.T) {
	// This test verifies that migration succeeds even when duplicate
	// repos.identity values exist. Multiple clones of the same repo
	// should be allowed (fix for https://github.com/roborev-dev/roborev/issues/131).
	dbPath := filepath.Join(t.TempDir(), "test.db")

	rawDB, err := openRawDB(dbPath)
	require.NoError(t, err, "Failed to open raw database: %v")

	// Create schema with identity column but no index (simulates partial migration)
	setupLegacySchema(t, rawDB, legacySchemaV2DDL)

	// Insert two repos with the same identity (e.g., two clones of same remote)
	_, err = rawDB.Exec(`INSERT INTO repos (root_path, name, identity) VALUES ('/repo1', 'repo1', 'git@github.com:org/repo.git')`)
	if err != nil {
		rawDB.Close()
		require.NoError(t, err, "Failed to insert repo1: %v")
	}
	_, err = rawDB.Exec(`INSERT INTO repos (root_path, name, identity) VALUES ('/repo2', 'repo2', 'git@github.com:org/repo.git')`)
	if err != nil {
		rawDB.Close()
		require.NoError(t, err, "Failed to insert repo2: %v")
	}

	rawDB.Close()

	// Now open with storage.Open which runs migrations - should succeed
	db, err := Open(dbPath)
	require.NoError(t, err, "Expected migration to succeed with duplicate identities, but got error: %v")

	defer db.Close()

	// Verify both repos exist
	repos, err := db.ListRepos()
	require.NoError(t, err, "ListRepos failed: %v")

	assert.Len(t, repos, 2)
}

func TestUniqueIndexMigration(t *testing.T) {
	// This test verifies that an existing database with the old UNIQUE index
	// on repos.identity is properly migrated to a non-unique index.
	// See: https://github.com/roborev-dev/roborev/issues/131
	dbPath := filepath.Join(t.TempDir(), "test.db")

	rawDB, err := openRawDB(dbPath)
	require.NoError(t, err, "Failed to open raw database: %v")

	// Create schema with identity column AND the old unique index
	setupLegacySchema(t, rawDB, legacySchemaV3DDL)

	// Insert one repo
	_, err = rawDB.Exec(`INSERT INTO repos (root_path, name, identity) VALUES ('/repo1', 'repo1', 'git@github.com:org/repo.git')`)
	if err != nil {
		rawDB.Close()
		require.NoError(t, err, "Failed to insert repo1: %v")
	}

	rawDB.Close()

	// Open with storage.Open which runs migrations
	db, err := Open(dbPath)
	require.NoError(t, err, "Migration failed: %v")

	// Verify we can now insert a second repo with the same identity
	// (this would fail if the unique index wasn't converted to non-unique)
	_, err = db.Exec(`INSERT INTO repos (root_path, name, identity) VALUES ('/repo2', 'repo2', 'git@github.com:org/repo.git')`)
	if err != nil {
		db.Close()
		require.NoError(t, err, "Inserting second repo with same identity should succeed after migration, but got: %v")
	}

	db.Close()
}
