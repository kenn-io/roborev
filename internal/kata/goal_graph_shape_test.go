package kata

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGoalGraphRequiresAnIssuesArray(t *testing.T) {
	for _, payload := range []string{`{}`, `{"issues":null}`} {
		t.Run(payload, func(t *testing.T) {
			client, _ := stubClient(payload, nil)
			_, err := client.List(context.Background(), ListOpts{Unlimited: true})
			require.Error(t, err, "missing graph data must not look like an empty project")
		})
	}
	client, _ := stubClient(`{"issues":[]}`, nil)
	issues, err := client.List(context.Background(), ListOpts{Unlimited: true})
	require.NoError(t, err)
	require.Empty(t, issues)
}
