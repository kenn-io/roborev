package daemon

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestExperimentReportSuccessWireSchema(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	_, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "topic", Agent: "test", Experiment: &storage.ExperimentAssignmentInput{
		ExperimentID: "success-v1", DefinitionHash: "definition", DefinitionJSON: `{"ratio":0.5,"success":{"metric":"cost_per_review","direction":"lower","min_effect":"20%"}}`, Arm: "default", SubjectHash: "subject-a", EffectiveConfigHash: "frozen", EffectiveConfigJSON: `{"Agent":"test"}`,
	}})
	require.NoError(t, err)
	rr := serveHuma(t, srv, http.MethodGet, "/api/experiments/success-v1/report", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var wire map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &wire))
	success, ok := wire["success"].(map[string]any)
	require.True(t, ok, "a configured bar must emit an object")
	assert.Equal(t, "not enough data", success["state"])
	guards, ok := success["guards"].([]any)
	require.True(t, ok, "an empty guard list must emit an array")
	assert.Empty(t, guards)
	api := (&Server{}).registerHumaAPI(http.NewServeMux())
	schema := api.OpenAPI().Components.Schemas.Map()["ExperimentReport"]
	require.NotNil(t, schema)
	assert.NotContains(t, schema.Required, "success")
	result := &huma.ValidateResult{}
	huma.Validate(api.OpenAPI().Components.Schemas, schema, &huma.PathBuffer{}, huma.ModeReadFromServer, wire, result)
	assert.Empty(t, result.Errors)
}
