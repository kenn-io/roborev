package searchindex

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/searchdoc"
)

func loadEmbeddingConfig(t *testing.T, endpoint, documentPrefix, queryPrefix, mode string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := fmt.Sprintf(`[search.embeddings]
base_url = %q
model = "embeddinggemma-2-text-r1"
dims = 768
document_prefix = %q
query_prefix = %q
input_type_mode = %q
batch_size = 4
timeout_seconds = 120
trust_private_network = true
`, endpoint, documentPrefix, queryPrefix, mode)
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	cfg, err := config.LoadGlobalFrom(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Search.Embeddings)
	return cfg
}

type embeddingRequest struct {
	method, path string
	body         map[string]json.RawMessage
}

func TestEmbeddingGemmaLiteralRoleTransport(t *testing.T) {
	requests := make(chan embeddingRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- embeddingRequest{r.Method, r.URL.Path, body}
		vec := make([]float32, 768)
		vec[0], vec[1] = 3, 4
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vec}}}))
	}))
	defer server.Close()
	cfg := loadEmbeddingConfig(t, server.URL+"/v1", "title: none | text: ", "task: search result | query: ", "none")
	client, err := NewEmbeddings(*cfg.Search.Embeddings, "", searchdoc.RecipeVersion)
	require.NoError(t, err)
	assert := assert.New(t)
	input := []string{"synthetic text"}
	for _, tc := range []struct {
		query bool
		want  string
	}{
		{false, "title: none | text: synthetic text"},
		{true, "task: search result | query: synthetic text"},
		{false, "title: none | text: synthetic text"},
	} {
		encode := encodeDocuments(client)
		if tc.query {
			encode = encodeQueries(client)
		}
		vectors, err := encode(t.Context(), input)
		require.NoError(t, err)
		require.Len(t, vectors, 1)
		require.Len(t, vectors[0], 768)
		assert.InDelta(0.6, vectors[0][0], 1e-6)
		assert.InDelta(0.8, vectors[0][1], 1e-6)
		request := <-requests
		assert.Equal(http.MethodPost, request.method)
		assert.Equal("/v1/embeddings", request.path)
		assert.JSONEq(`"embeddinggemma-2-text-r1"`, string(request.body["model"]))
		assert.JSONEq(fmt.Sprintf("[%q]", tc.want), string(request.body["input"]))
		assert.NotContains(request.body, "dimensions")
		assert.NotContains(request.body, "input_type")
		assert.Equal([]string{"synthetic text"}, input)
	}
}
