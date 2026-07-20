//go:build postgres

package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresBunHandleSharesPgxPool(t *testing.T) {
	ctx := t.Context()
	pool := openTestPgPool(t)
	key := "bun-shared-pool-" + GenerateUUID()

	_, err := pool.Pool().Exec(ctx, `
		INSERT INTO sync_metadata (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, key, "visible-through-bun")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := pool.Pool().Exec(context.Background(),
			`DELETE FROM sync_metadata WHERE key = $1`, key)
		assert.NoError(t, cleanupErr)
	})

	var value string
	err = pool.bun.NewSelect().
		Table("sync_metadata").
		Column("value").
		Where("key = ?", key).
		Scan(ctx, &value)
	require.NoError(t, err)
	assert.Equal(t, "visible-through-bun", value)

	require.NoError(t, pool.bun.Close())
	pool.bun = nil
	require.NoError(t, pool.Pool().Ping(ctx))
}
