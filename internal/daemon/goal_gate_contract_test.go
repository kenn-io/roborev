package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoalGateEmptyBodyReturnsGoalGateResponse(t *testing.T) {
	server, _, _ := newTestServer(t)
	request := httptest.NewRequest(http.MethodPost, "/api/goal-review", nil)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusBadRequest, response.Code)
	var body GoalGateResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, 1, body.Version)
	assert.Equal(t, "error", body.Status)
	assert.Equal(t, "block", body.Mode)
	assert.True(t, body.Blocked)
	assert.Empty(t, body.Findings)
	assert.Equal(t, "request body is required", body.Error)
}

func TestGoalGateOpenAPIAdvertisesJSONOnly(t *testing.T) {
	api := (&Server{}).registerHumaAPI(http.NewServeMux())
	operation := api.OpenAPI().Paths["/api/goal-review"].Post
	require.NotNil(t, operation.RequestBody)
	assert.False(t, operation.RequestBody.Required)
	require.Len(t, operation.RequestBody.Content, 1)
	require.Contains(t, operation.RequestBody.Content, "application/json")
	requestSchema := api.OpenAPI().Components.Schemas.Map()["GoalGateRequest"]
	require.NotNil(t, requestSchema)
	assert.NotContains(t, requestSchema.Properties, "$schema")
}

func TestGoalGateClientRetainsRejectionDetails(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"version":1,"status":"error","mode":"block","blocked":true,"findings":[],"error":"Synthetic gate rejection"}`))
			}))
			defer server.Close()
			client := &HTTPClient{baseURL: server.URL, httpClient: server.Client()}
			response, err := client.GoalReview(t.Context(), GoalGateRequest{RepoPath: "/workspace/project"})
			require.ErrorContains(t, err, "Synthetic gate rejection")
			require.NotNil(t, response)
			assert.Equal(t, "error", response.Status)
			assert.True(t, response.Blocked)
		})
	}
}
