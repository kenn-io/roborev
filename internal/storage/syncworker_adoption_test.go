package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Adopting a sync target must pick the right push-direction reconciliation.
// The first-ever adoption (no recorded target) is the case that matters most:
// it is when a machine with existing local history is first pointed at a
// shared database.
func TestAdoptionActionFor(t *testing.T) {
	const (
		oldTarget = "db-aaaa"
		newTarget = "db-bbbb"
	)
	cases := []struct {
		name         string
		lastTargetID string
		dbID         string
		skipBackfill bool
		syncedBefore bool
		want         adoptionAction
	}{
		{"first adoption with skip_backfill stamps", "", newTarget, true, false, adoptionStamp},
		{"first adoption without skip_backfill does nothing", "", newTarget, false, false, adoptionNone},
		{"upgrade with sync history is not a first adoption", "", newTarget, true, true, adoptionNone},
		{"changed target with skip_backfill stamps", oldTarget, newTarget, true, true, adoptionStamp},
		{"changed target without skip_backfill clears", oldTarget, newTarget, false, true, adoptionClear},
		{"same target does nothing", newTarget, newTarget, false, true, adoptionNone},
		{"same target with skip_backfill does nothing", newTarget, newTarget, true, true, adoptionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, adoptionActionFor(
				tc.lastTargetID, tc.dbID, tc.skipBackfill, tc.syncedBefore,
			))
		})
	}
}
