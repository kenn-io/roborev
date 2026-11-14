package kata

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocbankHealth(t *testing.T) {
	for _, tc := range []struct {
		name, response, credential string
		runErr                     error
		wantErr                    bool
	}{
		{name: "key resolves", response: `{"docbank":{"credential":"ok"}}`, credential: "ok"},
		{name: "missing credential", response: `{"docbank":{"credential":"missing"}}`, credential: "missing"},
		{name: "old or restricted", response: `{}`},
		{name: "unknown credential", response: `{"docbank":{"credential":"future"}}`, credential: "future"},
		{name: "malformed", response: `{`, wantErr: true},
		{name: "unavailable", runErr: errors.New("unavailable"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, args := stubClient(tc.response, tc.runErr)
			got, err := c.DocbankHealth(t.Context())
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.credential, got.Credential)
			assert.Equal(t, []string{"health", "--json"}, *args)
		})
	}
}
