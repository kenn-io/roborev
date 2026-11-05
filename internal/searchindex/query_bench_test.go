package searchindex

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/searchdoc"
)

func BenchmarkSearchLexicalHistory(b *testing.B) {
	for _, reviews := range []int{1000, 4000, 16000} {
		b.Run(fmt.Sprintf("reviews=%d", reviews), func(b *testing.B) {
			index, err := Open(b.Context(), filepath.Join(b.TempDir(), "search.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, index.Close()) })
			docs := make([]searchdoc.Document, reviews)
			for i := range docs {
				body := fmt.Sprintf("## Review %d\n\n", i+1) + strings.Repeat(
					"Finding: the needle lookup uses the previous request's cached result. "+
						"When the caller switches repositories, the handler can return a review from the previous repository. "+
						"Include the repository key in the cache lookup and cover two consecutive requests for different repositories.\n\n", 8)
				docs[i] = queryTestDocument(int64(i+1), fmt.Sprintf("group-%d", i), body, queryDocOptions{})
			}
			_, err = index.RefreshMirrorPage(b.Context(), docs, nil)
			require.NoError(b, err)
			for _, query := range []struct{ name, text string }{{"common", "needle"}, {"identifier", "main"}, {"no-match", "zzqxv"}} {
				b.Run(query.name, func(b *testing.B) {
					for b.Loop() {
						_, err := index.SearchLexical(b.Context(), query.text, SearchFilters{}, 60)
						require.NoError(b, err)
					}
				})
			}
		})
	}
}
