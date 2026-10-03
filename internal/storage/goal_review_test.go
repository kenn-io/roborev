package storage_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestGoalReviewJob(t *testing.T) {
	t.Parallel()
	job := storage.ReviewJob{JobType: storage.JobTypeGoalReview, GitRef: "goal:synthetic-digest"}
	assert := assert.New(t)
	assert.True(job.IsGoalReviewJob())
	assert.True(job.UsesStoredPrompt())
	assert.True(job.IsReviewJob())
	assert.False(job.IsTaskJob(), "intent critique produces a verdict")
	assert.False(job.IsDirtyJob())
	assert.False(job.IsFixJob())
	assert.False(storage.ReviewJob{JobType: storage.JobTypeReview}.IsGoalReviewJob())
}
