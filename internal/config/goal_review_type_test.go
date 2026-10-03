package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGoalReviewTypeNameReserved(t *testing.T) {
	err := validateCustomReviewTypes(map[string]ReviewTypeSpec{"goal": {Template: "review.md"}})
	require.ErrorContains(t, err, "reserved", "the goal command must not shadow a configured custom code-review type")
}
