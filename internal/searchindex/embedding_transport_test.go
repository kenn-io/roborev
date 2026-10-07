package searchindex

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/searchdoc"
)

func TestEmbeddingRolePrefixesPreserveEmptyAndSingleRoleInputs(t *testing.T) {
	for _, tc := range []struct {
		name, documentPrefix, queryPrefix, mode, wantDocument, wantQuery string
	}{
		{"empty", "", "", "none", "plain text", "plain text"},
		{"retrieval", "", "", "retrieval", "plain text", "plain text"},
		{"document only", "title: none | text: ", "", "none", "title: none | text: plain text", "plain text"},
		{"query only", "", "task: search result | query: ", "none", "plain text", "task: search result | query: plain text"},
		{"literal spaces", " ", " ", "none", " plain text", " plain text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan embeddingRequest, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- embeddingRequest{r.Method, r.URL.Path, body}
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"embedding": unitFixture(768)}}}))
			}))
			defer server.Close()
			cfg := loadEmbeddingConfig(t, server.URL+"/v1", tc.documentPrefix, tc.queryPrefix, tc.mode)
			client, err := NewEmbeddings(*cfg.Search.Embeddings, "", searchdoc.RecipeVersion)
			require.NoError(t, err)
			for _, call := range []struct {
				query           bool
				want, inputType string
			}{
				{false, tc.wantDocument, "document"}, {true, tc.wantQuery, "query"},
			} {
				encode := encodeDocuments(client)
				if call.query {
					encode = encodeQueries(client)
				}
				_, err := encode(t.Context(), []string{"plain text"})
				require.NoError(t, err)
				request := <-requests
				assert.JSONEq(t, fmt.Sprintf("[%q]", call.want), string(request.body["input"]))
				assert.NotContains(t, request.body, "dimensions")
				if tc.mode == "retrieval" {
					assert.JSONEq(t, fmt.Sprintf("%q", call.inputType), string(request.body["input_type"]))
				} else {
					assert.NotContains(t, request.body, "input_type")
				}
			}
		})
	}
}

func unitFixture(width int) []float32 {
	vector := make([]float32, width)
	vector[0] = 1
	return vector
}

func TestEmbeddingPrefixesDoNotMutateInputsOrReorderBatches(t *testing.T) {
	requests := make(chan []string, 6)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- body.Input
		var data []any
		for i := range slices.Backward(body.Input) {
			vec := make([]float32, 768)
			// A distinct axis for each text exposes swaps across calls/batches.
			axis := strings.Index("abcde", body.Input[i][len(body.Input[i])-1:])
			if !assert.GreaterOrEqual(t, axis, 0) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			vec[axis] = 1
			data = append(data, map[string]any{"index": i, "embedding": vec})
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	defer server.Close()
	cfg := loadEmbeddingConfig(t, server.URL, "title: none | text: ", "task: search result | query: ", "none")
	cfg.Search.Embeddings.BatchSize = 2
	client, err := NewEmbeddings(*cfg.Search.Embeddings, "", searchdoc.RecipeVersion)
	require.NoError(t, err)
	assert := assert.New(t)
	input := []string{"a", "title: none | text: b", "c", "d", "e"}
	for range 2 {
		vectors, err := encodeDocuments(client)(t.Context(), input)
		require.NoError(t, err)
		require.Len(t, vectors, 5)
		for i, vec := range vectors {
			want := make([]float32, 768)
			want[i] = 1
			assert.Equal(want, vec)
		}
		assert.Equal([]string{"title: none | text: a", "title: none | text: title: none | text: b"}, <-requests)
		assert.Equal([]string{"title: none | text: c", "title: none | text: d"}, <-requests)
		assert.Equal([]string{"title: none | text: e"}, <-requests)
		assert.Equal([]string{"a", "title: none | text: b", "c", "d", "e"}, input)
	}
	_, err = encodeQueries(client)(t.Context(), []string{" "})
	require.Error(t, err, "a prefix cannot make blank raw input usable")
	assert.Empty(requests)
}

func TestEmbeddingGemmaRejectsUnusableVectors(t *testing.T) {
	jsonVector := func(width int, first string) string {
		return "[" + first + strings.Repeat(",0", width-1) + "]"
	}
	base64Vector := func(value float32) string {
		bytes := make([]byte, 768*4)
		binary.LittleEndian.PutUint32(bytes, math.Float32bits(value))
		return fmt.Sprintf("%q", base64.StdEncoding.EncodeToString(bytes))
	}
	for _, tc := range []struct{ name, vector string }{
		{"short width", jsonVector(767, "1")},
		{"long width", jsonVector(769, "1")},
		{"zero norm", jsonVector(768, "0")},
		{"null component", "[null,1" + strings.Repeat(",0", 766) + "]"},
		{"float32 overflow", jsonVector(768, "1e100")},
		{"base64 NaN", base64Vector(float32(math.NaN()))},
		{"base64 Inf", base64Vector(float32(math.Inf(1)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := fmt.Fprintf(w, `{"data":[{"index":0,"embedding":%s}]}`, tc.vector)
				assert.NoError(t, err)
			}))
			defer server.Close()
			cfg := loadEmbeddingConfig(t, server.URL, "title: none | text: ", "task: search result | query: ", "none")
			client, err := NewEmbeddings(*cfg.Search.Embeddings, "", searchdoc.RecipeVersion)
			require.NoError(t, err)
			vectors, err := encodeQueries(client)(t.Context(), []string{"synthetic query"})
			require.Error(t, err)
			for _, vec := range vectors {
				assert.Empty(t, vec, "invalid responses cannot yield usable vectors")
			}
		})
	}
}
