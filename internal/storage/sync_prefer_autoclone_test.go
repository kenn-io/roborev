package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/config"
)

func TestPreferAutoCloneNormalizesPathSeparators(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "roborev-data")
	t.Setenv("ROBOREV_DATA_DIR", dataDir)

	t.Run("matches forward slash stored clone path", func(t *testing.T) {
		clone := Repo{
			ID:        1,
			RootPath:  filepath.ToSlash(filepath.Join(dataDir, "clones", "repo")),
			Identity:  "remote",
			CreatedAt: time.Unix(1, 0),
		}
		recent := Repo{
			ID:        2,
			RootPath:  filepath.ToSlash(filepath.Join(dataDir, "checkout")),
			Identity:  "remote",
			CreatedAt: time.Unix(2, 0),
		}

		got := PreferAutoClone([]Repo{clone, recent})
		assert.Equal(t, dataDir, config.DataDir())
		assert.Equal(t, int64(1), got.ID)
	})

	t.Run("requires the clone boundary slash", func(t *testing.T) {
		clonePrefixLookalike := Repo{
			ID:        1,
			RootPath:  filepath.ToSlash(filepath.Join(dataDir, "clones-other", "repo")),
			Identity:  "remote",
			CreatedAt: time.Unix(1, 0),
		}
		recent := Repo{
			ID:        2,
			RootPath:  filepath.ToSlash(filepath.Join(dataDir, "checkout")),
			Identity:  "remote",
			CreatedAt: time.Unix(2, 0),
		}

		got := PreferAutoClone([]Repo{clonePrefixLookalike, recent})
		assert.Equal(t, int64(2), got.ID)
	})
}
