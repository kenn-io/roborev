package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCodeowners(t *testing.T) {
	for _, tt := range []struct {
		name      string
		content   string
		wantUsers []string
		wantTeams []string
	}{
		{name: "empty", content: ""},
		{
			name: "users teams and emails",
			content: "# Default owners\n" +
				"*       @alice @acme/core\n" +
				"/docs/  docs@example.com @bob\n",
			wantUsers: []string{"alice", "bob"},
			wantTeams: []string{"acme/core"},
		},
		{
			name:      "inline comment ends the owner list",
			content:   "*.go @alice # @mallory is not an owner\n",
			wantUsers: []string{"alice"},
		},
		{
			name:    "pattern without owners and commented-out line",
			content: "/vendor/\n# * @mallory\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			users, teams := ParseCodeowners(tt.content)
			assert.Equal(t, tt.wantUsers, users)
			assert.Equal(t, tt.wantTeams, teams)
		})
	}
}

func TestCodeownerLogins(t *testing.T) {
	// The team appears twice, in different case, and its members span two pages.
	codeowners := "* @Alice @acme/core\n/docs/ @ACME/core\n"
	for _, tt := range []struct {
		name        string
		files       map[string]string // repo path -> CODEOWNERS content at the ref
		page2Status int               // status of the second team-members page
		want        map[string]struct{}
		wantPages   map[string]int // team-members requests per page number
		wantErr     string
	}{
		{
			name:        "first location wins and teams expand",
			files:       map[string]string{"CODEOWNERS": codeowners, "docs/CODEOWNERS": "* @mallory\n"},
			page2Status: http.StatusOK,
			want:        map[string]struct{}{"alice": {}, "carol": {}, "dave": {}},
			wantPages:   map[string]int{"": 1, "2": 1}, // the repeated team is expanded once
		},
		{
			name:      "no CODEOWNERS file",
			files:     map[string]string{},
			want:      map[string]struct{}{},
			wantPages: map[string]int{},
		},
		{
			name:        "failed later team page is an error",
			files:       map[string]string{".github/CODEOWNERS": codeowners},
			page2Status: http.StatusForbidden,
			wantErr:     "@acme/core",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			teamPages := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v3/orgs/acme/teams/core/members" {
					page := r.URL.Query().Get("page")
					teamPages[page]++
					if page == "" {
						w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=2>; rel=\"next\"", r.Host, r.URL.Path))
						fmt.Fprint(w, `[{"login":"carol"}]`)
						return
					}
					w.WriteHeader(tt.page2Status)
					if tt.page2Status == http.StatusOK {
						fmt.Fprint(w, `[{"login":"Dave"}]`)
					} else {
						fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
					}
					return
				}
				for path, content := range tt.files {
					if r.URL.Path == "/api/v3/repos/acme/api/contents/"+path {
						assert.Equal("main", r.URL.Query().Get("ref"))
						assert.NoError(json.NewEncoder(w).Encode(map[string]string{
							"type": "file", "encoding": "base64",
							"content": base64.StdEncoding.EncodeToString([]byte(content)),
						}))
						return
					}
				}
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
			}))
			defer server.Close()

			client, err := NewClient("", WithBaseURL(server.URL+"/"))
			require.NoError(t, err)
			got, err := client.CodeownerLogins(t.Context(), "acme/api", "main")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Nil(got)
				return
			}
			require.NoError(t, err)
			assert.Equal(tt.want, got)
			assert.Equal(tt.wantPages, teamPages)
		})
	}
}
