package daemon

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/storage"
)

func TestKataCommentReferenceResolvesOnlyItsMappedFindingThread(t *testing.T) {
	for _, mode := range []string{"root UID", "reply handle", "uppercase handle", "qualified handle", "full issue UID", "long issue suffix", "uppercase issue suffix", "long issue suffix excludes unavailable issue", "duplicate publication mapping", "foreign root", "foreign project", "missing comment", "ambiguous handle", "qualified handle excludes unavailable project", "qualified handle excludes unavailable issue", "unqualified unavailable candidate", "renamed alias", "renamed alias with replaced project UID"} {
		t.Run(mode, func(t *testing.T) {
			s, db, _ := newTestServer(t)
			jobID, findings := rebuttalFindingParent(t, s, db)
			provider := katafake.New(t)
			provider.Token = "test-credential"
			issue := provider.AddIssue(7, "ab12", "Synthetic review findings", "")
			issue.Out.UID = "01" + strings.Repeat("0", 20) + "AB12"
			cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: provider.URL(), APIKey: "test-credential", Teammate: "roborev", DefaultProject: "reviews"}
			s.configWatcher.Config().Integrations.Kata = cfg
			client, err := kata.NewAPIClient("", cfg, kata.WithoutWorkspace())
			require.NoError(t, err)
			t.Cleanup(client.CloseIdleConnections)
			root, err := client.Comment(t.Context(), kata.CommentReq{Project: "reviews", Issue: issue.Out.UID, Body: "The original synthetic finding.", IdempotencyKey: "root"})
			require.NoError(t, err)
			reply, err := client.Comment(t.Context(), kata.CommentReq{Project: "reviews", Issue: issue.Out.UID, Body: "Additional thread context.", ReplyTo: root.UID, Kind: kata.CommentReply, Teammate: "reader", IdempotencyKey: "reply"})
			require.NoError(t, err)
			require.NotNil(t, issue.Comments[1].Handle)
			handle := *issue.Comments[1].Handle
			foreign, err := client.Comment(t.Context(), kata.CommentReq{Project: "reviews", Issue: issue.Out.UID, Body: "A different unmapped thread.", IdempotencyKey: "foreign"})
			require.NoError(t, err)
			snapshot, err := db.FindingSnapshot(t.Context(), findings[0].ID)
			require.NoError(t, err)
			publication := katapublish.Request{OperationID: "publish:target-fixture", Endpoint: client.Endpoint(), Project: "reviews", ProjectUID: provider.Projects[0].UID, Policy: "comments", ReviewUUID: snapshot.ReviewUUID.String(), Teammate: "roborev", CredentialFingerprint: kataCredentialFingerprint(cfg), Entries: []katapublish.Entry{{Selector: snapshot.Selector, ReviewUUID: snapshot.ReviewUUID.String(), DocumentHash: snapshot.DocumentHash, Index: snapshot.Index, Body: "The original synthetic finding."}}} //nolint:forbidigo // Immutable review UUID crosses the provider publication boundary.
			if strings.Contains(mode, "renamed alias") {
				publication.WorkspaceAlias = &kata.ProjectAlias{Identity: "code.example/team/repo", Kind: "git"}
			}
			tx, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			require.NoError(t, storage.InsertKataHandoffTx(t.Context(), tx, jobID, publication))
			require.NoError(t, tx.Commit())
			state := katapublish.State{Done: true, IssueUID: issue.Out.UID, IssueRef: "reviews#ab12", ProjectUID: provider.Projects[0].UID, Receipts: map[string]katapublish.Receipt{snapshot.Selector: {UID: root.UID, Attribution: root.Attribution}}}
			raw, err := json.Marshal(state)
			require.NoError(t, err)
			_, err = db.Exec(`UPDATE kata_handoffs SET state_json=?,status='done' WHERE operation_id=?`, string(raw), publication.OperationID)
			require.NoError(t, err)
			duplicateRootUID := ""
			if mode == "duplicate publication mapping" {
				duplicateRoot, commentErr := client.Comment(t.Context(), kata.CommentReq{Project: "reviews", Issue: issue.Out.UID, Body: "A second publication of the same finding.", IdempotencyKey: "duplicate-root"})
				require.NoError(t, commentErr)
				duplicateRootUID = duplicateRoot.UID
				duplicate := publication
				duplicate.OperationID = "publish:duplicate-target-fixture"
				tx, err = db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				require.NoError(t, storage.InsertKataHandoffTx(t.Context(), tx, jobID, duplicate))
				require.NoError(t, tx.Commit())
				duplicateState := katapublish.State{Done: true, IssueUID: issue.Out.UID, IssueRef: "reviews#ab12", ProjectUID: provider.Projects[0].UID, Receipts: map[string]katapublish.Receipt{snapshot.Selector: {UID: duplicateRoot.UID, Attribution: duplicateRoot.Attribution}}}
				raw, err = json.Marshal(duplicateState)
				require.NoError(t, err)
				_, err = db.Exec(`UPDATE kata_handoffs SET state_json=?,status='done' WHERE operation_id=?`, string(raw), duplicate.OperationID)
				require.NoError(t, err)
			}
			if strings.Contains(mode, "unavailable") {
				otherJob, otherFindings := rebuttalFindingParent(t, s, db)
				otherSnapshot, err := db.FindingSnapshot(t.Context(), otherFindings[0].ID)
				require.NoError(t, err)
				unavailable := publication
				unavailable.OperationID = "aaa:unavailable-publication"
				unavailable.ReviewUUID = otherSnapshot.ReviewUUID.String() //nolint:forbidigo // Synthetic immutable review UUID boundary.
				unavailable.Entries = []katapublish.Entry{{Selector: otherSnapshot.Selector, ReviewUUID: unavailable.ReviewUUID, DocumentHash: otherSnapshot.DocumentHash, Index: otherSnapshot.Index, Body: "A different finding."}}
				if strings.Contains(mode, "project") {
					unavailable.Project, unavailable.ProjectUID = "other", provider.Projects[1].UID
				}
				tx, err := db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				require.NoError(t, storage.InsertKataHandoffTx(t.Context(), tx, otherJob, unavailable))
				require.NoError(t, tx.Commit())
				unavailableIssueUID := "01M00000000000000000000999"
				if mode == "qualified handle excludes unavailable project" {
					unavailableIssueUID = issue.Out.UID
					provider.Projects[1].Active = false
				}
				otherState := katapublish.State{Done: true, IssueUID: unavailableIssueUID, IssueRef: unavailable.Project + "#zzzz", ProjectUID: unavailable.ProjectUID, Receipts: map[string]katapublish.Receipt{otherSnapshot.Selector: {UID: "01M00000000000000000000888"}}}
				raw, err := json.Marshal(otherState)
				require.NoError(t, err)
				_, err = db.Exec(`UPDATE kata_handoffs SET state_json=?,status='done' WHERE operation_id=?`, string(raw), unavailable.OperationID)
				require.NoError(t, err)
			}
			if strings.Contains(mode, "renamed alias") {
				provider.Projects[0].Name = "renamed-reviews"
				if mode == "renamed alias with replaced project UID" {
					provider.Projects[0].UID = "replacement-project"
				}
				provider.Override = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path != "/api/v1/projects/resolve" {
						return false
					}
					const identity = "code.example/team/repo"
					var request map[string]any
					assert.NoError(t, json.UnmarshalRead(r.Body, &request))
					assert.Equal(t, map[string]any{"alias": map[string]any{"identity": identity, "kind": "git"}}, request)
					project := provider.Projects[0]
					provider.Reply(w, gen.ProjectResolveBody{Project: project, Alias: gen.ProjectAlias{
						AliasIdentity: identity, AliasKind: "git", ID: 1, ProjectID: project.ID, CreatedAt: time.Unix(0, 0).UTC(),
					}})
					return true
				}
			}
			target, want, wantRoot := root.UID, root.UID, root.UID
			switch mode {
			case "reply handle":
				target, want = handle, reply.UID
			case "uppercase handle":
				target, want = strings.ToUpper(handle), reply.UID
			case "qualified handle", "qualified handle excludes unavailable project", "qualified handle excludes unavailable issue":
				target, want = "reviews#ab12:"+strings.TrimPrefix(handle, "c:"), reply.UID
			case "full issue UID":
				target, want = "reviews#"+issue.Out.UID+":"+strings.TrimPrefix(handle, "c:"), reply.UID
			case "long issue suffix", "long issue suffix excludes unavailable issue":
				target, want = "reviews#"+strings.ToLower(issue.Out.UID[len(issue.Out.UID)-8:])+":"+strings.TrimPrefix(handle, "c:"), reply.UID
			case "uppercase issue suffix":
				target, want = "reviews#"+issue.Out.UID[len(issue.Out.UID)-8:]+":"+strings.TrimPrefix(handle, "c:"), reply.UID
			case "renamed alias", "renamed alias with replaced project UID":
				target, want = "renamed-reviews#ab12:"+strings.TrimPrefix(handle, "c:"), reply.UID
			case "duplicate publication mapping":
				target, want, wantRoot = duplicateRootUID, duplicateRootUID, duplicateRootUID
			case "unqualified unavailable candidate":
				target = handle
			case "foreign root":
				target = foreign.UID
			case "foreign project":
				target = "other#ab12:" + strings.TrimPrefix(handle, "c:")
			case "missing comment":
				target = "c:abcdef"
			case "ambiguous handle":
				issue.Comments[2].UID = "B" + reply.UID[1:]
				issue.Comments[2].Handle = issue.Comments[1].Handle
				target = handle
			}
			// A later default-project change must not redirect thread lookup.
			s.configWatcher.Config().Integrations.Kata.DefaultProject = "other"
			intent := kataresponse.Request{OperationID: "Fix-42:Attempt-A", FindingID: target, Kind: "reply", Body: "Follow-up evidence for the original thread."}
			raw, err = json.Marshal(intent)
			require.NoError(t, err)
			rr := serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
			if mode == "foreign root" || mode == "foreign project" || mode == "missing comment" || mode == "ambiguous handle" || mode == "unqualified unavailable candidate" || mode == "renamed alias with replaced project UID" {
				require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
				var count int
				require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM kata_responses`).Scan(&count))
				assert.Zero(t, count)
				return
			}
			require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
			handoffs, err := db.KataHandoffs(t.Context(), jobID)
			require.NoError(t, err)
			var captured []katapublish.Request
			for _, h := range handoffs {
				if len(h.Request.Responses) != 0 {
					captured = append(captured, h.Request)
				}
			}
			require.Len(t, captured, 1)
			assert.Equal(t, snapshot.Selector, captured[0].Entries[0].Selector)
			assert.Equal(t, wantRoot, captured[0].Responses[0].Root)
			assert.Equal(t, want, captured[0].Responses[0].Target)
			saved, err := db.KataResponseIntent(t.Context(), intent.OperationID)
			require.NoError(t, err)
			assert.Equal(t, intent, saved)
		})
	}
}

func FuzzSkipQualifiedResponseProject(f *testing.F) {
	f.Add("", "", false)
	f.Add("reviews", "reviews", false)
	f.Add("reviews", "other", false)
	f.Add("renamed-reviews", "old-reviews", true)
	f.Fuzz(func(t *testing.T, project, frozenProject string, hasAlias bool) {
		request := katapublish.Request{Project: frozenProject}
		if hasAlias {
			request.WorkspaceAlias = &kata.ProjectAlias{Identity: "example/repo", Kind: "git"}
		}
		want := project != "" && !hasAlias && project != frozenProject
		assert.Equal(t, want, skipQualifiedResponseProject(project, request))
	})
}
