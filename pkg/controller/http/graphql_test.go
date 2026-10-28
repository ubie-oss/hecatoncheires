package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	gollemmock "github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
	"github.com/robfig/cron/v3"
	"github.com/vektah/gqlparser/v2/gqlerror"

	gqlctrl "github.com/m-mizutani/hecatoncheires/pkg/controller/graphql"
	httpctrl "github.com/m-mizutani/hecatoncheires/pkg/controller/http"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	jobagent "github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/job"
	jobruntime "github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
)

const testWorkspaceID = "test-ws"

// setupGraphQLServer creates a test GraphQL server with HTTP handler
func setupGraphQLServer(repo interfaces.Repository) (http.Handler, error) {
	uc := usecase.New(repo, nil)
	resolver := gqlctrl.NewResolver(repo, uc)
	srv := handler.NewDefaultServer(
		gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: resolver}),
	)

	// Configure error presenter with stack traces (same as serve.go)
	srv.SetErrorPresenter(func(ctx context.Context, err error) *gqlerror.Error {
		gqlErr := graphql.DefaultErrorPresenter(ctx, err)
		wrappedErr := goerr.Wrap(err, "GraphQL error")
		logging.Default().Error("GraphQL error occurred", "error", wrappedErr)
		return gqlErr
	})

	// Configure panic handler (same as serve.go)
	srv.SetRecoverFunc(func(ctx context.Context, panicValue interface{}) error {
		var panicErr error
		switch e := panicValue.(type) {
		case error:
			panicErr = e
		case string:
			panicErr = goerr.New(e)
		default:
			panicErr = goerr.New("panic occurred", goerr.V("panic", panicValue))
		}

		wrappedErr := goerr.Wrap(panicErr, "GraphQL panic")
		logging.Default().Error("GraphQL panic occurred", "error", wrappedErr)
		return wrappedErr
	})

	// Wrap with dataloader middleware (same as serve.go)
	gqlHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loaders := gqlctrl.NewDataLoaders(repo, nil, nil)
		ctx := gqlctrl.WithDataLoaders(r.Context(), loaders)
		srv.ServeHTTP(w, r.WithContext(ctx))
	})

	return httpctrl.New(gqlHandler)
}

// GraphQL request/response structures
type graphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data,omitempty"`
	Errors []graphQLError  `json:"errors,omitempty"`
}

type graphQLError struct {
	Message string        `json:"message"`
	Path    []interface{} `json:"path,omitempty"`
	// Extensions carries the classification the ErrorPresenter attaches
	// (extensions.code and its code-specific detail keys). Only populated
	// by handlers that wire gqlctrl.ErrorExtensions, as serve.go does.
	Extensions map[string]interface{} `json:"extensions,omitempty"`
}

// executeGraphQLRequest sends a GraphQL request through the HTTP handler
func executeGraphQLRequest(t *testing.T, handler http.Handler, query string, variables map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()

	reqBody := graphQLRequest{
		Query:     query,
		Variables: variables,
	}

	body, err := json.Marshal(reqBody)
	gt.NoError(t, err).Required()

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

// executeGraphQLRequestWithAuth sends a GraphQL request with an auth token injected into the context
func executeGraphQLRequestWithAuth(t *testing.T, handler http.Handler, query string, variables map[string]interface{}, userID string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody := graphQLRequest{
		Query:     query,
		Variables: variables,
	}

	body, err := json.Marshal(reqBody)
	gt.NoError(t, err).Required()

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	// Inject auth token into request context
	ctx := auth.ContextWithToken(req.Context(), &auth.Token{Sub: userID})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

// parseGraphQLResponse parses the GraphQL response
func parseGraphQLResponse(t *testing.T, rec *httptest.ResponseRecorder) *graphQLResponse {
	t.Helper()

	var resp graphQLResponse
	gt.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp)).Required()

	return &resp
}

// TestGraphQLHandler_WorkspaceScopedIdentity pins the wire contract that makes
// (workspaceId, id) — not id alone — the identity of a Case and an Action.
// Both ids come from a per-workspace counter, so the first Case of every
// workspace is id 1. Without workspaceId on the wire a client that caches by
// id merges them into one object; that is exactly what the Home dashboard did,
// rendering one workspace's title and assignees under another's badge.
func TestGraphQLHandler_WorkspaceScopedIdentity(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()
	const wsA = "ws-alpha"
	const wsB = "ws-beta"

	seed := func(workspaceID, caseTitle, actionTitle string) (int64, int64) {
		c, err := repo.Case().Create(ctx, workspaceID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      caseTitle,
		})
		gt.NoError(t, err).Required()
		a, err := repo.Action().Create(ctx, workspaceID, &model.Action{
			CaseID: c.ID,
			Title:  actionTitle,
			Status: "TODO",
		})
		gt.NoError(t, err).Required()
		return c.ID, a.ID
	}

	caseA, actionA := seed(wsA, "Alpha case", "Alpha action")
	caseB, actionB := seed(wsB, "Beta case", "Beta action")

	// The premise of the whole fix: the two workspaces really do reuse ids.
	gt.Value(t, caseA).Equal(caseB)
	gt.Value(t, actionA).Equal(actionB)

	query := `
		query($workspaceId: String!, $caseId: Int!, $actionId: Int!) {
			case(workspaceId: $workspaceId, id: $caseId) {
				id
				workspaceId
				title
			}
			action(workspaceId: $workspaceId, id: $actionId) {
				id
				workspaceId
				title
			}
		}
	`

	assertScoped := func(t *testing.T, workspaceID, caseTitle, actionTitle string, caseID, actionID int64) {
		t.Helper()
		rec := executeGraphQLRequest(t, handler, query, map[string]interface{}{
			"workspaceId": workspaceID,
			"caseId":      int(caseID),
			"actionId":    int(actionID),
		})
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			Case struct {
				ID          int    `json:"id"`
				WorkspaceID string `json:"workspaceId"`
				Title       string `json:"title"`
			} `json:"case"`
			Action struct {
				ID          int    `json:"id"`
				WorkspaceID string `json:"workspaceId"`
				Title       string `json:"title"`
			} `json:"action"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Number(t, result.Case.ID).Equal(int(caseID))
		gt.String(t, result.Case.WorkspaceID).Equal(workspaceID)
		gt.String(t, result.Case.Title).Equal(caseTitle)

		gt.Number(t, result.Action.ID).Equal(int(actionID))
		gt.String(t, result.Action.WorkspaceID).Equal(workspaceID)
		gt.String(t, result.Action.Title).Equal(actionTitle)
	}

	t.Run("each workspace reports its own id alongside the shared numeric id", func(t *testing.T) {
		assertScoped(t, wsA, "Alpha case", "Alpha action", caseA, actionA)
		assertScoped(t, wsB, "Beta case", "Beta action", caseB, actionB)
	})
}

func TestGraphQLHandler_CasesQuery(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	t.Run("empty cases list", func(t *testing.T) {
		query := `
			query($workspaceId: String!) {
				cases(workspaceId: $workspaceId) {
					id
					title
					description
					assigneeIDs
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()
	})

	t.Run("cases list with data", func(t *testing.T) {
		ctx := context.Background()

		// Create test cases
		case1 := &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Test Case 1",
			Description: "Test case description 1",
			AssigneeIDs: []string{"U001", "U002"},
		}

		case2 := &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Test Case 2",
			Description: "Test case description 2",
			AssigneeIDs: []string{"U003"},
		}

		createdCase1, err := repo.Case().Create(ctx, testWorkspaceID, case1)
		gt.NoError(t, err).Required()

		createdCase2, err := repo.Case().Create(ctx, testWorkspaceID, case2)
		gt.NoError(t, err).Required()

		query := `
			query($workspaceId: String!) {
				cases(workspaceId: $workspaceId) {
					id
					title
					description
					assigneeIDs
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		// Parse cases from response
		var result struct {
			Cases []struct {
				ID          int      `json:"id"`
				Title       string   `json:"title"`
				Description string   `json:"description"`
				AssigneeIDs []string `json:"assigneeIDs"`
			} `json:"cases"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Array(t, result.Cases).Length(2)

		// Verify cases are returned (order may vary based on implementation)
		foundCase1 := false
		foundCase2 := false
		for _, c := range result.Cases {
			if int64(c.ID) == createdCase1.ID && c.Title == "Test Case 1" {
				foundCase1 = true
				gt.Value(t, c.Description).Equal("Test case description 1")
				gt.Array(t, c.AssigneeIDs).Length(2)
			}
			if int64(c.ID) == createdCase2.ID && c.Title == "Test Case 2" {
				foundCase2 = true
				gt.Value(t, c.Description).Equal("Test case description 2")
				gt.Array(t, c.AssigneeIDs).Length(1)
			}
		}

		gt.Bool(t, foundCase1).True()
		gt.Bool(t, foundCase2).True()
	})
}

// archiveCaseRow is the Case shape the archive tests select.
type archiveCaseRow struct {
	ID          int     `json:"id"`
	WorkspaceID string  `json:"workspaceId"`
	Status      string  `json:"status"`
	ArchivedAt  *string `json:"archivedAt"`
}

// decodeArchiveCases unmarshals a `cases` query payload.
func decodeArchiveCases(t *testing.T, resp *graphQLResponse) []archiveCaseRow {
	t.Helper()
	var result struct {
		Cases []archiveCaseRow `json:"cases"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
	return result.Cases
}

// archiveTestCaseIDs pulls the ids out of a `cases` query payload.
func archiveTestCaseIDs(t *testing.T, resp *graphQLResponse) map[int]bool {
	t.Helper()
	rows := decodeArchiveCases(t, resp)
	ids := make(map[int]bool, len(rows))
	for _, row := range rows {
		ids[row.ID] = true
	}
	return ids
}

func TestGraphQLHandler_CaseArchive(t *testing.T) {
	const actor = "U-ARCHIVER"

	casesQuery := `
		query($workspaceId: String!, $filter: CaseArchiveFilter) {
			cases(workspaceId: $workspaceId, filter: $filter) {
				id
				workspaceId
				archivedAt
			}
		}
	`
	archiveMutation := `
		mutation($workspaceId: String!, $id: Int!) {
			archiveCase(workspaceId: $workspaceId, id: $id) {
				id
				workspaceId
				status
				archivedAt
			}
		}
	`
	unarchiveMutation := `
		mutation($workspaceId: String!, $id: Int!) {
			unarchiveCase(workspaceId: $workspaceId, id: $id) {
				id
				workspaceId
				archivedAt
			}
		}
	`

	seed := func(t *testing.T, repo interfaces.Repository, title string, status types.CaseStatus) *model.Case {
		t.Helper()
		created, err := repo.Case().Create(context.Background(), testWorkspaceID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      title,
			Status:     status,
		})
		gt.NoError(t, err).Required()
		return created
	}

	t.Run("archiveCase sets archivedAt and keeps the case closed", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		closed := seed(t, repo, "Closed case", types.CaseStatusClosed)

		rec := executeGraphQLRequestWithAuth(t, handler, archiveMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          closed.ID,
		}, actor)
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0).Required()

		var result struct {
			ArchiveCase archiveCaseRow `json:"archiveCase"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Value(t, result.ArchiveCase.ID).Equal(int(closed.ID))
		gt.Value(t, result.ArchiveCase.Status).Equal("CLOSED")
		gt.Value(t, result.ArchiveCase.ArchivedAt).NotNil()

		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, closed.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).True()
	})

	t.Run("archivedAt is null for an active case", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		seed(t, repo, "Open case", types.CaseStatusOpen)

		rec := executeGraphQLRequestWithAuth(t, handler, casesQuery, map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}, actor)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0).Required()

		rows := decodeArchiveCases(t, resp)
		gt.Array(t, rows).Length(1).Required()
		gt.Value(t, rows[0].ArchivedAt).Nil()
	})

	t.Run("the cases filter selects the archive slice", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		openCase := seed(t, repo, "Open", types.CaseStatusOpen)
		activeClosed := seed(t, repo, "Closed active", types.CaseStatusClosed)
		archived := seed(t, repo, "Closed archived", types.CaseStatusClosed)

		rec := executeGraphQLRequestWithAuth(t, handler, archiveMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          archived.ID,
		}, actor)
		gt.Array(t, parseGraphQLResponse(t, rec).Errors).Length(0).Required()

		// No filter argument: the schema default ACTIVE applies, so an existing
		// client keeps seeing exactly what it saw before archiving existed.
		rec = executeGraphQLRequestWithAuth(t, handler, casesQuery, map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}, actor)
		ids := archiveTestCaseIDs(t, parseGraphQLResponse(t, rec))
		gt.Bool(t, ids[int(openCase.ID)]).True()
		gt.Bool(t, ids[int(activeClosed.ID)]).True()
		gt.Bool(t, ids[int(archived.ID)]).False()

		rec = executeGraphQLRequestWithAuth(t, handler, casesQuery, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"filter":      "ARCHIVED",
		}, actor)
		ids = archiveTestCaseIDs(t, parseGraphQLResponse(t, rec))
		gt.Number(t, len(ids)).Equal(1)
		gt.Bool(t, ids[int(archived.ID)]).True()

		rec = executeGraphQLRequestWithAuth(t, handler, casesQuery, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"filter":      "ALL",
		}, actor)
		ids = archiveTestCaseIDs(t, parseGraphQLResponse(t, rec))
		gt.Number(t, len(ids)).Equal(3)
		gt.Bool(t, ids[int(archived.ID)]).True()
	})

	t.Run("unarchiveCase clears archivedAt", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		closed := seed(t, repo, "Closed case", types.CaseStatusClosed)

		rec := executeGraphQLRequestWithAuth(t, handler, archiveMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          closed.ID,
		}, actor)
		gt.Array(t, parseGraphQLResponse(t, rec).Errors).Length(0).Required()

		rec = executeGraphQLRequestWithAuth(t, handler, unarchiveMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          closed.ID,
		}, actor)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0).Required()

		var result struct {
			UnarchiveCase archiveCaseRow `json:"unarchiveCase"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Value(t, result.UnarchiveCase.ID).Equal(int(closed.ID))
		gt.Value(t, result.UnarchiveCase.ArchivedAt).Nil()

		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, closed.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})

	t.Run("archiveCase rejects an open case", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		openCase := seed(t, repo, "Open case", types.CaseStatusOpen)

		rec := executeGraphQLRequestWithAuth(t, handler, archiveMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          openCase.ID,
		}, actor)
		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)

		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, openCase.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})

	// Without a token the usecase would treat the call as a system context and
	// skip the private-case check, so the resolver refuses it outright.
	t.Run("archiveCase requires an auth token", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		closed := seed(t, repo, "Closed case", types.CaseStatusClosed)

		rec := executeGraphQLRequest(t, handler, archiveMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          closed.ID,
		})
		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)

		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, closed.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})

	t.Run("archiveCase denies a non-member of a private case", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		created, err := repo.Case().Create(context.Background(), testWorkspaceID, &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Private closed case",
			Status:         types.CaseStatusClosed,
			IsPrivate:      true,
			ChannelUserIDs: []string{"U-MEMBER"},
		})
		gt.NoError(t, err).Required()

		rec := executeGraphQLRequestWithAuth(t, handler, archiveMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          created.ID,
		}, "U-OUTSIDER")
		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)

		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, created.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})
}

func TestGraphQLHandler_BulkCaseArchive(t *testing.T) {
	const actor = "U-ARCHIVER"

	bulkArchive := `
		mutation($workspaceId: String!, $ids: [Int!]!) {
			bulkArchiveCases(workspaceId: $workspaceId, ids: $ids)
		}
	`
	bulkUnarchive := `
		mutation($workspaceId: String!, $ids: [Int!]!) {
			bulkUnarchiveCases(workspaceId: $workspaceId, ids: $ids)
		}
	`

	seedClosed := func(t *testing.T, repo interfaces.Repository, title string) *model.Case {
		t.Helper()
		created, err := repo.Case().Create(context.Background(), testWorkspaceID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      title,
			Status:     types.CaseStatusClosed,
		})
		gt.NoError(t, err).Required()
		return created
	}

	acceptedIDs := func(t *testing.T, resp *graphQLResponse, field string) []int {
		t.Helper()
		var payload map[string][]int
		gt.NoError(t, json.Unmarshal(resp.Data, &payload)).Required()
		ids, ok := payload[field]
		gt.Bool(t, ok).True().Required()
		return ids
	}

	t.Run("returns the accepted ids and archives them in the async tail", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		c1 := seedClosed(t, repo, "A")
		c2 := seedClosed(t, repo, "B")

		rec := executeGraphQLRequestWithAuth(t, handler, bulkArchive, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"ids":         []int64{c1.ID, c2.ID},
		}, actor)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0).Required()

		accepted := acceptedIDs(t, resp, "bulkArchiveCases")
		gt.Array(t, accepted).Length(2).Required()
		gt.Value(t, accepted[0]).Equal(int(c1.ID))
		gt.Value(t, accepted[1]).Equal(int(c2.ID))

		async.Wait()
		for _, id := range []int64{c1.ID, c2.ID} {
			stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, id)
			gt.NoError(t, getErr).Required()
			gt.Bool(t, stored.IsArchived()).True()
		}
	})

	t.Run("bulk unarchive restores them in the async tail", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		c1 := seedClosed(t, repo, "A")
		rec := executeGraphQLRequestWithAuth(t, handler, bulkArchive, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"ids":         []int64{c1.ID},
		}, actor)
		gt.Array(t, parseGraphQLResponse(t, rec).Errors).Length(0).Required()
		async.Wait()

		rec = executeGraphQLRequestWithAuth(t, handler, bulkUnarchive, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"ids":         []int64{c1.ID},
		}, actor)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0).Required()
		gt.Array(t, acceptedIDs(t, resp, "bulkUnarchiveCases")).Length(1)

		async.Wait()
		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, c1.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})

	// The auth check runs before the dispatch, so an unauthenticated call must
	// start no background work at all.
	t.Run("requires an auth token and starts no background work without one", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		c1 := seedClosed(t, repo, "A")

		rec := executeGraphQLRequest(t, handler, bulkArchive, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"ids":         []int64{c1.ID},
		})
		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)

		async.Wait()
		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, c1.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})

	t.Run("an empty id list returns an empty list and does nothing", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		rec := executeGraphQLRequestWithAuth(t, handler, bulkArchive, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"ids":         []int64{},
		}, actor)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0).Required()
		gt.Array(t, acceptedIDs(t, resp, "bulkArchiveCases")).Length(0)
		async.Wait()
	})
}

func TestGraphQLHandler_CaseQuery(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	testCase := &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Title:       "Test Case for Query",
		Description: "Test case description for single query",
		AssigneeIDs: []string{"U123"},
	}

	createdCase, err := repo.Case().Create(ctx, testWorkspaceID, testCase)
	gt.NoError(t, err).Required()

	t.Run("query case by ID", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $id: Int!) {
				case(workspaceId: $workspaceId, id: $id) {
					id
					title
					description
					assigneeIDs
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          int(createdCase.ID),
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			Case struct {
				ID          int      `json:"id"`
				Title       string   `json:"title"`
				Description string   `json:"description"`
				AssigneeIDs []string `json:"assigneeIDs"`
			} `json:"case"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.Case.Title).Equal("Test Case for Query")

		gt.Value(t, result.Case.Description).Equal("Test case description for single query")

		gt.Array(t, result.Case.AssigneeIDs).Length(1)
		gt.Value(t, result.Case.AssigneeIDs[0]).Equal("U123")
	})

	t.Run("query non-existent case", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $id: Int!) {
				case(workspaceId: $workspaceId, id: $id) {
					id
					title
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          999999999,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		resp := parseGraphQLResponse(t, rec)

		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})
}

// seedSlackUsersHTTP stores minimal SlackUser records so case writes that
// reference these ids pass the existence check CaseUseCase now performs.
func seedSlackUsersHTTP(t *testing.T, repo interfaces.Repository, ids ...string) {
	t.Helper()
	users := make([]*model.SlackUser, 0, len(ids))
	for _, id := range ids {
		users = append(users, &model.SlackUser{ID: model.SlackUserID(id), Name: id})
	}
	gt.NoError(t, repo.SlackUser().SaveMany(context.Background(), users)).Required()
}

func TestGraphQLHandler_CreateCaseMutation(t *testing.T) {
	repo := memory.New()
	seedSlackUsersHTTP(t, repo, "U001", "U002")
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	t.Run("create new case", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: CreateCaseInput!) {
				createCase(workspaceId: $workspaceId, input: $input) {
					id
					title
					description
					assigneeIDs
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"title":       "New Case from Mutation",
				"description": "Case created via GraphQL mutation",
				"assigneeIDs": []string{"U001", "U002"},
			},
		}

		// Authenticated request — Case.Validate now requires every
		// persisted case to carry a ReporterID, and the only path
		// that fills it in production is the auth-context Token
		// injected by the Web auth middleware.
		rec := executeGraphQLRequestWithAuth(t, handler, mutation, variables, "U-CREATE-CASE-REPORTER")

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			CreateCase struct {
				ID          int      `json:"id"`
				Title       string   `json:"title"`
				Description string   `json:"description"`
				AssigneeIDs []string `json:"assigneeIDs"`
			} `json:"createCase"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.CreateCase.Title).Equal("New Case from Mutation")

		gt.Value(t, result.CreateCase.Description).Equal("Case created via GraphQL mutation")

		gt.Array(t, result.CreateCase.AssigneeIDs).Length(2)

		// Verify the case was actually saved to repository
		ctx := context.Background()
		savedCase, err := repo.Case().Get(ctx, testWorkspaceID, int64(result.CreateCase.ID))
		gt.NoError(t, err).Required()

		gt.Value(t, savedCase.Title).Equal("New Case from Mutation")

		gt.Array(t, savedCase.AssigneeIDs).Length(2)
	})

	t.Run("create case with missing required fields", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: CreateCaseInput!) {
				createCase(workspaceId: $workspaceId, input: $input) {
					id
					title
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"title": "", // Empty title should fail validation
			},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		resp := parseGraphQLResponse(t, rec)

		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})
}

func TestGraphQLHandler_CaseIsTestFlag(t *testing.T) {
	repo := memory.New()
	seedSlackUsersHTTP(t, repo, "U001")
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	createMutation := `
		mutation($workspaceId: String!, $input: CreateCaseInput!) {
			createCase(workspaceId: $workspaceId, input: $input) {
				id
				isTest
			}
		}
	`
	createVars := map[string]interface{}{
		"workspaceId": testWorkspaceID,
		"input": map[string]interface{}{
			"title":  "Test-flagged case",
			"isTest": true,
		},
	}
	rec := executeGraphQLRequestWithAuth(t, handler, createMutation, createVars, "U-ISTEST-REPORTER")
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp := parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0)

	var created struct {
		CreateCase struct {
			ID     int  `json:"id"`
			IsTest bool `json:"isTest"`
		} `json:"createCase"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &created)).Required()
	gt.Bool(t, created.CreateCase.IsTest).True()

	// Persisted as test.
	saved, err := repo.Case().Get(context.Background(), testWorkspaceID, int64(created.CreateCase.ID))
	gt.NoError(t, err).Required()
	gt.Bool(t, saved.IsTest).True()

	// updateCase flips it back off and the response/store reflect false.
	updateMutation := `
		mutation($workspaceId: String!, $input: UpdateCaseInput!) {
			updateCase(workspaceId: $workspaceId, input: $input) {
				id
				isTest
			}
		}
	`
	updateVars := map[string]interface{}{
		"workspaceId": testWorkspaceID,
		"input": map[string]interface{}{
			"id":     created.CreateCase.ID,
			"isTest": false,
		},
	}
	rec = executeGraphQLRequestWithAuth(t, handler, updateMutation, updateVars, "U-ISTEST-REPORTER")
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp = parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0)

	var updated struct {
		UpdateCase struct {
			IsTest bool `json:"isTest"`
		} `json:"updateCase"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &updated)).Required()
	gt.Bool(t, updated.UpdateCase.IsTest).False()

	saved, err = repo.Case().Get(context.Background(), testWorkspaceID, int64(created.CreateCase.ID))
	gt.NoError(t, err).Required()
	gt.Bool(t, saved.IsTest).False()
}

func TestGraphQLHandler_UpdateCaseMutation(t *testing.T) {
	repo := memory.New()
	seedSlackUsersHTTP(t, repo, "U001", "U002", "U003")
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	// Create a case first
	caseToUpdate := &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Title:       "Original Title",
		Description: "Original Description",
		AssigneeIDs: []string{"U001"},
	}
	createdCase, err := repo.Case().Create(ctx, testWorkspaceID, caseToUpdate)
	gt.NoError(t, err).Required()

	t.Run("update existing case", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: UpdateCaseInput!) {
				updateCase(workspaceId: $workspaceId, input: $input) {
					id
					title
					description
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"id":          createdCase.ID,
				"title":       "Updated Title",
				"description": "Updated Description",
			},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			UpdateCase struct {
				ID          int    `json:"id"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"updateCase"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.UpdateCase.Title).Equal("Updated Title")

		gt.Value(t, result.UpdateCase.Description).Equal("Updated Description")

		// Verify the case was actually updated in repository
		updatedCase, err := repo.Case().Get(ctx, testWorkspaceID, createdCase.ID)
		gt.NoError(t, err).Required()

		gt.Value(t, updatedCase.Title).Equal("Updated Title")
	})

	t.Run("update non-existent case", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: UpdateCaseInput!) {
				updateCase(workspaceId: $workspaceId, input: $input) {
					id
					title
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"id":          99999,
				"title":       "Should Fail",
				"description": "This should fail",
			},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		resp := parseGraphQLResponse(t, rec)

		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})
}

func TestGraphQLHandler_AssignUnassignCaseMutation(t *testing.T) {
	repo := memory.New()
	seedSlackUsersHTTP(t, repo, "U001", "U002", "U003")
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	createdCase, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Title:       "Assignment Target",
		Description: "Desc",
		AssigneeIDs: []string{"U001"},
	})
	gt.NoError(t, err).Required()

	t.Run("assignCase unions the new users", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $id: Int!, $userIDs: [String!]!) {
				assignCase(workspaceId: $workspaceId, id: $id, userIDs: $userIDs) {
					id
					assigneeIDs
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdCase.ID,
			"userIDs":     []string{"U002", "U003"},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			AssignCase struct {
				ID          int      `json:"id"`
				AssigneeIDs []string `json:"assigneeIDs"`
			} `json:"assignCase"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Value(t, result.AssignCase.AssigneeIDs).Equal([]string{"U001", "U002", "U003"})

		stored, err := repo.Case().Get(ctx, testWorkspaceID, createdCase.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.AssigneeIDs).Equal([]string{"U001", "U002", "U003"})
	})

	t.Run("unassignCase removes the listed users", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $id: Int!, $userIDs: [String!]!) {
				unassignCase(workspaceId: $workspaceId, id: $id, userIDs: $userIDs) {
					id
					assigneeIDs
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdCase.ID,
			"userIDs":     []string{"U001", "U003"},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			UnassignCase struct {
				ID          int      `json:"id"`
				AssigneeIDs []string `json:"assigneeIDs"`
			} `json:"unassignCase"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Value(t, result.UnassignCase.AssigneeIDs).Equal([]string{"U002"})

		stored, err := repo.Case().Get(ctx, testWorkspaceID, createdCase.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.AssigneeIDs).Equal([]string{"U002"})
	})

	t.Run("assignCase on a non-existent case errors", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $id: Int!, $userIDs: [String!]!) {
				assignCase(workspaceId: $workspaceId, id: $id, userIDs: $userIDs) {
					id
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          99999,
			"userIDs":     []string{"U001"},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)
		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})
}

func TestGraphQLHandler_DeleteCaseMutation(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	t.Run("delete existing case", func(t *testing.T) {
		// Create a case to delete
		caseToDelete := &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Case to Delete",
			Description: "This case will be deleted",
		}
		createdCase, err := repo.Case().Create(ctx, testWorkspaceID, caseToDelete)
		gt.NoError(t, err).Required()

		mutation := `
			mutation($workspaceId: String!, $id: Int!) {
				deleteCase(workspaceId: $workspaceId, id: $id)
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdCase.ID,
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			DeleteCase bool `json:"deleteCase"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Bool(t, result.DeleteCase).True()

		// Verify the case was actually deleted from repository
		_, err = repo.Case().Get(ctx, testWorkspaceID, createdCase.ID)
		gt.Value(t, err).NotNil()
	})

	t.Run("delete non-existent case", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $id: Int!) {
				deleteCase(workspaceId: $workspaceId, id: $id)
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          99999,
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		resp := parseGraphQLResponse(t, rec)

		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})

	t.Run("delete case also deletes associated actions", func(t *testing.T) {
		// Create a case
		caseWithActions := &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Case with Actions",
			Description: "This case has actions",
		}
		createdCase, err := repo.Case().Create(ctx, testWorkspaceID, caseWithActions)
		gt.NoError(t, err).Required()

		// Create actions associated with this case
		action1 := &model.Action{
			CaseID:      createdCase.ID,
			Title:       "Action 1",
			Description: "First action",
		}
		action2 := &model.Action{
			CaseID:      createdCase.ID,
			Title:       "Action 2",
			Description: "Second action",
		}

		_, err = repo.Action().Create(ctx, testWorkspaceID, action1)
		gt.NoError(t, err).Required()

		_, err = repo.Action().Create(ctx, testWorkspaceID, action2)
		gt.NoError(t, err).Required()

		mutation := `
			mutation($workspaceId: String!, $id: Int!) {
				deleteCase(workspaceId: $workspaceId, id: $id)
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdCase.ID,
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		// Verify associated actions were also deleted (cascade includes archived)
		actions, err := repo.Action().GetByCase(ctx, testWorkspaceID, createdCase.ID, interfaces.ActionListOptions{ArchiveScope: interfaces.ActionArchiveScopeAll})
		gt.NoError(t, err).Required()

		gt.Array(t, actions).Length(0)
	})
}

func TestGraphQLHandler_FrontendCasesQuery(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	// Create test case with all fields
	testCase := &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		Title:          "Frontend Test Case",
		Description:    "Test case for frontend query",
		AssigneeIDs:    []string{"U001"},
		SlackChannelID: "C12345",
	}

	createdCase, err := repo.Case().Create(ctx, testWorkspaceID, testCase)
	gt.NoError(t, err).Required()

	t.Run("frontend GET_CASES query format", func(t *testing.T) {
		// This mimics the query structure used by the frontend
		// Note: assignees field is omitted in this test as it requires SlackUser data
		query := `
			query GetCases($workspaceId: String!) {
				cases(workspaceId: $workspaceId) {
					id
					title
					description
					assigneeIDs
					slackChannelID
					createdAt
					updatedAt
					fields {
						fieldId
						value
					}
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		// Verify the response structure matches frontend expectations
		var result struct {
			Cases []struct {
				ID             int      `json:"id"`
				Title          string   `json:"title"`
				Description    string   `json:"description"`
				AssigneeIDs    []string `json:"assigneeIDs"`
				SlackChannelID string   `json:"slackChannelID"`
				Fields         []struct {
					FieldId string `json:"fieldId"`
					Value   any    `json:"value"`
				} `json:"fields"`
			} `json:"cases"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Array(t, result.Cases).Length(1).Required()

		c := result.Cases[0]
		gt.Value(t, int64(c.ID)).Equal(createdCase.ID)
		gt.Value(t, c.Title).Equal("Frontend Test Case")
		gt.Value(t, c.SlackChannelID).Equal("C12345")
	})

	t.Run("frontend GET_CASE query format", func(t *testing.T) {
		// This mimics the query structure used by the frontend for single case
		// Note: assignees, actions, knowledges fields are omitted in this test
		query := `
			query GetCase($workspaceId: String!, $id: Int!) {
				case(workspaceId: $workspaceId, id: $id) {
					id
					title
					description
					assigneeIDs
					slackChannelID
					createdAt
					updatedAt
					fields {
						fieldId
						value
					}
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          int(createdCase.ID),
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()
	})

	t.Run("verify slackChannelName field exists", func(t *testing.T) {
		query := `
			query($workspaceId: String!) {
				cases(workspaceId: $workspaceId) {
					id
					slackChannelName
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)
		gt.Value(t, resp.Data).NotNil()
	})

	t.Run("verify fieldID is case-sensitive (fieldId is correct)", func(t *testing.T) {
		// This should fail because fieldID (capital D) is incorrect
		query := `
			query($workspaceId: String!) {
				cases(workspaceId: $workspaceId) {
					id
					fields {
						fieldID
						value
					}
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		resp := parseGraphQLResponse(t, rec)

		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)

		// Check that the error message mentions the invalid field
		if len(resp.Errors) > 0 {
			gt.Value(t, resp.Errors[0].Message).Equal(`Cannot query field "fieldID" on type "FieldValue". Did you mean "fieldId"?`)
		}
	})
}

func TestGraphQLHandler_ErrorHandling(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	t.Run("invalid GraphQL syntax", func(t *testing.T) {
		query := `
			query($workspaceId: String!) {
				cases(workspaceId: $workspaceId) {
					invalid syntax here
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		resp := parseGraphQLResponse(t, rec)

		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})

	t.Run("unknown field", func(t *testing.T) {
		query := `
			query($workspaceId: String!) {
				cases(workspaceId: $workspaceId) {
					id
					nonExistentField
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		resp := parseGraphQLResponse(t, rec)

		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})

	t.Run("missing required variable", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $id: Int!) {
				case(workspaceId: $workspaceId, id: $id) {
					id
				}
			}
		`

		// Don't provide the required $id variable
		rec := executeGraphQLRequest(t, handler, query, nil)

		resp := parseGraphQLResponse(t, rec)

		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})
}

func TestGraphQLHandler_ActionMutations(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	// Create a case for actions to belong to
	testCase := &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Title:       "Test Case for Actions",
		Description: "Case for action tests",
	}
	createdCase, err := repo.Case().Create(ctx, testWorkspaceID, testCase)
	gt.NoError(t, err).Required()

	t.Run("create new action", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: CreateActionInput!) {
				createAction(workspaceId: $workspaceId, input: $input) {
					id
					caseID
					title
					description
					status
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"caseID":      createdCase.ID,
				"title":       "New Action",
				"description": "Action created via GraphQL",
				"status":      "TODO",
			},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			CreateAction struct {
				ID          int    `json:"id"`
				CaseID      int    `json:"caseID"`
				Title       string `json:"title"`
				Description string `json:"description"`
				Status      string `json:"status"`
			} `json:"createAction"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.CreateAction.Title).Equal("New Action")

		gt.Value(t, result.CreateAction.CaseID).Equal(int(createdCase.ID))

		// Verify the action was actually saved to repository
		savedAction, err := repo.Action().Get(ctx, testWorkspaceID, int64(result.CreateAction.ID))
		gt.NoError(t, err).Required()

		gt.Value(t, savedAction.Title).Equal("New Action")

		gt.Value(t, savedAction.CaseID).Equal(createdCase.ID)
	})

	t.Run("create action without caseID fails", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: CreateActionInput!) {
				createAction(workspaceId: $workspaceId, input: $input) {
					id
					title
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"title":       "Action without Case",
				"description": "Should fail",
			},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		resp := parseGraphQLResponse(t, rec)

		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})

	t.Run("update action", func(t *testing.T) {
		// Create an action to update
		actionToUpdate := &model.Action{
			CaseID:      createdCase.ID,
			Title:       "Original Action Title",
			Description: "Original Description",
			Status:      "TODO",
		}
		createdAction, err := repo.Action().Create(ctx, testWorkspaceID, actionToUpdate)
		gt.NoError(t, err).Required()

		mutation := `
			mutation($workspaceId: String!, $input: UpdateActionInput!) {
				updateAction(workspaceId: $workspaceId, input: $input) {
					id
					caseID
					title
					description
					status
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"id":          createdAction.ID,
				"title":       "Updated Action Title",
				"description": "Updated Description",
				"status":      "IN_PROGRESS",
			},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			UpdateAction struct {
				ID          int    `json:"id"`
				Title       string `json:"title"`
				Description string `json:"description"`
				Status      string `json:"status"`
			} `json:"updateAction"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.UpdateAction.Title).Equal("Updated Action Title")

		gt.Value(t, result.UpdateAction.Status).Equal("IN_PROGRESS")

		// Verify the action was actually updated in repository
		updatedAction, err := repo.Action().Get(ctx, testWorkspaceID, createdAction.ID)
		gt.NoError(t, err).Required()

		gt.Value(t, updatedAction.Title).Equal("Updated Action Title")
	})

	t.Run("archive and unarchive action", func(t *testing.T) {
		// Create an action to archive
		actionToArchive := &model.Action{
			CaseID:      createdCase.ID,
			Title:       "Action to Archive",
			Description: "This action will be archived",
		}
		createdAction, err := repo.Action().Create(ctx, testWorkspaceID, actionToArchive)
		gt.NoError(t, err).Required()

		archiveMutation := `
			mutation($workspaceId: String!, $id: Int!) {
				archiveAction(workspaceId: $workspaceId, id: $id) {
					id
					archived
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdAction.ID,
		}

		// Archive/Unarchive resolvers reject requests without an
		// authenticated user, so we route through the auth-injecting
		// helper. Using a test user ID is sufficient because the case
		// created by the surrounding test is public.
		const archiveTestUser = "U-archive-test"
		rec := executeGraphQLRequestWithAuth(t, handler, archiveMutation, variables, archiveTestUser)
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		gt.Value(t, resp.Data).NotNil().Required()

		var archiveResult struct {
			ArchiveAction struct {
				ID       int  `json:"id"`
				Archived bool `json:"archived"`
			} `json:"archiveAction"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &archiveResult)).Required()
		gt.Bool(t, archiveResult.ArchiveAction.Archived).True()

		// The action document is preserved; ArchivedAt is set
		stored, err := repo.Action().Get(ctx, testWorkspaceID, createdAction.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, stored.IsArchived()).True()

		// Now unarchive
		unarchiveMutation := `
			mutation($workspaceId: String!, $id: Int!) {
				unarchiveAction(workspaceId: $workspaceId, id: $id) {
					id
					archived
				}
			}
		`

		rec = executeGraphQLRequestWithAuth(t, handler, unarchiveMutation, variables, archiveTestUser)
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp = parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var unarchiveResult struct {
			UnarchiveAction struct {
				ID       int  `json:"id"`
				Archived bool `json:"archived"`
			} `json:"unarchiveAction"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &unarchiveResult)).Required()
		gt.Bool(t, unarchiveResult.UnarchiveAction.Archived).False()

		stored, err = repo.Action().Get(ctx, testWorkspaceID, createdAction.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})

	t.Run("archive action without auth token returns error", func(t *testing.T) {
		// Confirm the resolver hardening: a missing auth context must
		// surface as a GraphQL error rather than silently falling back
		// to ActorKindSystem.
		actionForUnauthArchive := &model.Action{
			CaseID:      createdCase.ID,
			Title:       "Action to Archive Unauth",
			Description: "Unauthenticated archive should fail",
		}
		created, err := repo.Action().Create(ctx, testWorkspaceID, actionForUnauthArchive)
		gt.NoError(t, err).Required()

		mutation := `
			mutation($workspaceId: String!, $id: Int!) {
				archiveAction(workspaceId: $workspaceId, id: $id) { id }
			}
		`
		rec := executeGraphQLRequest(t, handler, mutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          created.ID,
		})
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(1).Required()

		stored, err := repo.Action().Get(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})

	t.Run("bulk archive actions archives every id", func(t *testing.T) {
		// Three active actions standing in for a completed Kanban column.
		a1, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: createdCase.ID, Title: "Bulk 1", Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()
		a2, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: createdCase.ID, Title: "Bulk 2", Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()
		a3, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: createdCase.ID, Title: "Bulk 3", Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()

		// The mutation returns the accepted ids immediately and archives them
		// asynchronously, so the return type is a scalar id list (no subfields).
		mutation := `
			mutation($workspaceId: String!, $ids: [Int!]!) {
				bulkArchiveActions(workspaceId: $workspaceId, ids: $ids)
			}
		`
		const bulkTestUser = "U-bulk-archive-test"
		rec := executeGraphQLRequestWithAuth(t, handler, mutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"ids":         []int{int(a1.ID), int(a2.ID), int(a3.ID)},
		}, bulkTestUser)
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			BulkArchiveActions []int `json:"bulkArchiveActions"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		// The accepted ids echoed back must be exactly the ids we sent.
		gotIDs := map[int]bool{}
		for _, id := range result.BulkArchiveActions {
			gotIDs[id] = true
		}
		for _, id := range []int{int(a1.ID), int(a2.ID), int(a3.ID)} {
			gt.Bool(t, gotIDs[id]).True()
		}

		// Wait for the background archiving to finish, then assert each action
		// is actually archived in the repository.
		async.Wait()
		for _, id := range []int{int(a1.ID), int(a2.ID), int(a3.ID)} {
			stored, getErr := repo.Action().Get(ctx, testWorkspaceID, int64(id))
			gt.NoError(t, getErr).Required()
			gt.Bool(t, stored.IsArchived()).True()
		}
	})

	t.Run("bulk archive without auth token returns error", func(t *testing.T) {
		// Same hardening as archiveAction: no auth context must surface as a
		// GraphQL error rather than silently archiving as the system actor.
		// The auth check is synchronous, so no background work is dispatched.
		unauth, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: createdCase.ID, Title: "Bulk Unauth", Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()

		mutation := `
			mutation($workspaceId: String!, $ids: [Int!]!) {
				bulkArchiveActions(workspaceId: $workspaceId, ids: $ids)
			}
		`
		rec := executeGraphQLRequest(t, handler, mutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"ids":         []int{int(unauth.ID)},
		})
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(1).Required()

		async.Wait()
		stored, err := repo.Action().Get(ctx, testWorkspaceID, unauth.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})

	t.Run("actionsByCase respects archive filter", func(t *testing.T) {
		// Set up: one active and one archived action
		caseForArchiveQuery, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{ReporterID: "U-TEST-DEFAULT", Title: "archive query case", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
		gt.NoError(t, err).Required()

		active, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: caseForArchiveQuery.ID, Title: "active action", Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()

		archived, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: caseForArchiveQuery.ID, Title: "archived action", Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()
		now := time.Now().UTC()
		archived.ArchivedAt = &now
		_, err = repo.Action().Update(ctx, testWorkspaceID, archived)
		gt.NoError(t, err).Required()

		query := `
			query($workspaceId: String!, $caseID: Int!, $filter: ActionArchiveFilter) {
				actionsByCase(workspaceId: $workspaceId, caseID: $caseID, filter: $filter) {
					id
					archived
				}
			}
		`

		// Default: filter omitted → ACTIVE only
		rec := executeGraphQLRequest(t, handler, query, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"caseID":      caseForArchiveQuery.ID,
		})
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var defaultResult struct {
			ActionsByCase []struct {
				ID       int  `json:"id"`
				Archived bool `json:"archived"`
			} `json:"actionsByCase"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &defaultResult)).Required()
		gt.Array(t, defaultResult.ActionsByCase).Length(1).Required()
		gt.Value(t, defaultResult.ActionsByCase[0].ID).Equal(int(active.ID))

		// filter=ARCHIVED → archived only
		rec = executeGraphQLRequest(t, handler, query, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"caseID":      caseForArchiveQuery.ID,
			"filter":      "ARCHIVED",
		})
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp = parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var archivedResult struct {
			ActionsByCase []struct {
				ID       int  `json:"id"`
				Archived bool `json:"archived"`
			} `json:"actionsByCase"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &archivedResult)).Required()
		gt.Array(t, archivedResult.ActionsByCase).Length(1).Required()
		gt.Value(t, archivedResult.ActionsByCase[0].ID).Equal(int(archived.ID))
		gt.Bool(t, archivedResult.ActionsByCase[0].Archived).True()

		// filter=ALL → both
		rec = executeGraphQLRequest(t, handler, query, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"caseID":      caseForArchiveQuery.ID,
			"filter":      "ALL",
		})
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp = parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var allResult struct {
			ActionsByCase []struct {
				ID       int  `json:"id"`
				Archived bool `json:"archived"`
			} `json:"actionsByCase"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &allResult)).Required()
		gt.Array(t, allResult.ActionsByCase).Length(2)
	})

	t.Run("postActionSlackMessage surfaces usecase error", func(t *testing.T) {
		// The test harness wires no Slack service; calling
		// postActionSlackMessage exercises the resolver → usecase plumbing
		// and verifies that the strict "slack not configured" error
		// surfaces as a GraphQL error rather than a panic or silent OK.
		actionForRepost := &model.Action{
			CaseID:      createdCase.ID,
			Title:       "Action to repost",
			Description: "Initial Slack post never happened",
		}
		createdAction, err := repo.Action().Create(ctx, testWorkspaceID, actionForRepost)
		gt.NoError(t, err).Required()

		mutation := `
			mutation($workspaceId: String!, $id: Int!) {
				postActionSlackMessage(workspaceId: $workspaceId, id: $id) {
					id
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdAction.ID,
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})
}

func TestGraphQLHandler_NoopMutation(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	t.Run("noop mutation returns true", func(t *testing.T) {
		mutation := `
			mutation {
				noop
			}
		`

		rec := executeGraphQLRequest(t, handler, mutation, nil)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			Noop bool `json:"noop"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Bool(t, result.Noop).True()
	})
}

func TestGraphQLHandler_SlackUsersQuery(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	t.Run("empty slack users list", func(t *testing.T) {
		query := `
			query {
				slackUsers {
					id
					name
					realName
					imageUrl
				}
			}
		`

		rec := executeGraphQLRequest(t, handler, query, nil)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			SlackUsers []struct {
				ID       string  `json:"id"`
				Name     string  `json:"name"`
				RealName string  `json:"realName"`
				ImageURL *string `json:"imageUrl"`
			} `json:"slackUsers"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Array(t, result.SlackUsers).Length(0)
	})

	t.Run("slack users list with data", func(t *testing.T) {
		users := []*model.SlackUser{
			{
				ID:       "U001",
				Name:     "john.doe",
				RealName: "John Doe",
				ImageURL: "https://example.com/john.png",
			},
			{
				ID:       "U002",
				Name:     "jane.doe",
				RealName: "Jane Doe",
				ImageURL: "",
			},
		}
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, users)).Required()

		query := `
			query {
				slackUsers {
					id
					name
					realName
					imageUrl
				}
			}
		`

		rec := executeGraphQLRequest(t, handler, query, nil)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			SlackUsers []struct {
				ID       string  `json:"id"`
				Name     string  `json:"name"`
				RealName string  `json:"realName"`
				ImageURL *string `json:"imageUrl"`
			} `json:"slackUsers"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Array(t, result.SlackUsers).Length(2)

		foundJohn := false
		foundJane := false
		for _, u := range result.SlackUsers {
			if u.ID == "U001" {
				foundJohn = true
				gt.Value(t, u.Name).Equal("john.doe")
				gt.Value(t, u.RealName).Equal("John Doe")
				gt.Value(t, u.ImageURL).NotNil()
				gt.Value(t, *u.ImageURL).Equal("https://example.com/john.png")
			}
			if u.ID == "U002" {
				foundJane = true
				gt.Value(t, u.Name).Equal("jane.doe")
				gt.Value(t, u.RealName).Equal("Jane Doe")
				// ImageURL should be nil when empty
				gt.Value(t, u.ImageURL).Nil()
			}
		}

		gt.Bool(t, foundJohn).True()
		gt.Bool(t, foundJane).True()
	})
}

func TestGraphQLHandler_FrequentAssigneeIDsQuery(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()
	now := time.Now().UTC()

	seedCase := func(assigneeIDs ...string) {
		t.Helper()
		_, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:       "ranking case",
			Status:      types.CaseStatusOpen,
			ReporterID:  "U-REPORTER",
			AssigneeIDs: assigneeIDs,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		gt.NoError(t, err).Required()
	}
	seedCase("U-BUSY")
	seedCase("U-BUSY", "U-QUIET")

	query := `
		query($workspaceId: String!) {
			frequentAssigneeIDs(workspaceId: $workspaceId)
		}
	`
	variables := map[string]interface{}{"workspaceId": testWorkspaceID}

	run := func(t *testing.T) []string {
		t.Helper()
		rec := executeGraphQLRequest(t, handler, query, variables)
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			FrequentAssigneeIDs []string `json:"frequentAssigneeIDs"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		// The field is [String!]! so it must arrive as [], never JSON null —
		// which decodes to a nil slice here.
		gt.Bool(t, result.FrequentAssigneeIDs != nil).True()
		return result.FrequentAssigneeIDs
	}

	t.Run("cold cache returns an empty list", func(t *testing.T) {
		gt.Array(t, run(t)).Length(0)
	})

	t.Run("returns the ranking once the background refresh lands", func(t *testing.T) {
		async.Wait()

		ids := run(t)
		gt.Array(t, ids).Length(2).Required()
		gt.Value(t, ids[0]).Equal("U-BUSY")
		gt.Value(t, ids[1]).Equal("U-QUIET")
	})
}

func TestGraphQLHandler_SourceQueries(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	t.Run("empty sources list", func(t *testing.T) {
		query := `
			query($workspaceId: String!) {
				sources(workspaceId: $workspaceId) {
					id
					name
					sourceType
					description
					enabled
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			Sources []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"sources"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Array(t, result.Sources).Length(0)
	})

	t.Run("create and query notion source", func(t *testing.T) {
		source := &model.Source{
			Name:        "Test Notion Source",
			SourceType:  model.SourceTypeNotionDB,
			Description: "Test notion database source",
			Enabled:     true,
			NotionDBConfig: &model.NotionDBConfig{
				DatabaseID:    "db-123",
				DatabaseTitle: "My Database",
				DatabaseURL:   "https://notion.so/db-123",
			},
		}

		createdSource, err := repo.Source().Create(ctx, testWorkspaceID, source)
		gt.NoError(t, err).Required()

		// Test sources list query
		query := `
			query($workspaceId: String!) {
				sources(workspaceId: $workspaceId) {
					id
					name
					sourceType
					description
					enabled
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		var listResult struct {
			Sources []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				SourceType  string `json:"sourceType"`
				Description string `json:"description"`
				Enabled     bool   `json:"enabled"`
			} `json:"sources"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &listResult)).Required()

		gt.Number(t, len(listResult.Sources)).GreaterOrEqual(1)

		found := false
		for _, s := range listResult.Sources {
			if s.ID == string(createdSource.ID) {
				found = true
				gt.Value(t, s.Name).Equal("Test Notion Source")
				gt.Value(t, s.SourceType).Equal("NOTION_DB")
				gt.Bool(t, s.Enabled).True()
			}
		}
		gt.Bool(t, found).True()

		// Test single source query
		singleQuery := `
			query($workspaceId: String!, $id: String!) {
				source(workspaceId: $workspaceId, id: $id) {
					id
					name
					sourceType
					description
					enabled
				}
			}
		`

		singleVariables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          string(createdSource.ID),
		}

		rec = executeGraphQLRequest(t, handler, singleQuery, singleVariables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp = parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		var singleResult struct {
			Source struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				SourceType  string `json:"sourceType"`
				Description string `json:"description"`
				Enabled     bool   `json:"enabled"`
			} `json:"source"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &singleResult)).Required()

		gt.Value(t, singleResult.Source.ID).Equal(string(createdSource.ID))
		gt.Value(t, singleResult.Source.Name).Equal("Test Notion Source")
	})
}

func TestGraphQLHandler_SourceMutations(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	t.Run("create notion source", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: CreateNotionDBSourceInput!) {
				createNotionDBSource(workspaceId: $workspaceId, input: $input) {
					id
					name
					sourceType
					description
					enabled
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"name":        "My Notion DB",
				"description": "A test notion source",
				"databaseID":  "a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6",
				"enabled":     true,
			},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			CreateNotionDBSource struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				SourceType  string `json:"sourceType"`
				Description string `json:"description"`
				Enabled     bool   `json:"enabled"`
			} `json:"createNotionDBSource"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.CreateNotionDBSource.Name).Equal("My Notion DB")
		gt.Value(t, result.CreateNotionDBSource.SourceType).Equal("NOTION_DB")
		gt.Bool(t, result.CreateNotionDBSource.Enabled).True()
	})

	t.Run("create slack source", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: CreateSlackSourceInput!) {
				createSlackSource(workspaceId: $workspaceId, input: $input) {
					id
					name
					sourceType
					description
					enabled
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"name":        "My Slack Source",
				"description": "A test slack source",
				"channelIDs":  []string{"C001", "C002"},
				"enabled":     true,
			},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			CreateSlackSource struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				SourceType  string `json:"sourceType"`
				Description string `json:"description"`
				Enabled     bool   `json:"enabled"`
			} `json:"createSlackSource"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.CreateSlackSource.Name).Equal("My Slack Source")
		gt.Value(t, result.CreateSlackSource.SourceType).Equal("SLACK")
		gt.Bool(t, result.CreateSlackSource.Enabled).True()
	})

	t.Run("update source", func(t *testing.T) {
		source := &model.Source{
			Name:        "Source to Update",
			SourceType:  model.SourceTypeNotionDB,
			Description: "Original description",
			Enabled:     true,
			NotionDBConfig: &model.NotionDBConfig{
				DatabaseID:    "db-update",
				DatabaseTitle: "Update DB",
				DatabaseURL:   "https://notion.so/db-update",
			},
		}
		createdSource, err := repo.Source().Create(ctx, testWorkspaceID, source)
		gt.NoError(t, err).Required()

		mutation := `
			mutation($workspaceId: String!, $input: UpdateSourceInput!) {
				updateSource(workspaceId: $workspaceId, input: $input) {
					id
					name
					description
					enabled
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"id":          string(createdSource.ID),
				"name":        "Updated Source Name",
				"description": "Updated description",
				"enabled":     false,
			},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			UpdateSource struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Description string `json:"description"`
				Enabled     bool   `json:"enabled"`
			} `json:"updateSource"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.UpdateSource.Name).Equal("Updated Source Name")
		gt.Value(t, result.UpdateSource.Description).Equal("Updated description")
		gt.Bool(t, result.UpdateSource.Enabled).False()
	})

	t.Run("update slack source with channels", func(t *testing.T) {
		source := &model.Source{
			Name:        "Slack Source to Update",
			SourceType:  model.SourceTypeSlack,
			Description: "Original slack source",
			Enabled:     true,
			SlackConfig: &model.SlackConfig{
				Channels: []model.SlackChannel{
					{ID: "C001", Name: "C001"},
				},
			},
		}
		createdSource, err := repo.Source().Create(ctx, testWorkspaceID, source)
		gt.NoError(t, err).Required()

		mutation := `
			mutation($workspaceId: String!, $input: UpdateSlackSourceInput!) {
				updateSlackSource(workspaceId: $workspaceId, input: $input) {
					id
					name
					description
					enabled
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"id":         string(createdSource.ID),
				"name":       "Updated Slack Source",
				"channelIDs": []string{"C003", "C004"},
			},
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			UpdateSlackSource struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Description string `json:"description"`
				Enabled     bool   `json:"enabled"`
			} `json:"updateSlackSource"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.UpdateSlackSource.Name).Equal("Updated Slack Source")

		// Verify the channels were updated in repository
		updatedSource, err := repo.Source().Get(ctx, testWorkspaceID, createdSource.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, updatedSource.SlackConfig).NotNil()
		gt.Array(t, updatedSource.SlackConfig.Channels).Length(2)
	})

	t.Run("delete source", func(t *testing.T) {
		source := &model.Source{
			Name:        "Source to Delete",
			SourceType:  model.SourceTypeNotionDB,
			Description: "Will be deleted",
			Enabled:     true,
			NotionDBConfig: &model.NotionDBConfig{
				DatabaseID:    "db-delete",
				DatabaseTitle: "Delete DB",
				DatabaseURL:   "https://notion.so/db-delete",
			},
		}
		createdSource, err := repo.Source().Create(ctx, testWorkspaceID, source)
		gt.NoError(t, err).Required()

		mutation := `
			mutation($workspaceId: String!, $id: String!) {
				deleteSource(workspaceId: $workspaceId, id: $id)
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          string(createdSource.ID),
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			DeleteSource bool `json:"deleteSource"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Bool(t, result.DeleteSource).True()

		// Verify the source was actually deleted
		_, err = repo.Source().Get(ctx, testWorkspaceID, createdSource.ID)
		gt.Value(t, err).NotNil()
	})

	t.Run("validate notion DB without service", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $databaseID: String!) {
				validateNotionDB(workspaceId: $workspaceId, databaseID: $databaseID) {
					valid
					errorMessage
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"databaseID":  "b1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6",
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			ValidateNotionDB struct {
				Valid        bool    `json:"valid"`
				ErrorMessage *string `json:"errorMessage"`
			} `json:"validateNotionDB"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		// Without Notion service configured, validation should return not valid
		gt.Bool(t, result.ValidateNotionDB.Valid).False()
		gt.Value(t, result.ValidateNotionDB.ErrorMessage).NotNil()
	})

	t.Run("validate notion DB with empty ID", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $databaseID: String!) {
				validateNotionDB(workspaceId: $workspaceId, databaseID: $databaseID) {
					valid
					errorMessage
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"databaseID":  "",
		}

		rec := executeGraphQLRequest(t, handler, mutation, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			ValidateNotionDB struct {
				Valid        bool    `json:"valid"`
				ErrorMessage *string `json:"errorMessage"`
			} `json:"validateNotionDB"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Bool(t, result.ValidateNotionDB.Valid).False()
	})
}

func TestGraphQLHandler_ActionsByCaseQuery(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	// Create a case
	testCase := &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Title:       "Case with Multiple Actions",
		Description: "Testing actions query",
	}
	createdCase, err := repo.Case().Create(ctx, testWorkspaceID, testCase)
	gt.NoError(t, err).Required()

	// Create multiple actions for this case
	action1 := &model.Action{
		CaseID:      createdCase.ID,
		Title:       "First Action",
		Description: "Action 1",
		Status:      "TODO",
	}
	action2 := &model.Action{
		CaseID:      createdCase.ID,
		Title:       "Second Action",
		Description: "Action 2",
		Status:      "IN_PROGRESS",
	}
	action3 := &model.Action{
		CaseID:      createdCase.ID,
		Title:       "Third Action",
		Description: "Action 3",
		Status:      "COMPLETED",
	}

	_, err = repo.Action().Create(ctx, testWorkspaceID, action1)
	gt.NoError(t, err).Required()
	_, err = repo.Action().Create(ctx, testWorkspaceID, action2)
	gt.NoError(t, err).Required()
	_, err = repo.Action().Create(ctx, testWorkspaceID, action3)
	gt.NoError(t, err).Required()

	t.Run("query actions by case ID", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $caseID: Int!) {
				actionsByCase(workspaceId: $workspaceId, caseID: $caseID) {
					id
					caseID
					title
					description
					status
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"caseID":      createdCase.ID,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		gt.Value(t, resp.Data).NotNil().Required()

		var result struct {
			ActionsByCase []struct {
				ID          int    `json:"id"`
				CaseID      int    `json:"caseID"`
				Title       string `json:"title"`
				Description string `json:"description"`
				Status      string `json:"status"`
			} `json:"actionsByCase"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Array(t, result.ActionsByCase).Length(3)

		// Verify all actions belong to the correct case
		for _, action := range result.ActionsByCase {
			gt.Value(t, action.CaseID).Equal(int(createdCase.ID))
		}
	})

	t.Run("query actions for non-existent case returns empty list", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $caseID: Int!) {
				actionsByCase(workspaceId: $workspaceId, caseID: $caseID) {
					id
					title
				}
			}
		`

		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"caseID":      99999,
		}

		rec := executeGraphQLRequest(t, handler, query, variables)

		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)

		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			ActionsByCase []struct {
				ID    int    `json:"id"`
				Title string `json:"title"`
			} `json:"actionsByCase"`
		}

		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Array(t, result.ActionsByCase).Length(0)
	})
}

func TestGraphQLHandler_PrivateCaseAccessControl(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()

	// Create a private case with specific channel users
	privateCase := &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		Title:          "Private Case",
		Description:    "Secret information",
		IsPrivate:      true,
		ChannelUserIDs: []string{"UMEMBER"},
		AssigneeIDs:    []string{"UMEMBER"},
	}
	createdPrivate, err := repo.Case().Create(ctx, testWorkspaceID, privateCase)
	gt.NoError(t, err).Required()

	// Create a public case for comparison
	publicCase := &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Title:       "Public Case",
		Description: "Public information",
		AssigneeIDs: []string{},
	}
	createdPublic, err := repo.Case().Create(ctx, testWorkspaceID, publicCase)
	gt.NoError(t, err).Required()

	// Create an action on the private case
	privateAction := &model.Action{
		CaseID:      createdPrivate.ID,
		Title:       "Private Action",
		Description: "Secret action",
	}
	createdPrivateAction, err := repo.Action().Create(ctx, testWorkspaceID, privateAction)
	gt.NoError(t, err).Required()

	// Create an action on the public case
	publicAction := &model.Action{
		CaseID:      createdPublic.ID,
		Title:       "Public Action",
		Description: "Public action",
	}
	_, err = repo.Action().Create(ctx, testWorkspaceID, publicAction)
	gt.NoError(t, err).Required()

	t.Run("member can see private case details", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $id: Int!) {
				case(workspaceId: $workspaceId, id: $id) {
					id
					title
					description
					isPrivate
					accessDenied
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdPrivate.ID,
		}

		rec := executeGraphQLRequestWithAuth(t, handler, query, variables, "UMEMBER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			Case struct {
				ID           int    `json:"id"`
				Title        string `json:"title"`
				Description  string `json:"description"`
				IsPrivate    bool   `json:"isPrivate"`
				AccessDenied bool   `json:"accessDenied"`
			} `json:"case"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.Case.Title).Equal("Private Case")
		gt.Value(t, result.Case.Description).Equal("Secret information")
		gt.Value(t, result.Case.IsPrivate).Equal(true)
		gt.Value(t, result.Case.AccessDenied).Equal(false)
	})

	t.Run("non-member sees restricted private case", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $id: Int!) {
				case(workspaceId: $workspaceId, id: $id) {
					id
					title
					description
					isPrivate
					accessDenied
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdPrivate.ID,
		}

		rec := executeGraphQLRequestWithAuth(t, handler, query, variables, "UOTHER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			Case struct {
				ID           int    `json:"id"`
				Title        string `json:"title"`
				Description  string `json:"description"`
				IsPrivate    bool   `json:"isPrivate"`
				AccessDenied bool   `json:"accessDenied"`
			} `json:"case"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		// Title and description should be empty (restricted)
		gt.Value(t, result.Case.Title).Equal("")
		gt.Value(t, result.Case.Description).Equal("")
		gt.Value(t, result.Case.IsPrivate).Equal(true)
		gt.Value(t, result.Case.AccessDenied).Equal(true)
	})

	t.Run("cases list restricts private cases for non-members", func(t *testing.T) {
		query := `
			query($workspaceId: String!) {
				cases(workspaceId: $workspaceId) {
					id
					title
					isPrivate
					accessDenied
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequestWithAuth(t, handler, query, variables, "UOTHER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			Cases []struct {
				ID           int    `json:"id"`
				Title        string `json:"title"`
				IsPrivate    bool   `json:"isPrivate"`
				AccessDenied bool   `json:"accessDenied"`
			} `json:"cases"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Array(t, result.Cases).Length(2)

		for _, c := range result.Cases {
			if int64(c.ID) == createdPrivate.ID {
				// Private case should be restricted
				gt.Value(t, c.Title).Equal("")
				gt.Value(t, c.AccessDenied).Equal(true)
				gt.Value(t, c.IsPrivate).Equal(true)
			} else {
				// Public case should be fully visible
				gt.Value(t, c.Title).Equal("Public Case")
				gt.Value(t, c.AccessDenied).Equal(false)
			}
		}
	})

	t.Run("non-member cannot update private case", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: UpdateCaseInput!) {
				updateCase(workspaceId: $workspaceId, input: $input) {
					id
					title
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"id":    createdPrivate.ID,
				"title": "Hacked Title",
			},
		}

		rec := executeGraphQLRequestWithAuth(t, handler, mutation, variables, "UOTHER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})

	t.Run("non-member cannot delete private case", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $id: Int!) {
				deleteCase(workspaceId: $workspaceId, id: $id)
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdPrivate.ID,
		}

		rec := executeGraphQLRequestWithAuth(t, handler, mutation, variables, "UOTHER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)

		// Verify case still exists
		c, err := repo.Case().Get(ctx, testWorkspaceID, createdPrivate.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, c.Title).Equal("Private Case")
	})

	t.Run("actions by private case returns empty for non-member", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $caseId: Int!) {
				actionsByCase(workspaceId: $workspaceId, caseID: $caseId) {
					id
					title
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"caseId":      createdPrivate.ID,
		}

		rec := executeGraphQLRequestWithAuth(t, handler, query, variables, "UOTHER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			ActionsByCase []struct {
				ID    int    `json:"id"`
				Title string `json:"title"`
			} `json:"actionsByCase"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Array(t, result.ActionsByCase).Length(0)
	})

	t.Run("member can see actions by private case", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $caseId: Int!) {
				actionsByCase(workspaceId: $workspaceId, caseID: $caseId) {
					id
					title
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"caseId":      createdPrivate.ID,
		}

		rec := executeGraphQLRequestWithAuth(t, handler, query, variables, "UMEMBER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			ActionsByCase []struct {
				ID    int    `json:"id"`
				Title string `json:"title"`
			} `json:"actionsByCase"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Array(t, result.ActionsByCase).Length(1)
		gt.Value(t, result.ActionsByCase[0].Title).Equal("Private Action")
	})

	t.Run("private case sub-resolvers return empty for non-member", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $id: Int!) {
				case(workspaceId: $workspaceId, id: $id) {
					id
					accessDenied
					actions {
						id
						title
					}
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdPrivate.ID,
		}

		rec := executeGraphQLRequestWithAuth(t, handler, query, variables, "UOTHER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			Case struct {
				ID           int  `json:"id"`
				AccessDenied bool `json:"accessDenied"`
				Actions      []struct {
					ID int `json:"id"`
				} `json:"actions"`
			} `json:"case"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.Case.AccessDenied).Equal(true)
		gt.Array(t, result.Case.Actions).Length(0)
	})

	t.Run("action comments are hidden from a non-member and readable by a member", func(t *testing.T) {
		seededAt := time.Now().UTC()
		seeded := &model.ActionComment{
			ID:        "comment-private-1",
			ActionID:  createdPrivateAction.ID,
			AuthorID:  "UMEMBER",
			Body:      "secret note",
			CreatedAt: seededAt,
			UpdatedAt: seededAt,
		}
		gt.NoError(t, repo.ActionComment().Create(ctx, testWorkspaceID, createdPrivateAction.ID, seeded)).Required()

		query := `
			query($workspaceId: String!, $id: Int!) {
				action(workspaceId: $workspaceId, id: $id) {
					id
					comments {
						items { id body authorID edited }
						nextCursor
					}
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdPrivateAction.ID,
		}

		var result struct {
			Action struct {
				ID       int `json:"id"`
				Comments struct {
					Items []struct {
						ID       string `json:"id"`
						Body     string `json:"body"`
						AuthorID string `json:"authorID"`
						Edited   bool   `json:"edited"`
					} `json:"items"`
					NextCursor string `json:"nextCursor"`
				} `json:"comments"`
			} `json:"action"`
		}

		rec := executeGraphQLRequestWithAuth(t, handler, query, variables, "UOTHER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Array(t, result.Action.Comments.Items).Length(0)

		rec = executeGraphQLRequestWithAuth(t, handler, query, variables, "UMEMBER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp = parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Array(t, result.Action.Comments.Items).Length(1).Required()
		gt.Value(t, result.Action.Comments.Items[0].ID).Equal("comment-private-1")
		gt.Value(t, result.Action.Comments.Items[0].Body).Equal("secret note")
		gt.Value(t, result.Action.Comments.Items[0].AuthorID).Equal("UMEMBER")
		gt.Value(t, result.Action.Comments.Items[0].Edited).Equal(false)

		// A system / bot context (no auth token) reads freely, matching the
		// other private-case sub-resolvers.
		rec = executeGraphQLRequest(t, handler, query, variables)
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp = parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Array(t, result.Action.Comments.Items).Length(1)
	})

	t.Run("non-member cannot comment on a private case action", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: CreateActionCommentInput!) {
				createActionComment(workspaceId: $workspaceId, input: $input) { id }
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"actionId": createdPrivateAction.ID,
				"body":     "let me in",
			},
		}

		before, _, err := repo.ActionComment().List(ctx, testWorkspaceID, createdPrivateAction.ID, 100, "")
		gt.NoError(t, err).Required()

		rec := executeGraphQLRequestWithAuth(t, handler, mutation, variables, "UOUTSIDER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1).Required()

		// The mutation must not have landed. (The FORBIDDEN classification of
		// ErrAccessDenied is pinned in pkg/controller/graphql/errors_test.go;
		// this test's handler does not attach extensions.)
		after, _, err := repo.ActionComment().List(ctx, testWorkspaceID, createdPrivateAction.ID, 100, "")
		gt.NoError(t, err).Required()
		gt.Number(t, len(after)).Equal(len(before))
	})

	t.Run("member can comment on a private case action", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: CreateActionCommentInput!) {
				createActionComment(workspaceId: $workspaceId, input: $input) {
					id
					body
					authorID
					edited
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"actionId": createdPrivateAction.ID,
				"body":     "  looks fine  ",
			},
		}

		rec := executeGraphQLRequestWithAuth(t, handler, mutation, variables, "UMEMBER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			CreateActionComment struct {
				ID       string `json:"id"`
				Body     string `json:"body"`
				AuthorID string `json:"authorID"`
				Edited   bool   `json:"edited"`
			} `json:"createActionComment"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Value(t, result.CreateActionComment.Body).Equal("looks fine")
		gt.Value(t, result.CreateActionComment.AuthorID).Equal("UMEMBER")
		gt.Value(t, result.CreateActionComment.Edited).Equal(false)
	})

	t.Run("without auth token private case is fully visible (backward compat)", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $id: Int!) {
				case(workspaceId: $workspaceId, id: $id) {
					id
					title
					isPrivate
					accessDenied
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          createdPrivate.ID,
		}

		// No auth token (system/bot context) — should bypass access control
		rec := executeGraphQLRequest(t, handler, query, variables)
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			Case struct {
				ID           int    `json:"id"`
				Title        string `json:"title"`
				IsPrivate    bool   `json:"isPrivate"`
				AccessDenied bool   `json:"accessDenied"`
			} `json:"case"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Value(t, result.Case.Title).Equal("Private Case")
		gt.Value(t, result.Case.AccessDenied).Equal(false)
	})

	t.Run("assistLogs root query returns empty for non-member of private case", func(t *testing.T) {
		// Create an assist log linked to the private case
		assistLog := &model.AssistLog{
			CaseID:  createdPrivate.ID,
			Summary: "Secret analysis",
			Actions: "Secret actions",
		}
		_, err := repo.AssistLog().Create(ctx, testWorkspaceID, createdPrivate.ID, assistLog)
		gt.NoError(t, err).Required()

		query := `
			query($workspaceId: String!, $caseId: Int!) {
				assistLogs(workspaceId: $workspaceId, caseId: $caseId) {
					items {
						id
						summary
					}
					totalCount
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"caseId":      createdPrivate.ID,
		}

		// Non-member should get empty
		rec := executeGraphQLRequestWithAuth(t, handler, query, variables, "UOTHER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			AssistLogs struct {
				Items []struct {
					ID      string `json:"id"`
					Summary string `json:"summary"`
				} `json:"items"`
				TotalCount int `json:"totalCount"`
			} `json:"assistLogs"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Array(t, result.AssistLogs.Items).Length(0)
		gt.Value(t, result.AssistLogs.TotalCount).Equal(0)

		// Member should see it
		rec = executeGraphQLRequestWithAuth(t, handler, query, variables, "UMEMBER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp = parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var memberResult struct {
			AssistLogs struct {
				Items []struct {
					ID      string `json:"id"`
					Summary string `json:"summary"`
				} `json:"items"`
				TotalCount int `json:"totalCount"`
			} `json:"assistLogs"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &memberResult)).Required()
		gt.Number(t, len(memberResult.AssistLogs.Items)).GreaterOrEqual(1)
		gt.Value(t, memberResult.AssistLogs.Items[0].Summary).Equal("Secret analysis")
	})
}

func TestGraphQLHandler_ActionStepMutations(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()
	ctx := context.Background()

	// Create a Case + Action that the steps will live under.
	c, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Title:      "Step E2E Case",
	})
	gt.NoError(t, err).Required()
	action, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
		CaseID: c.ID,
		Title:  "Step E2E Action",
		Status: types.ActionStatusTodo,
	})
	gt.NoError(t, err).Required()

	stepFields := `
		id
		actionID
		title
		done
		doneAt
	`

	var stepID string
	t.Run("addActionStep creates a step and exposes it via Action.steps", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: AddActionStepInput!) {
				addActionStep(workspaceId: $workspaceId, input: $input) {
					` + stepFields + `
				}
			}
		`
		rec := executeGraphQLRequest(t, handler, mutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"actionId": action.ID,
				"title":    "first step",
			},
		})
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			AddActionStep struct {
				ID       string  `json:"id"`
				ActionID int     `json:"actionID"`
				Title    string  `json:"title"`
				Done     bool    `json:"done"`
				DoneAt   *string `json:"doneAt"`
			} `json:"addActionStep"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.String(t, result.AddActionStep.ID).NotEqual("")
		gt.Value(t, result.AddActionStep.Title).Equal("first step")
		gt.Bool(t, result.AddActionStep.Done).False()
		gt.Value(t, result.AddActionStep.ActionID).Equal(int(action.ID))
		gt.Value(t, result.AddActionStep.DoneAt).Nil()
		stepID = result.AddActionStep.ID

		stored, err := repo.ActionStep().Get(ctx, testWorkspaceID, action.ID, stepID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Title).Equal("first step")
	})

	t.Run("Action.steps and stepProgress reflect the new step", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $id: Int!) {
				action(workspaceId: $workspaceId, id: $id) {
					steps { id title done }
					stepProgress { done total }
				}
			}
		`
		rec := executeGraphQLRequest(t, handler, query, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          action.ID,
		})
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			Action struct {
				Steps []struct {
					ID    string `json:"id"`
					Title string `json:"title"`
					Done  bool   `json:"done"`
				} `json:"steps"`
				StepProgress struct {
					Done  int `json:"done"`
					Total int `json:"total"`
				} `json:"stepProgress"`
			} `json:"action"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Array(t, result.Action.Steps).Length(1).Required()
		gt.Value(t, result.Action.Steps[0].ID).Equal(stepID)
		gt.Value(t, result.Action.StepProgress.Done).Equal(0)
		gt.Value(t, result.Action.StepProgress.Total).Equal(1)
	})

	t.Run("setActionStepDone toggles state and updates progress", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: SetActionStepDoneInput!) {
				setActionStepDone(workspaceId: $workspaceId, input: $input) {
					id done doneAt
				}
			}
		`
		rec := executeGraphQLRequest(t, handler, mutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"actionId": action.ID,
				"stepId":   stepID,
				"done":     true,
			},
		})
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		var result struct {
			SetActionStepDone struct {
				ID     string  `json:"id"`
				Done   bool    `json:"done"`
				DoneAt *string `json:"doneAt"`
			} `json:"setActionStepDone"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Bool(t, result.SetActionStepDone.Done).True()
		gt.Value(t, result.SetActionStepDone.DoneAt).NotNil()
	})

	t.Run("renameActionStep changes title", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: RenameActionStepInput!) {
				renameActionStep(workspaceId: $workspaceId, input: $input) {
					id title
				}
			}
		`
		rec := executeGraphQLRequest(t, handler, mutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"actionId": action.ID,
				"stepId":   stepID,
				"title":    "renamed step",
			},
		})
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		stored, err := repo.ActionStep().Get(ctx, testWorkspaceID, action.ID, stepID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Title).Equal("renamed step")
	})

	t.Run("deleteActionStep removes the step and progress goes to 0/0", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: DeleteActionStepInput!) {
				deleteActionStep(workspaceId: $workspaceId, input: $input)
			}
		`
		rec := executeGraphQLRequest(t, handler, mutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"actionId": action.ID,
				"stepId":   stepID,
			},
		})
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		steps, err := repo.ActionStep().List(ctx, testWorkspaceID, action.ID)
		gt.NoError(t, err).Required()
		gt.Array(t, steps).Length(0)
	})

	t.Run("authenticated mutation attributes the step and ActionEvent to the user", func(t *testing.T) {
		// Fresh action so the ActionEvent feed is isolated.
		freshAction, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: c.ID,
			Title:  "Attribution Action",
			Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()

		const userID = "UATTRIBUTOR"
		mutation := `
			mutation($workspaceId: String!, $input: AddActionStepInput!) {
				addActionStep(workspaceId: $workspaceId, input: $input) {
					id
				}
			}
		`
		rec := executeGraphQLRequestWithAuth(t, handler, mutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"actionId": freshAction.ID,
				"title":    "attributed step",
			},
		}, userID)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		var result struct {
			AddActionStep struct {
				ID string `json:"id"`
			} `json:"addActionStep"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		stored, err := repo.ActionStep().Get(ctx, testWorkspaceID, freshAction.ID, result.AddActionStep.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.CreatedBy).Equal(userID)

		events, _, err := repo.ActionEvent().List(ctx, testWorkspaceID, freshAction.ID, 10, "")
		gt.NoError(t, err).Required()
		// Newest first; the STEP_ADDED event must carry the user id.
		gt.Number(t, len(events)).GreaterOrEqual(1).Required()
		gt.Value(t, events[0].Kind).Equal(types.ActionEventStepAdded)
		gt.Value(t, events[0].ActorID).Equal(userID)
	})
}

func TestGraphQLHandler_ActionStepPrivateCaseAccessControl(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()
	ctx := context.Background()

	const memberID = "UMEMBER"
	const intruderID = "UINTRUDER"
	c, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		Title:          "Private Step Case",
		IsPrivate:      true,
		ChannelUserIDs: []string{memberID},
	})
	gt.NoError(t, err).Required()
	action, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
		CaseID: c.ID,
		Title:  "Private Step Action",
		Status: types.ActionStatusTodo,
	})
	gt.NoError(t, err).Required()
	gt.NoError(t, repo.ActionStep().Put(ctx, testWorkspaceID, &model.ActionStep{
		ID:        "step-private-1",
		ActionID:  action.ID,
		Title:     "secret step",
		CreatedBy: memberID,
	})).Required()

	query := `
		query($workspaceId: String!, $id: Int!) {
			action(workspaceId: $workspaceId, id: $id) {
				steps { id title }
				stepProgress { done total }
			}
		}
	`

	t.Run("non-member sees empty steps and 0/0 progress", func(t *testing.T) {
		rec := executeGraphQLRequestWithAuth(t, handler, query, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          action.ID,
		}, intruderID)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		var result struct {
			Action struct {
				Steps []struct {
					ID string `json:"id"`
				} `json:"steps"`
				StepProgress struct {
					Done  int `json:"done"`
					Total int `json:"total"`
				} `json:"stepProgress"`
			} `json:"action"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Array(t, result.Action.Steps).Length(0)
		gt.Value(t, result.Action.StepProgress.Done).Equal(0)
		gt.Value(t, result.Action.StepProgress.Total).Equal(0)
	})

	t.Run("member sees the step and 0/1 progress", func(t *testing.T) {
		rec := executeGraphQLRequestWithAuth(t, handler, query, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          action.ID,
		}, memberID)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		var result struct {
			Action struct {
				Steps []struct {
					ID    string `json:"id"`
					Title string `json:"title"`
				} `json:"steps"`
				StepProgress struct {
					Done  int `json:"done"`
					Total int `json:"total"`
				} `json:"stepProgress"`
			} `json:"action"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Array(t, result.Action.Steps).Length(1).Required()
		gt.Value(t, result.Action.Steps[0].ID).Equal("step-private-1")
		gt.Value(t, result.Action.StepProgress.Total).Equal(1)
	})

	t.Run("non-member addActionStep is rejected", func(t *testing.T) {
		mutation := `
			mutation($workspaceId: String!, $input: AddActionStepInput!) {
				addActionStep(workspaceId: $workspaceId, input: $input) {
					id
				}
			}
		`
		rec := executeGraphQLRequestWithAuth(t, handler, mutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"actionId": action.ID,
				"title":    "intruder step",
			},
		}, intruderID)
		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})
}

func TestGraphQLHandler_DraftsLifecycle(t *testing.T) {
	const (
		reporterID = "U-REPORTER"
		strangerID = "U-STRANGER"
	)

	t.Run("save → list → submit promotes to OPEN", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		// Persist a draft via the repository (representing the Slack
		// Save-as-Draft outcome — the equivalent of CaseUseCase.CreateDraft
		// with the reporter wired up by the Slack handler).
		ctx := context.Background()
		draft, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:      "Half written",
			Status:     types.CaseStatusDraft,
			ReporterID: reporterID,
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
		})
		gt.NoError(t, err).Required()

		// drafts query as the reporter returns the draft.
		draftsQuery := `
			query($workspaceId: String!) {
				drafts(workspaceId: $workspaceId) {
					id
					title
					status
				}
			}
		`
		rec := executeGraphQLRequestWithAuth(t, handler, draftsQuery, map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}, reporterID)
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var listResult struct {
			Drafts []struct {
				ID     int    `json:"id"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"drafts"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &listResult)).Required()
		gt.Array(t, listResult.Drafts).Length(1).Required()
		gt.Value(t, int64(listResult.Drafts[0].ID)).Equal(draft.ID)
		gt.Value(t, listResult.Drafts[0].Status).Equal("DRAFT")

		// Stranger sees the public draft too — drafts are workspace-wide
		// readable so any team member can pick up an in-progress entry.
		strangerRec := executeGraphQLRequestWithAuth(t, handler, draftsQuery, map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}, strangerID)
		strangerResp := parseGraphQLResponse(t, strangerRec)
		gt.Array(t, strangerResp.Errors).Length(0)
		var strangerResult struct {
			Drafts []struct {
				ID int `json:"id"`
			} `json:"drafts"`
		}
		gt.NoError(t, json.Unmarshal(strangerResp.Data, &strangerResult)).Required()
		gt.Array(t, strangerResult.Drafts).Length(1).Required()
		gt.Value(t, int64(strangerResult.Drafts[0].ID)).Equal(draft.ID)

		// Stranger CAN fetch the public draft via the `case` query — only
		// private drafts are hidden from non-reporters.
		caseQuery := `
			query($workspaceId: String!, $id: Int!) {
				case(workspaceId: $workspaceId, id: $id) {
					id
					title
					status
				}
			}
		`
		caseRec := executeGraphQLRequestWithAuth(t, handler, caseQuery, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          int(draft.ID),
		}, strangerID)
		caseResp := parseGraphQLResponse(t, caseRec)
		gt.Array(t, caseResp.Errors).Length(0)
		var caseResult struct {
			Case struct {
				ID     int    `json:"id"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"case"`
		}
		gt.NoError(t, json.Unmarshal(caseResp.Data, &caseResult)).Required()
		gt.Value(t, int64(caseResult.Case.ID)).Equal(draft.ID)
		gt.Value(t, caseResult.Case.Status).Equal("DRAFT")

		// Reporter submits → Case is promoted to OPEN. Submit requires a
		// non-empty title, which we provided above.
		submitMutation := `
			mutation($workspaceId: String!, $id: Int!) {
				submitDraft(workspaceId: $workspaceId, id: $id) {
					id
					title
					status
				}
			}
		`
		submitRec := executeGraphQLRequestWithAuth(t, handler, submitMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          int(draft.ID),
		}, reporterID)
		submitResp := parseGraphQLResponse(t, submitRec)
		gt.Array(t, submitResp.Errors).Length(0)
		var submitResult struct {
			SubmitDraft struct {
				ID     int    `json:"id"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"submitDraft"`
		}
		gt.NoError(t, json.Unmarshal(submitResp.Data, &submitResult)).Required()
		gt.Value(t, int64(submitResult.SubmitDraft.ID)).Equal(draft.ID)
		gt.Value(t, submitResult.SubmitDraft.Status).Equal("OPEN")

		// After submit, drafts query returns empty for the same reporter.
		rec2 := executeGraphQLRequestWithAuth(t, handler, draftsQuery, map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}, reporterID)
		resp2 := parseGraphQLResponse(t, rec2)
		var listAfter struct {
			Drafts []struct {
				ID int `json:"id"`
			} `json:"drafts"`
		}
		gt.NoError(t, json.Unmarshal(resp2.Data, &listAfter)).Required()
		gt.Array(t, listAfter.Drafts).Length(0)

		// Promoted case now appears in the regular cases listing.
		casesRec := executeGraphQLRequestWithAuth(t, handler, `
			query($workspaceId: String!) {
				cases(workspaceId: $workspaceId) {
					id
					status
				}
			}`, map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}, reporterID)
		casesResp := parseGraphQLResponse(t, casesRec)
		gt.Array(t, casesResp.Errors).Length(0)
		var casesResult struct {
			Cases []struct {
				ID     int    `json:"id"`
				Status string `json:"status"`
			} `json:"cases"`
		}
		gt.NoError(t, json.Unmarshal(casesResp.Data, &casesResult)).Required()
		gt.Array(t, casesResult.Cases).Length(1).Required()
		gt.Value(t, int64(casesResult.Cases[0].ID)).Equal(draft.ID)
		gt.Value(t, casesResult.Cases[0].Status).Equal("OPEN")
	})

	t.Run("discardDraft deletes a private draft for the reporter only", func(t *testing.T) {
		// Public drafts are workspace-shared so any teammate can act on
		// them; the access-control story only kicks in for private drafts.
		// We exercise the strict path here.
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		ctx := context.Background()
		draft, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:      "To discard",
			Status:     types.CaseStatusDraft,
			ReporterID: reporterID,
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			IsPrivate:  true,
		})
		gt.NoError(t, err).Required()

		discardMutation := `
			mutation($workspaceId: String!, $id: Int!) {
				discardDraft(workspaceId: $workspaceId, id: $id)
			}
		`
		// Stranger cannot discard a private draft (looks like "not found").
		strangerRec := executeGraphQLRequestWithAuth(t, handler, discardMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          int(draft.ID),
		}, strangerID)
		strangerResp := parseGraphQLResponse(t, strangerRec)
		gt.Number(t, len(strangerResp.Errors)).GreaterOrEqual(1)

		// Reporter can.
		rec := executeGraphQLRequestWithAuth(t, handler, discardMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          int(draft.ID),
		}, reporterID)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		var discardResult struct {
			DiscardDraft bool `json:"discardDraft"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &discardResult)).Required()
		gt.Bool(t, discardResult.DiscardDraft).True()

		// Draft is gone.
		_, getErr := repo.Case().Get(ctx, testWorkspaceID, draft.ID)
		gt.Error(t, getErr)
	})

	t.Run("discardDraft on a public draft works for any teammate", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		ctx := context.Background()
		draft, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:      "Shared cleanup",
			Status:     types.CaseStatusDraft,
			ReporterID: reporterID,
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
		})
		gt.NoError(t, err).Required()

		discardMutation := `
			mutation($workspaceId: String!, $id: Int!) {
				discardDraft(workspaceId: $workspaceId, id: $id)
			}
		`
		rec := executeGraphQLRequestWithAuth(t, handler, discardMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          int(draft.ID),
		}, strangerID)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)
		var discardResult struct {
			DiscardDraft bool `json:"discardDraft"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &discardResult)).Required()
		gt.Bool(t, discardResult.DiscardDraft).True()

		_, getErr := repo.Case().Get(ctx, testWorkspaceID, draft.ID)
		gt.Error(t, getErr)
	})

	t.Run("submitDraft on an OPEN case errors", func(t *testing.T) {
		repo := memory.New()
		handler, err := setupGraphQLServer(repo)
		gt.NoError(t, err).Required()

		ctx := context.Background()
		open, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:      "Already open",
			Status:     types.CaseStatusOpen,
			ReporterID: reporterID,
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
		})
		gt.NoError(t, err).Required()

		submitMutation := `
			mutation($workspaceId: String!, $id: Int!) {
				submitDraft(workspaceId: $workspaceId, id: $id) {
					id
				}
			}
		`
		rec := executeGraphQLRequestWithAuth(t, handler, submitMutation, map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"id":          int(open.ID),
		}, reporterID)
		resp := parseGraphQLResponse(t, rec)
		gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
	})
}

// setupGraphQLServerWithAuth mirrors setupGraphQLServer but wires the
// real authMiddleware in front of the GraphQL endpoint so tests can
// exercise the full HTTP-to-resolver auth-context plumbing (cookie /
// no-auth shortcut → ContextWithToken → resolver's TokenFromContext).
// Tests that pre-inject auth via executeGraphQLRequestWithAuth bypass
// this path and therefore cannot catch regressions in the middleware
// chain itself.
func setupGraphQLServerWithAuth(repo interfaces.Repository, authUC usecase.AuthUseCaseInterface) (http.Handler, error) {
	uc := usecase.New(repo, nil)
	resolver := gqlctrl.NewResolver(repo, uc)
	srv := handler.NewDefaultServer(
		gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: resolver}),
	)
	gqlHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loaders := gqlctrl.NewDataLoaders(repo, nil, nil)
		ctx := gqlctrl.WithDataLoaders(r.Context(), loaders)
		srv.ServeHTTP(w, r.WithContext(ctx))
	})
	return httpctrl.New(gqlHandler, httpctrl.WithAuth(authUC))
}

// TestGraphQLHandler_CreateDraftReporterFromNoAuthnMiddleware pins the
// reporter-recording flow when the application runs in no-auth mode —
// i.e. the developer-mode shortcut where every request resolves to a
// pre-configured user via NoAuthnUseCase.ValidateToken. The Drafts
// page's "Reporter" column comes out empty if any one of the following
// silently fails:
//
//   - authMiddleware does not invoke ValidateToken in the no-auth shortcut
//   - NoAuthnUseCase.ValidateToken is constructed with an empty Sub
//   - persistCase does not read Sub off the auth-context Token
//   - the GraphQL converter strips ReporterID
//   - the drafts query resolver does not return ReporterID
//
// Going through the real HTTP entry point (no executeGraphQLRequestWithAuth
// shortcut) is what makes this test catch all of the above together.
func TestGraphQLHandler_CreateDraftReporterFromNoAuthnMiddleware(t *testing.T) {
	const reporterID = "U-NOAUTHN-SUB"

	repo := memory.New()
	authUC := usecase.NewNoAuthnUseCase(repo, reporterID, "noauth@example.com", "No Auth User")
	handler, err := setupGraphQLServerWithAuth(repo, authUC)
	gt.NoError(t, err).Required()

	createMutation := `
		mutation($workspaceId: String!, $input: CreateDraftInput!) {
			createDraft(workspaceId: $workspaceId, input: $input) {
				id
				reporterID
			}
		}
	`
	body, err := json.Marshal(graphQLRequest{
		Query: createMutation,
		Variables: map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"title":       "Reporter via no-auth middleware",
				"description": "draft body",
			},
		},
	})
	gt.NoError(t, err).Required()

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	gt.Value(t, rec.Code).Equal(http.StatusOK).Required()

	resp := parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0).Required()

	var created struct {
		CreateDraft struct {
			ID         int     `json:"id"`
			ReporterID *string `json:"reporterID"`
		} `json:"createDraft"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &created)).Required()
	gt.Value(t, created.CreateDraft.ReporterID).NotNil().Required()
	gt.Value(t, *created.CreateDraft.ReporterID).Equal(reporterID)

	persisted, err := repo.Case().Get(context.Background(), testWorkspaceID, int64(created.CreateDraft.ID))
	gt.NoError(t, err).Required()
	gt.Value(t, persisted.ReporterID).Equal(reporterID)
}

// TestGraphQLHandler_CreateDraftRecordsReporter pins the end-to-end
// reporter-recording contract for the createDraft mutation. The reporter
// is what populates the "Reporter" column on the Drafts page, and the
// only way it gets there is via the auth-context Token that the HTTP
// auth middleware injects on its way through the GraphQL request. This
// test goes through the public HTTP entry point exactly the way a
// browser does so a regression in any layer along the way — middleware,
// resolver, usecase, repository, GraphQL converter, drafts query
// resolver — is caught here instead of silently shipping a Drafts page
// with empty Reporter cells.
func TestGraphQLHandler_CreateDraftRecordsReporter(t *testing.T) {
	const reporterID = "U-CREATEDRAFT-REPORTER"

	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	createMutation := `
		mutation($workspaceId: String!, $input: CreateDraftInput!) {
			createDraft(workspaceId: $workspaceId, input: $input) {
				id
				reporterID
			}
		}
	`
	rec := executeGraphQLRequestWithAuth(t, handler, createMutation, map[string]interface{}{
		"workspaceId": testWorkspaceID,
		"input": map[string]interface{}{
			"title":       "Reporter capture test",
			"description": "draft body",
		},
	}, reporterID)
	resp := parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0).Required()

	var created struct {
		CreateDraft struct {
			ID         int     `json:"id"`
			ReporterID *string `json:"reporterID"`
		} `json:"createDraft"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &created)).Required()
	gt.Value(t, created.CreateDraft.ReporterID).NotNil().Required()
	gt.Value(t, *created.CreateDraft.ReporterID).Equal(reporterID)

	// Persistence is part of the contract: read the draft straight back
	// out of the repository and assert the ReporterID actually landed in
	// storage (not merely echoed in the mutation response).
	persisted, err := repo.Case().Get(context.Background(), testWorkspaceID, int64(created.CreateDraft.ID))
	gt.NoError(t, err).Required()
	gt.Value(t, persisted.ReporterID).Equal(reporterID)

	// And the drafts query — the path the UI actually uses — must
	// surface the same reporterID back to the client. This is what
	// the Drafts page reads to render the Reporter column.
	listQuery := `
		query($workspaceId: String!) {
			drafts(workspaceId: $workspaceId) {
				id
				reporterID
			}
		}
	`
	listRec := executeGraphQLRequestWithAuth(t, handler, listQuery, map[string]interface{}{
		"workspaceId": testWorkspaceID,
	}, reporterID)
	listResp := parseGraphQLResponse(t, listRec)
	gt.Array(t, listResp.Errors).Length(0).Required()

	var listResult struct {
		Drafts []struct {
			ID         int     `json:"id"`
			ReporterID *string `json:"reporterID"`
		} `json:"drafts"`
	}
	gt.NoError(t, json.Unmarshal(listResp.Data, &listResult)).Required()
	gt.Array(t, listResult.Drafts).Length(1).Required()
	gt.Value(t, listResult.Drafts[0].ReporterID).NotNil().Required()
	gt.Value(t, *listResult.Drafts[0].ReporterID).Equal(reporterID)
}

// TestGraphQLHandler_DraftsReporterResolvesToSlackUser pins the exact
// shape the Drafts page actually queries — `reporter { id name realName
// imageUrl }` — and asserts that the SlackUser dataloader resolves the
// nested object end-to-end. Earlier tests only verified the raw
// `reporterID` field, which would still come back even if the
// SlackUserLoader silently returned nil; that was exactly the
// regression mode behind the empty-Reporter-column bug. By driving the
// same field set the UI uses, plus the normalisation path for legacy
// "Uxxx-Txxx" composite IDs, this test fails the way the production UI
// fails instead of silently passing.
func TestGraphQLHandler_DraftsReporterResolvesToSlackUser(t *testing.T) {
	const (
		bareReporterID      = "UBAREDRAFT"
		compositeReporterID = "ULEGACYDRAFT-TWORKSPACE"
	)

	repo := memory.New()
	ctx := context.Background()

	// The SlackUser repository keys on the bare user ID. Both the
	// modern (bare) and legacy (composite) reporters should resolve to
	// the same entry once the dataloader normalises the composite form.
	gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
		{ID: "UBAREDRAFT", Name: "alice", RealName: "Alice"},
		{ID: "ULEGACYDRAFT", Name: "bob", RealName: "Bob"},
	})).Required()

	// Two drafts: one persisted with the bare reporter ID (the modern
	// flow), one with the composite OIDC sub form (data persisted
	// before the auth-side normalisation fix).
	bareDraft, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
		Title:      "Bare reporter draft",
		Status:     types.CaseStatusDraft,
		ReporterID: bareReporterID,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	})
	gt.NoError(t, err).Required()
	legacyDraft, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
		Title:      "Legacy composite reporter draft",
		Status:     types.CaseStatusDraft,
		ReporterID: compositeReporterID,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	})
	gt.NoError(t, err).Required()

	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	// Same field set as frontend/src/graphql/drafts.ts so a regression in
	// the SlackUserLoader, dataloader plumbing, or reporter resolver
	// surfaces here exactly as it would in the browser.
	query := `
		query($workspaceId: String!) {
			drafts(workspaceId: $workspaceId) {
				id
				reporterID
				reporter {
					id
					name
					realName
					imageUrl
				}
			}
		}
	`
	rec := executeGraphQLRequestWithAuth(t, handler, query, map[string]interface{}{
		"workspaceId": testWorkspaceID,
	}, bareReporterID)
	resp := parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0).Required()

	type slackUserPayload struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		RealName string  `json:"realName"`
		ImageURL *string `json:"imageUrl"`
	}
	var result struct {
		Drafts []struct {
			ID         int               `json:"id"`
			ReporterID *string           `json:"reporterID"`
			Reporter   *slackUserPayload `json:"reporter"`
		} `json:"drafts"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
	gt.Array(t, result.Drafts).Length(2).Required()

	byID := make(map[int64]struct {
		ReporterID *string
		Reporter   *slackUserPayload
	}, len(result.Drafts))
	for _, d := range result.Drafts {
		byID[int64(d.ID)] = struct {
			ReporterID *string
			Reporter   *slackUserPayload
		}{ReporterID: d.ReporterID, Reporter: d.Reporter}
	}

	bare, ok := byID[bareDraft.ID]
	gt.Bool(t, ok).True().Required()
	gt.Value(t, bare.ReporterID).NotNil().Required()
	gt.Value(t, *bare.ReporterID).Equal(bareReporterID)
	gt.Value(t, bare.Reporter).NotNil().Required()
	gt.Value(t, bare.Reporter.ID).Equal("UBAREDRAFT")
	gt.Value(t, bare.Reporter.RealName).Equal("Alice")

	legacy, ok := byID[legacyDraft.ID]
	gt.Bool(t, ok).True().Required()
	gt.Value(t, legacy.ReporterID).NotNil().Required()
	// The reporterID echoes the raw persisted value (composite form)
	// — the converter does not rewrite stored data.
	gt.Value(t, *legacy.ReporterID).Equal(compositeReporterID)
	// But the nested reporter object MUST still resolve via the
	// dataloader's normalisation path. If this assertion fails the
	// Drafts page shows an empty Reporter cell for every legacy draft.
	gt.Value(t, legacy.Reporter).NotNil().Required()
	gt.Value(t, legacy.Reporter.ID).Equal("ULEGACYDRAFT")
	gt.Value(t, legacy.Reporter.RealName).Equal("Bob")
}

// TestGraphQLHandler_DraftsReporterMissingResolvesToNull pins the
// display-first contract: when the ReporterID is recorded but the
// SlackUser repository has no entry for it (an unsynced thread-mode
// poster, a stale workspace, a deleted account), the GraphQL response
// must NOT carry a field-level error. A single missing reporter must
// not fail the whole list query and blank the page out (Sentry
// ARGUS-7S). The reporter field resolves to null while reporterID is
// still echoed, and ops visibility is preserved out-of-band by the
// SlackUser dataloader's errutil.Handle report (logs + Sentry).
func TestGraphQLHandler_DraftsReporterMissingResolvesToNull(t *testing.T) {
	const reporterID = "UORPHANREPORTER"

	repo := memory.New()
	ctx := context.Background()

	// Persist a draft whose ReporterID has NO corresponding SlackUser
	// entry — the exact production failure mode (repo never synced or
	// stale workspace).
	_, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
		Title:      "Reporter is orphaned",
		Status:     types.CaseStatusDraft,
		ReporterID: reporterID,
	})
	gt.NoError(t, err).Required()

	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	query := `
		query($workspaceId: String!) {
			drafts(workspaceId: $workspaceId) {
				id
				reporterID
				reporter {
					id
					realName
				}
			}
		}
	`
	rec := executeGraphQLRequestWithAuth(t, handler, query, map[string]interface{}{
		"workspaceId": testWorkspaceID,
	}, reporterID)
	resp := parseGraphQLResponse(t, rec)

	// No field-level error for the reporter path: the query succeeds so
	// the Drafts page renders, even though one reporter could not be
	// resolved.
	for _, e := range resp.Errors {
		for _, p := range e.Path {
			if s, ok := p.(string); ok && s == "reporter" {
				t.Fatalf("unexpected reporter-path GraphQL error: %s", e.Message)
			}
		}
	}

	// reporterID is still echoed back since the converter does not
	// touch storage; reporter resolves to null because the SlackUser
	// is absent from the repository.
	var result struct {
		Drafts []struct {
			ID         int     `json:"id"`
			ReporterID *string `json:"reporterID"`
			Reporter   *struct {
				ID       string `json:"id"`
				RealName string `json:"realName"`
			} `json:"reporter"`
		} `json:"drafts"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
	gt.Array(t, result.Drafts).Length(1).Required()
	gt.Value(t, result.Drafts[0].ReporterID).NotNil().Required()
	gt.Value(t, *result.Drafts[0].ReporterID).Equal(reporterID)
	gt.Value(t, result.Drafts[0].Reporter).Nil()
}

func TestGraphQLHandler_UpdateCaseAgentSettingsMutation(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()
	c, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
		Title:      "agent target",
		ReporterID: "U-REPORTER",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	})
	gt.NoError(t, err).Required()

	src, err := repo.Source().Create(ctx, testWorkspaceID, &model.Source{
		ID: model.NewSourceID(), Name: "Datadog", SourceType: model.SourceTypeSlack, Enabled: true,
		SlackConfig: &model.SlackConfig{},
	})
	gt.NoError(t, err).Required()

	mutation := `
		mutation($workspaceId: String!, $input: UpdateCaseAgentSettingsInput!) {
			updateCaseAgentSettings(workspaceId: $workspaceId, input: $input) {
				id
				agentAdditionalPrompt
				agentSources { id name }
			}
		}
	`
	vars := map[string]any{
		"workspaceId": testWorkspaceID,
		"input": map[string]any{
			"caseId":                c.ID,
			"agentAdditionalPrompt": "### per-case\n- focus on prod",
			"enabledSourceIds":      []string{string(src.ID)},
		},
	}
	rec := executeGraphQLRequestWithAuth(t, handler, mutation, vars, "U-REPORTER")
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp := parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0)

	var out struct {
		UpdateCaseAgentSettings struct {
			ID                    int    `json:"id"`
			AgentAdditionalPrompt string `json:"agentAdditionalPrompt"`
			AgentSources          []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"agentSources"`
		} `json:"updateCaseAgentSettings"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &out)).Required()
	gt.Value(t, out.UpdateCaseAgentSettings.AgentAdditionalPrompt).Equal("### per-case\n- focus on prod")
	gt.Array(t, out.UpdateCaseAgentSettings.AgentSources).Length(1).Required()
	gt.Value(t, out.UpdateCaseAgentSettings.AgentSources[0].ID).Equal(string(src.ID))
	gt.Value(t, out.UpdateCaseAgentSettings.AgentSources[0].Name).Equal("Datadog")

	persisted, err := repo.Case().Get(ctx, testWorkspaceID, c.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, persisted.AgentAdditionalPrompt).Equal("### per-case\n- focus on prod")
	gt.Array(t, persisted.AgentSourceIDs).Length(1).Required()
	gt.Value(t, persisted.AgentSourceIDs[0]).Equal(src.ID)
}

func TestGraphQLHandler_CaseJobRunLogsQuery(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	ctx := context.Background()
	c, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
		Title:      "agent target",
		ReporterID: "U-REPORTER",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	})
	gt.NoError(t, err).Required()

	base := time.Date(2026, 5, 24, 9, 0, 0, 0, time.UTC)
	for i, runID := range []string{"run-a", "run-b", "run-c"} {
		started := base.Add(time.Duration(i) * time.Minute)
		log := &model.JobRunLog{
			WorkspaceID:     testWorkspaceID,
			CaseID:          c.ID,
			JobID:           "incident-rca",
			RunID:           runID,
			TraceID:         "trace-" + runID,
			Stage:           model.JobRunStageRunning,
			StartedAt:       started,
			ExecutorKind:    "single_loop",
			ExecutorVersion: "test",
		}
		gt.NoError(t, repo.JobRunLog().Create(ctx, log)).Required()
		log.Stage = model.JobRunStageSuccess
		log.EndedAt = started.Add(5 * time.Second)
		gt.NoError(t, repo.JobRunLog().Finish(ctx, log)).Required()

		gt.NoError(t, repo.JobRun().RecordRun(
			ctx,
			model.JobRunKey{WorkspaceID: testWorkspaceID, CaseID: c.ID, JobID: "incident-rca"},
			model.JobRunStatusSuccess,
			started, runID, "trace-"+runID, "",
		)).Required()
	}

	query := `
		query($wsid: String!, $cid: Int!, $first: Int) {
			caseJobRunLogs(workspaceId: $wsid, caseId: $cid, first: $first) {
				items { runId stage startedAt durationMs }
				nextCursor
			}
		}
	`
	vars := map[string]any{
		"wsid":  testWorkspaceID,
		"cid":   c.ID,
		"first": 2,
	}
	rec := executeGraphQLRequestWithAuth(t, handler, query, vars, "U-REPORTER")
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp := parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0)

	var out struct {
		CaseJobRunLogs struct {
			Items []struct {
				RunID      string `json:"runId"`
				Stage      string `json:"stage"`
				StartedAt  string `json:"startedAt"`
				DurationMs *int   `json:"durationMs"`
			} `json:"items"`
			NextCursor *string `json:"nextCursor"`
		} `json:"caseJobRunLogs"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &out)).Required()
	gt.Array(t, out.CaseJobRunLogs.Items).Length(2).Required()
	gt.Value(t, out.CaseJobRunLogs.Items[0].RunID).Equal("run-c")
	gt.Value(t, out.CaseJobRunLogs.Items[1].RunID).Equal("run-b")
	gt.Value(t, out.CaseJobRunLogs.NextCursor).NotNil().Required()
	gt.Value(t, out.CaseJobRunLogs.Items[0].DurationMs).NotNil().Required()
	gt.Number(t, *out.CaseJobRunLogs.Items[0].DurationMs).Equal(5000)
}

// TestGraphQLHandler_CaseJobsQuery drives the caseJobs query through the
// HTTP boundary against a Workspace registry holding Job definitions. It
// asserts the enabled/relevant filter, the trigger shape per event domain,
// and that private-case access control refuses a non-member.
func TestGraphQLHandler_CaseJobsQuery(t *testing.T) {
	const caseJobsQuery = `
		query($wsid: String!, $cid: Int!) {
			caseJobs(workspaceId: $wsid, caseId: $cid) {
				id name description strategy quiet prompt
				trigger {
					caseEvents
					schedule { everySeconds cron }
				}
			}
		}
	`

	type caseJobRow struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Strategy    string `json:"strategy"`
		Quiet       bool   `json:"quiet"`
		Prompt      string `json:"prompt"`
		Trigger     struct {
			CaseEvents []string `json:"caseEvents"`
			Schedule   *struct {
				EverySeconds *int    `json:"everySeconds"`
				Cron         *string `json:"cron"`
			} `json:"schedule"`
		} `json:"trigger"`
	}

	buildRegistryHandler := func(t *testing.T, repo interfaces.Repository) http.Handler {
		t.Helper()
		cronSched, err := cron.ParseStandard("0 9 * * *")
		gt.NoError(t, err).Required()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			Jobs: []*model.Job{
				{
					ID: "triage", Name: "Initial triage", Description: "evaluate on create",
					Prompt: "triage prompt", Strategy: model.JobStrategyPlanexec,
					Events: model.JobEvents{Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}}},
				},
				{
					ID: "stale", Name: "Stale check", Description: "remind", Prompt: "stale prompt", Quiet: true,
					Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
				},
				{
					ID: "daily", Name: "Daily summary", Description: "report", Prompt: "daily prompt",
					Strategy: model.JobStrategyPlanexec,
					Events:   model.JobEvents{Scheduled: &model.ScheduledEventConfig{Cron: cronSched, CronExpr: "0 9 * * *"}},
				},
				{
					ID: "disabled-job", Name: "Disabled", Description: "never", Prompt: "p", Disabled: true,
					Events: model.JobEvents{Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}}},
				},
			},
		})
		uc := usecase.New(repo, registry)
		resolver := gqlctrl.NewResolver(repo, uc)
		srv := handler.NewDefaultServer(
			gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: resolver}),
		)
		gqlHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			loaders := gqlctrl.NewDataLoaders(repo, nil, nil)
			ctx := gqlctrl.WithDataLoaders(r.Context(), loaders)
			srv.ServeHTTP(w, r.WithContext(ctx))
		})
		registryHandler, err := httpctrl.New(gqlHandler)
		gt.NoError(t, err).Required()
		return registryHandler
	}

	t.Run("open case returns enabled jobs with trigger detail", func(t *testing.T) {
		repo := memory.New()
		h := buildRegistryHandler(t, repo)
		c, err := repo.Case().Create(context.Background(), testWorkspaceID, &model.Case{
			Title:      "agent target",
			ReporterID: "U-REPORTER",
			Status:     types.CaseStatusOpen,
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
		})
		gt.NoError(t, err).Required()

		rec := executeGraphQLRequestWithAuth(t, h, caseJobsQuery, map[string]any{
			"wsid": testWorkspaceID, "cid": c.ID,
		}, "U-REPORTER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var out struct {
			CaseJobs []caseJobRow `json:"caseJobs"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &out)).Required()
		gt.Array(t, out.CaseJobs).Length(3).Required()

		byID := make(map[string]caseJobRow, len(out.CaseJobs))
		for _, j := range out.CaseJobs {
			byID[j.ID] = j
		}
		_, hasDisabled := byID["disabled-job"]
		gt.Bool(t, hasDisabled).False()

		triage := byID["triage"]
		gt.String(t, triage.Strategy).Equal("PLANEXEC")
		gt.String(t, triage.Prompt).Equal("triage prompt")
		gt.Array(t, triage.Trigger.CaseEvents).Length(1).Required()
		gt.String(t, triage.Trigger.CaseEvents[0]).Equal("CREATED")
		gt.Value(t, triage.Trigger.Schedule).Nil()

		stale := byID["stale"]
		gt.Bool(t, stale.Quiet).True()
		gt.String(t, stale.Strategy).Equal("SIMPLE")
		gt.Array(t, stale.Trigger.CaseEvents).Length(0)
		gt.Value(t, stale.Trigger.Schedule).NotNil().Required()
		gt.Value(t, stale.Trigger.Schedule.EverySeconds).NotNil().Required()
		gt.Number(t, *stale.Trigger.Schedule.EverySeconds).Equal(3600)
		gt.Value(t, stale.Trigger.Schedule.Cron).Nil()

		daily := byID["daily"]
		gt.Value(t, daily.Trigger.Schedule).NotNil().Required()
		gt.Value(t, daily.Trigger.Schedule.Cron).NotNil().Required()
		gt.String(t, *daily.Trigger.Schedule.Cron).Equal("0 9 * * *")
		gt.Value(t, daily.Trigger.Schedule.EverySeconds).Nil()
	})

	t.Run("private case refuses a non-member", func(t *testing.T) {
		repo := memory.New()
		h := buildRegistryHandler(t, repo)
		c, err := repo.Case().Create(context.Background(), testWorkspaceID, &model.Case{
			Title:          "private agent target",
			ReporterID:     "U-REPORTER",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: []string{"U-REPORTER"},
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
		})
		gt.NoError(t, err).Required()

		rec := executeGraphQLRequestWithAuth(t, h, caseJobsQuery, map[string]any{
			"wsid": testWorkspaceID, "cid": c.ID,
		}, "U-STRANGER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(1).Required()
	})
}

// manualRunExecutor stands in for the LLM-backed Job executor so the
// trigger test can drive a real JobRunner without an LLM. It records the
// JobIDs it was asked to run.
type manualRunExecutor struct {
	mu     sync.Mutex
	jobIDs []string
}

func (e *manualRunExecutor) Execute(_ context.Context, req jobagent.ExecuteRequest) (*jobagent.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.jobIDs = append(e.jobIDs, req.JobID)
	return &jobagent.ExecuteResult{Status: jobagent.ExecuteStatusSuccess}, nil
}

func (e *manualRunExecutor) ran() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.jobIDs...)
}

// TestGraphQLHandler_TriggerCaseJobMutation drives the manual Job trigger
// through the HTTP boundary with a real JobRunner behind it: a successful
// call must actually produce a JobRunLog tagged with the manual
// provenance, and each refusal must carry the documented extensions.code.
func TestGraphQLHandler_TriggerCaseJobMutation(t *testing.T) {
	const triggerMutation = `
		mutation($wsid: String!, $cid: Int!, $jid: String!) {
			triggerCaseJob(workspaceId: $wsid, caseId: $cid, jobId: $jid)
		}
	`

	buildTriggerHandler := func(t *testing.T, repo interfaces.Repository) (http.Handler, *manualRunExecutor) {
		t.Helper()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			Jobs: []*model.Job{
				{
					ID: "stale", Name: "Stale check", Description: "remind", Prompt: "stale prompt",
					Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
				},
				{
					ID: "disabled-job", Name: "Disabled", Description: "never", Prompt: "p", Disabled: true,
					Events: model.JobEvents{Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}}},
				},
			},
		})
		uc := usecase.New(repo, registry)

		exec := &manualRunExecutor{}
		runner := jobruntime.NewJobRunner(jobruntime.RunnerDeps{
			Repo:      repo,
			Registry:  registry,
			Executors: map[model.JobStrategy]jobagent.JobExecutor{model.JobStrategySimple: exec},
		})
		uc.JobRun.SetTrigger(runner)

		resolver := gqlctrl.NewResolver(repo, uc)
		srv := handler.NewDefaultServer(
			gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: resolver}),
		)
		// Mirror serve.go so extensions.code reaches the response body.
		srv.SetErrorPresenter(func(ctx context.Context, err error) *gqlerror.Error {
			gqlErr := graphql.DefaultErrorPresenter(ctx, err)
			if gqlErr.Extensions == nil {
				gqlErr.Extensions = map[string]any{}
			}
			maps.Copy(gqlErr.Extensions, gqlctrl.ErrorExtensions(err))
			return gqlErr
		})
		gqlHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			loaders := gqlctrl.NewDataLoaders(repo, nil, nil)
			ctx := gqlctrl.WithDataLoaders(r.Context(), loaders)
			srv.ServeHTTP(w, r.WithContext(ctx))
		})
		h, err := httpctrl.New(gqlHandler)
		gt.NoError(t, err).Required()
		return h, exec
	}

	openCase := func(t *testing.T, repo interfaces.Repository, private bool) *model.Case {
		t.Helper()
		c := &model.Case{
			Title:      "agent target",
			ReporterID: "U-REPORTER",
			Status:     types.CaseStatusOpen,
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
		}
		if private {
			c.IsPrivate = true
			c.ChannelUserIDs = []string{"U-REPORTER"}
		}
		created, err := repo.Case().Create(context.Background(), testWorkspaceID, c)
		gt.NoError(t, err).Required()
		return created
	}

	// errorCode returns the extensions.code of the single expected error.
	errorCode := func(t *testing.T, resp *graphQLResponse) string {
		t.Helper()
		gt.Array(t, resp.Errors).Length(1).Required()
		return fmt.Sprint(resp.Errors[0].Extensions["code"])
	}

	t.Run("runs the job and records a manual run log", func(t *testing.T) {
		repo := memory.New()
		h, exec := buildTriggerHandler(t, repo)
		c := openCase(t, repo, false)

		rec := executeGraphQLRequestWithAuth(t, h, triggerMutation, map[string]any{
			"wsid": testWorkspaceID, "cid": c.ID, "jid": "stale",
		}, "U-REPORTER")
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var out struct {
			TriggerCaseJob bool `json:"triggerCaseJob"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &out)).Required()
		gt.Bool(t, out.TriggerCaseJob).True()

		async.Wait()

		gt.Array(t, exec.ran()).Length(1).Required()
		gt.String(t, exec.ran()[0]).Equal("stale")

		key := model.JobRunKey{WorkspaceID: testWorkspaceID, CaseID: c.ID, JobID: "stale"}
		logs, err := repo.JobRunLog().List(context.Background(), key, 0)
		gt.NoError(t, err).Required()
		gt.Array(t, logs).Length(1).Required()
		gt.Value(t, logs[0].Stage).Equal(model.JobRunStageSuccess)
		gt.String(t, logs[0].EventType).Equal("manual")
		gt.String(t, logs[0].JobID).Equal("stale")
	})

	t.Run("refuses a non-member of a private case with FORBIDDEN", func(t *testing.T) {
		repo := memory.New()
		h, exec := buildTriggerHandler(t, repo)
		c := openCase(t, repo, true)

		rec := executeGraphQLRequestWithAuth(t, h, triggerMutation, map[string]any{
			"wsid": testWorkspaceID, "cid": c.ID, "jid": "stale",
		}, "U-STRANGER")
		gt.String(t, errorCode(t, parseGraphQLResponse(t, rec))).Equal("FORBIDDEN")

		async.Wait()
		gt.Array(t, exec.ran()).Length(0)
	})

	t.Run("refuses an untriggerable job with NOT_FOUND", func(t *testing.T) {
		repo := memory.New()
		h, exec := buildTriggerHandler(t, repo)
		c := openCase(t, repo, false)

		rec := executeGraphQLRequestWithAuth(t, h, triggerMutation, map[string]any{
			"wsid": testWorkspaceID, "cid": c.ID, "jid": "no-such-job",
		}, "U-REPORTER")
		gt.String(t, errorCode(t, parseGraphQLResponse(t, rec))).Equal("NOT_FOUND")

		rec = executeGraphQLRequestWithAuth(t, h, triggerMutation, map[string]any{
			"wsid": testWorkspaceID, "cid": c.ID, "jid": "disabled-job",
		}, "U-REPORTER")
		gt.String(t, errorCode(t, parseGraphQLResponse(t, rec))).Equal("NOT_FOUND")

		async.Wait()
		gt.Array(t, exec.ran()).Length(0)
	})

	t.Run("refuses a run already in flight with CONFLICT", func(t *testing.T) {
		repo := memory.New()
		h, exec := buildTriggerHandler(t, repo)
		c := openCase(t, repo, false)

		key := model.JobRunKey{WorkspaceID: testWorkspaceID, CaseID: c.ID, JobID: "stale"}
		acquired, err := repo.JobRun().TryAcquireLease(context.Background(), key, time.Now().UTC(), 10*time.Minute)
		gt.NoError(t, err).Required()
		gt.Bool(t, acquired).True()

		rec := executeGraphQLRequestWithAuth(t, h, triggerMutation, map[string]any{
			"wsid": testWorkspaceID, "cid": c.ID, "jid": "stale",
		}, "U-REPORTER")
		gt.String(t, errorCode(t, parseGraphQLResponse(t, rec))).Equal("CONFLICT")

		async.Wait()
		gt.Array(t, exec.ran()).Length(0)
	})
}

// TestGraphQLHandler_CaseImportLifecycle drives the import end-to-end
// through the GraphQL HTTP boundary: createCaseImport → executeCaseImport
// → drafts query. The point is to assert that an import-driven Case
// actually lands in the repository as DRAFT and is visible through the
// regular drafts query — the bug class we're guarding against is "the
// resolver returns a session but no Case is ever persisted" or "the
// Case is persisted but without the reporter / wrong status".
func TestGraphQLHandler_CaseImportLifecycle(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	const reporter = "U-IMPORT-CALLER"

	// Pre-seed the Slack users referenced in the YAML so the preview's
	// per-user existence check succeeds. Without this the session would
	// surface "unknown Slack user" errors for every assignee and the
	// lifecycle never reaches APPLIED.
	gt.NoError(t, repo.SlackUser().SaveMany(context.Background(), []*model.SlackUser{
		{ID: model.SlackUserID("U001"), Name: "u001"},
		{ID: model.SlackUserID("U002"), Name: "u002"},
	})).Required()

	yamlContent := `version: 1
cases:
  - title: "Suspicious login"
    description: "Multiple failed attempts."
    isPrivate: false
    assigneeIDs: [U001]
    actions:
      - title: "Block source IP"
        description: "Add firewall rule"
        assigneeID: U002
      - title: "Notify SOC"
  - title: "Failed deployment"
    description: "Canary deploy failed."
    actions:
      - title: "Roll back to v2.2"
`

	// Step 1: createCaseImport → ImportSession is persisted (status=PENDING, valid=true).
	createMutation := `
		mutation($workspaceId: String!, $input: CreateCaseImportInput!) {
			createCaseImport(workspaceId: $workspaceId, input: $input) {
				id
				status
				valid
				createdCount
				failedCount
				skippedCount
				issues { path message severity }
				snapshot {
					cases {
						index
						title
						actions { index title }
					}
				}
			}
		}
	`
	createVars := map[string]any{
		"workspaceId": testWorkspaceID,
		"input": map[string]any{
			"content":          yamlContent,
			"originalFileName": "incidents.yaml",
		},
	}
	rec := executeGraphQLRequestWithAuth(t, handler, createMutation, createVars, reporter)
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp := parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0)

	var createOut struct {
		CreateCaseImport struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			Valid        bool   `json:"valid"`
			CreatedCount int    `json:"createdCount"`
			FailedCount  int    `json:"failedCount"`
			SkippedCount int    `json:"skippedCount"`
			Issues       []struct {
				Path     string `json:"path"`
				Message  string `json:"message"`
				Severity string `json:"severity"`
			} `json:"issues"`
			Snapshot struct {
				Cases []struct {
					Index   int    `json:"index"`
					Title   string `json:"title"`
					Actions []struct {
						Index int    `json:"index"`
						Title string `json:"title"`
					} `json:"actions"`
				} `json:"cases"`
			} `json:"snapshot"`
		} `json:"createCaseImport"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &createOut)).Required()

	gt.String(t, createOut.CreateCaseImport.Status).Equal("PENDING")
	gt.Bool(t, createOut.CreateCaseImport.Valid).True()
	gt.Number(t, createOut.CreateCaseImport.CreatedCount).Equal(0)
	// The YAML carries actions: blocks; Import drops them and emits a
	// WARNING per Case. That keeps Valid=true (warnings do not block
	// execute) but the session-level issues list stays empty.
	gt.Array(t, createOut.CreateCaseImport.Issues).Length(0)
	gt.Array(t, createOut.CreateCaseImport.Snapshot.Cases).Length(2).Required()
	gt.String(t, createOut.CreateCaseImport.Snapshot.Cases[0].Title).Equal("Suspicious login")
	gt.String(t, createOut.CreateCaseImport.Snapshot.Cases[1].Title).Equal("Failed deployment")
	// Actions are NOT imported (DRAFT restriction). The snapshot must
	// carry zero Action rows; the dropped count is conveyed through a
	// per-Case WARNING issue instead.
	gt.Array(t, createOut.CreateCaseImport.Snapshot.Cases[0].Actions).Length(0)
	gt.Array(t, createOut.CreateCaseImport.Snapshot.Cases[1].Actions).Length(0)

	sessionID := createOut.CreateCaseImport.ID
	gt.String(t, sessionID).NotEqual("")

	// Step 2: executeCaseImport → both Cases become CREATED, status=APPLIED.
	execMutation := `
		mutation($workspaceId: String!, $id: ID!) {
			executeCaseImport(workspaceId: $workspaceId, id: $id) {
				id
				status
				createdCount
				failedCount
				skippedCount
				snapshot {
					cases {
						title
						result {
							status
							createdCaseID
						}
						actions {
							title
							result { status createdActionID }
						}
					}
				}
			}
		}
	`
	execVars := map[string]any{
		"workspaceId": testWorkspaceID,
		"id":          sessionID,
	}
	rec = executeGraphQLRequestWithAuth(t, handler, execMutation, execVars, reporter)
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp = parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0)

	var execOut struct {
		ExecuteCaseImport struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			CreatedCount int    `json:"createdCount"`
			FailedCount  int    `json:"failedCount"`
			SkippedCount int    `json:"skippedCount"`
			Snapshot     struct {
				Cases []struct {
					Title  string `json:"title"`
					Result struct {
						Status        string `json:"status"`
						CreatedCaseID *int   `json:"createdCaseID"`
					} `json:"result"`
					Actions []struct {
						Title  string `json:"title"`
						Result struct {
							Status          string `json:"status"`
							CreatedActionID *int   `json:"createdActionID"`
						} `json:"result"`
					} `json:"actions"`
				} `json:"cases"`
			} `json:"snapshot"`
		} `json:"executeCaseImport"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &execOut)).Required()

	gt.String(t, execOut.ExecuteCaseImport.Status).Equal("APPLIED")
	gt.Number(t, execOut.ExecuteCaseImport.CreatedCount).Equal(2)
	gt.Number(t, execOut.ExecuteCaseImport.FailedCount).Equal(0)
	gt.Number(t, execOut.ExecuteCaseImport.SkippedCount).Equal(0)
	gt.Array(t, execOut.ExecuteCaseImport.Snapshot.Cases).Length(2).Required()
	for _, c := range execOut.ExecuteCaseImport.Snapshot.Cases {
		gt.String(t, c.Result.Status).Equal("CREATED")
		gt.Value(t, c.Result.CreatedCaseID).NotNil().Required()
		// Actions are never imported (DRAFT restriction). The snapshot
		// must not surface any Action rows from the YAML.
		gt.Array(t, c.Actions).Length(0)
	}

	// Step 3: drafts query — both imported cases must surface as DRAFT
	// with the correct title and reporter (the auth-context userID).
	draftsQuery := `
		query($wsId: String!) {
			drafts(workspaceId: $wsId) {
				id
				title
				status
				reporterID
				assigneeIDs
			}
		}
	`
	rec = executeGraphQLRequestWithAuth(t, handler, draftsQuery, map[string]any{
		"wsId": testWorkspaceID,
	}, reporter)
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp = parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0)

	var draftsOut struct {
		Drafts []struct {
			ID          int      `json:"id"`
			Title       string   `json:"title"`
			Status      string   `json:"status"`
			ReporterID  *string  `json:"reporterID"`
			AssigneeIDs []string `json:"assigneeIDs"`
		} `json:"drafts"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &draftsOut)).Required()
	gt.Array(t, draftsOut.Drafts).Length(2).Required()

	titles := []string{draftsOut.Drafts[0].Title, draftsOut.Drafts[1].Title}
	gt.Array(t, titles).Has("Suspicious login")
	gt.Array(t, titles).Has("Failed deployment")
	for _, d := range draftsOut.Drafts {
		gt.String(t, d.Status).Equal(string(types.CaseStatusDraft))
		gt.Value(t, d.ReporterID).NotNil().Required()
		gt.String(t, *d.ReporterID).Equal(reporter)
	}

	// Step 4: persistence sanity check — re-read the cases directly
	// from the repository (bypassing GraphQL) and confirm Import did
	// NOT create any Actions. Actions cannot be edited on DRAFT cases,
	// so the import path drops them on purpose.
	ctx := context.Background()
	for _, d := range draftsOut.Drafts {
		actions, aerr := repo.Action().GetByCase(ctx, testWorkspaceID, int64(d.ID), interfaces.ActionListOptions{})
		gt.NoError(t, aerr).Required()
		gt.Array(t, actions).Length(0)
	}

	// Step 5: a second executeCaseImport call on the same session must
	// be rejected because the session has already transitioned to APPLIED.
	rec = executeGraphQLRequestWithAuth(t, handler, execMutation, execVars, reporter)
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp = parseGraphQLResponse(t, rec)
	gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)

	// Step 6: a different user must not be able to see the same import
	// session through caseImport (session is creator-scoped).
	getImportQuery := `
		query($wsId: String!, $id: ID!) {
			caseImport(workspaceId: $wsId, id: $id) { id status }
		}
	`
	rec = executeGraphQLRequestWithAuth(t, handler, getImportQuery, map[string]any{
		"wsId": testWorkspaceID,
		"id":   sessionID,
	}, "U-OTHER")
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp = parseGraphQLResponse(t, rec)
	gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)
}

// TestGraphQLHandler_CaseImportPreviewWithErrors verifies the preview
// path: a YAML that fails structural validation surfaces the issues to
// the client, the session is persisted in PENDING/valid=false state,
// and executeCaseImport on it is rejected (so DRAFT cases are never
// created from an invalid import).
func TestGraphQLHandler_CaseImportPreviewWithErrors(t *testing.T) {
	repo := memory.New()
	handler, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	const reporter = "U-IMPORT-CALLER"

	yamlContent := `version: 1
cases:
  - title: ""
    description: "missing title"
    actions:
      - title: "should not run"
  - title: "OK case"
    actions:
      - title: ""
`

	createMutation := `
		mutation($workspaceId: String!, $input: CreateCaseImportInput!) {
			createCaseImport(workspaceId: $workspaceId, input: $input) {
				id valid
				snapshot {
					cases {
						title
						issues { path message severity }
						actions { title issues { path message severity } }
					}
				}
			}
		}
	`
	rec := executeGraphQLRequestWithAuth(t, handler, createMutation, map[string]any{
		"workspaceId": testWorkspaceID,
		"input": map[string]any{
			"content":          yamlContent,
			"originalFileName": "bad.yaml",
		},
	}, reporter)
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp := parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0)

	var createOut struct {
		CreateCaseImport struct {
			ID       string `json:"id"`
			Valid    bool   `json:"valid"`
			Snapshot struct {
				Cases []struct {
					Title  string `json:"title"`
					Issues []struct {
						Path     string `json:"path"`
						Severity string `json:"severity"`
					} `json:"issues"`
					Actions []struct {
						Title  string `json:"title"`
						Issues []struct {
							Path     string `json:"path"`
							Severity string `json:"severity"`
						} `json:"issues"`
					} `json:"actions"`
				} `json:"cases"`
			} `json:"snapshot"`
		} `json:"createCaseImport"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &createOut)).Required()

	gt.Bool(t, createOut.CreateCaseImport.Valid).False()
	gt.Array(t, createOut.CreateCaseImport.Snapshot.Cases).Length(2).Required()
	// Case 0 has a missing-title ERROR on the case itself (this is
	// what makes the whole session invalid).
	c0 := createOut.CreateCaseImport.Snapshot.Cases[0]
	c0HasTitleError := false
	for _, i := range c0.Issues {
		if i.Severity == "ERROR" && strings.Contains(i.Path, "cases[0].title") {
			c0HasTitleError = true
		}
	}
	gt.Bool(t, c0HasTitleError).True()
	// Case 1 has a non-empty `actions:` block in the YAML; Import
	// drops those actions but surfaces a WARNING so the user knows
	// they were skipped. Snapshot.Actions must be empty (DRAFT
	// restriction — Import never creates Actions).
	c1 := createOut.CreateCaseImport.Snapshot.Cases[1]
	gt.Array(t, c1.Actions).Length(0)
	c1HasActionWarning := false
	for _, i := range c1.Issues {
		if i.Severity == "WARNING" && strings.Contains(i.Path, "cases[1].actions") {
			c1HasActionWarning = true
		}
	}
	gt.Bool(t, c1HasActionWarning).True()

	// Execute must refuse and no DRAFT cases should appear.
	execMutation := `
		mutation($workspaceId: String!, $id: ID!) {
			executeCaseImport(workspaceId: $workspaceId, id: $id) { id status }
		}
	`
	rec = executeGraphQLRequestWithAuth(t, handler, execMutation, map[string]any{
		"workspaceId": testWorkspaceID,
		"id":          createOut.CreateCaseImport.ID,
	}, reporter)
	gt.Value(t, rec.Code).Equal(http.StatusOK)
	resp = parseGraphQLResponse(t, rec)
	gt.Number(t, len(resp.Errors)).GreaterOrEqual(1)

	draftsQuery := `query($wsId: String!) { drafts(workspaceId: $wsId) { id } }`
	rec = executeGraphQLRequestWithAuth(t, handler, draftsQuery, map[string]any{
		"wsId": testWorkspaceID,
	}, reporter)
	resp = parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0)
	var draftsOut struct {
		Drafts []struct {
			ID int `json:"id"`
		} `json:"drafts"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &draftsOut)).Required()
	gt.Array(t, draftsOut.Drafts).Length(0)
}

type tagPayload struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type knowledgePayload struct {
	ID    string       `json:"id"`
	Title string       `json:"title"`
	Claim string       `json:"claim"`
	Tags  []tagPayload `json:"tags"`
}

// createTagForTest creates a tag via the GraphQL mutation and returns its id.
func createTagForTest(t *testing.T, h http.Handler, ws, name string) string {
	t.Helper()
	rec := executeGraphQLRequest(t, h,
		`mutation($ws: String!, $name: String) { createTag(workspaceId: $ws, name: $name) { id name } }`,
		map[string]interface{}{"ws": ws, "name": name})
	resp := parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0).Required()
	var out struct {
		CreateTag tagPayload `json:"createTag"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &out)).Required()
	gt.String(t, out.CreateTag.ID).NotEqual("").Required()
	gt.String(t, out.CreateTag.Name).Equal(name)
	return out.CreateTag.ID
}

func TestGraphQLHandler_KnowledgeLifecycle(t *testing.T) {
	repo := memory.New()
	h, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	// Tags must be created first; knowledge references them by id.
	opsID := createTagForTest(t, h, testWorkspaceID, "ops")
	githubID := createTagForTest(t, h, testWorkspaceID, "github")

	// Create
	createQuery := `mutation($ws: String!, $input: CreateKnowledgeInput!) {
		createKnowledge(workspaceId: $ws, input: $input) { id title claim tags { id name } }
	}`
	rec := executeGraphQLRequest(t, h, createQuery, map[string]interface{}{
		"ws": testWorkspaceID,
		"input": map[string]interface{}{
			"title":  "GitHub policy",
			"claim":  "## rule\n- pin actions by sha",
			"tagIds": []string{opsID, githubID},
		},
	})
	resp := parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0).Required()
	var createOut struct {
		CreateKnowledge knowledgePayload `json:"createKnowledge"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &createOut)).Required()
	created := createOut.CreateKnowledge
	gt.String(t, created.ID).NotEqual("")
	gt.String(t, created.Title).Equal("GitHub policy")
	gt.String(t, created.Claim).Equal("## rule\n- pin actions by sha")
	gt.Array(t, created.Tags).Length(2).Required()
	gt.String(t, created.Tags[0].ID).Equal(opsID)
	gt.String(t, created.Tags[1].ID).Equal(githubID)

	// Create rejected without tags
	recNoTags := executeGraphQLRequest(t, h, createQuery, map[string]interface{}{
		"ws": testWorkspaceID,
		"input": map[string]interface{}{
			"title":  "no tags",
			"tagIds": []string{},
		},
	})
	respNoTags := parseGraphQLResponse(t, recNoTags)
	gt.Bool(t, len(respNoTags.Errors) > 0).True()

	// Create rejected with an unknown tag id
	recBadTag := executeGraphQLRequest(t, h, createQuery, map[string]interface{}{
		"ws": testWorkspaceID,
		"input": map[string]interface{}{
			"title":  "bad tag",
			"tagIds": []string{"00000000-0000-0000-0000-000000000000"},
		},
	})
	respBadTag := parseGraphQLResponse(t, recBadTag)
	gt.Bool(t, len(respBadTag.Errors) > 0).True()

	// List
	listQuery := `query($ws: String!) { knowledges(workspaceId: $ws) { id title tags { id name } } }`
	rec = executeGraphQLRequest(t, h, listQuery, map[string]interface{}{"ws": testWorkspaceID})
	resp = parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0).Required()
	var listOut struct {
		Knowledges []knowledgePayload `json:"knowledges"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &listOut)).Required()
	gt.Array(t, listOut.Knowledges).Length(1).Required()
	gt.String(t, listOut.Knowledges[0].ID).Equal(created.ID)

	// tags query returns the two created tags (sorted by CreatedAt asc)
	rec = executeGraphQLRequest(t, h, `query($ws: String!) { tags(workspaceId: $ws) { id name } }`,
		map[string]interface{}{"ws": testWorkspaceID})
	resp = parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0).Required()
	var tagsOut struct {
		Tags []tagPayload `json:"tags"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &tagsOut)).Required()
	gt.Array(t, tagsOut.Tags).Length(2).Required()
	gt.Value(t, tagsOut.Tags[0].ID).Equal(opsID)
	gt.Value(t, tagsOut.Tags[0].Name).Equal("ops")
	gt.Value(t, tagsOut.Tags[1].ID).Equal(githubID)
	gt.Value(t, tagsOut.Tags[1].Name).Equal("github")

	// searchKnowledge (substring fallback, no embed client wired in tests)
	rec = executeGraphQLRequest(t, h,
		`query($ws: String!, $q: String!) { searchKnowledge(workspaceId: $ws, query: $q) { id title } }`,
		map[string]interface{}{"ws": testWorkspaceID, "q": "github"})
	resp = parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0).Required()
	var searchOut struct {
		SearchKnowledge []knowledgePayload `json:"searchKnowledge"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &searchOut)).Required()
	gt.Array(t, searchOut.SearchKnowledge).Length(1).Required()
	gt.String(t, searchOut.SearchKnowledge[0].ID).Equal(created.ID)

	// Update (title + tags). A new tag must be created before it can be referenced.
	securityID := createTagForTest(t, h, testWorkspaceID, "security")
	updateQuery := `mutation($ws: String!, $input: UpdateKnowledgeInput!) {
		updateKnowledge(workspaceId: $ws, input: $input) { id title tags { id name } }
	}`
	rec = executeGraphQLRequest(t, h, updateQuery, map[string]interface{}{
		"ws": testWorkspaceID,
		"input": map[string]interface{}{
			"id":     created.ID,
			"title":  "GitHub policy v2",
			"tagIds": []string{opsID, githubID, securityID},
		},
	})
	resp = parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0).Required()
	var updateOut struct {
		UpdateKnowledge knowledgePayload `json:"updateKnowledge"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &updateOut)).Required()
	gt.String(t, updateOut.UpdateKnowledge.Title).Equal("GitHub policy v2")
	gt.Array(t, updateOut.UpdateKnowledge.Tags).Length(3)

	// Delete
	rec = executeGraphQLRequest(t, h,
		`mutation($ws: String!, $id: ID!) { deleteKnowledge(workspaceId: $ws, id: $id) }`,
		map[string]interface{}{"ws": testWorkspaceID, "id": created.ID})
	resp = parseGraphQLResponse(t, rec)
	gt.Array(t, resp.Errors).Length(0).Required()
	var deleteOut struct {
		DeleteKnowledge bool `json:"deleteKnowledge"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &deleteOut)).Required()
	gt.Bool(t, deleteOut.DeleteKnowledge).True()

	// List is empty after delete
	rec = executeGraphQLRequest(t, h, listQuery, map[string]interface{}{"ws": testWorkspaceID})
	resp = parseGraphQLResponse(t, rec)
	gt.NoError(t, json.Unmarshal(resp.Data, &listOut)).Required()
	gt.Array(t, listOut.Knowledges).Length(0)
}

func TestGraphQLHandler_ReferenceableCases(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()

	// Create two normal (non-private, OPEN) cases in testWorkspaceID.
	case1 := &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Title:       "Referenceable Case One",
		Description: "First public case",
		AssigneeIDs: []string{},
	}
	createdCase1, err := repo.Case().Create(ctx, testWorkspaceID, case1)
	gt.NoError(t, err).Required()

	case2 := &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		CreatedAt:   time.Now().UTC().Add(-time.Second), // older than case1
		UpdatedAt:   time.Now().UTC().Add(-time.Second),
		Title:       "Referenceable Case Two",
		Description: "Second public case",
		AssigneeIDs: []string{},
	}
	createdCase2, err := repo.Case().Create(ctx, testWorkspaceID, case2)
	gt.NoError(t, err).Required()

	// Create one private case in testWorkspaceID — must be excluded from results.
	privateCase := &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		Title:          "Private Case",
		Description:    "Should not appear in referenceable cases",
		IsPrivate:      true,
		ChannelUserIDs: []string{"UMEMBER"},
		AssigneeIDs:    []string{},
	}
	createdPrivate, err := repo.Case().Create(ctx, testWorkspaceID, privateCase)
	gt.NoError(t, err).Required()

	// Create a case in a different workspace to verify workspace scoping.
	otherWS := fmt.Sprintf("ref-ws-%d", time.Now().UnixNano())
	otherCase := &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Title:       "Other Workspace Case",
		Description: "Belongs to a different workspace",
		AssigneeIDs: []string{},
	}
	_, err = repo.Case().Create(ctx, otherWS, otherCase)
	gt.NoError(t, err).Required()

	srv, err := setupGraphQLServer(repo)
	gt.NoError(t, err).Required()

	t.Run("referenceableCases returns non-private cases only", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $query: String, $limit: Int) {
				referenceableCases(workspaceId: $workspaceId, query: $query, limit: $limit) {
					id
					title
					status
					workspaceId
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, srv, query, variables)
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			ReferenceableCases []struct {
				ID          int    `json:"id"`
				Title       string `json:"title"`
				Status      string `json:"status"`
				WorkspaceID string `json:"workspaceId"`
			} `json:"referenceableCases"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		// Must contain the two public cases and exclude the private case.
		gt.Array(t, result.ReferenceableCases).Length(2)

		foundCase1, foundCase2 := false, false
		for _, c := range result.ReferenceableCases {
			gt.Value(t, c.WorkspaceID).Equal(testWorkspaceID)
			gt.Value(t, c.ID).NotEqual(int(createdPrivate.ID))
			if c.ID == int(createdCase1.ID) {
				foundCase1 = true
				gt.Value(t, c.Title).Equal("Referenceable Case One")
				gt.Value(t, c.Status).Equal("OPEN")
			}
			if c.ID == int(createdCase2.ID) {
				foundCase2 = true
				gt.Value(t, c.Title).Equal("Referenceable Case Two")
				gt.Value(t, c.Status).Equal("OPEN")
			}
		}
		gt.Bool(t, foundCase1).True()
		gt.Bool(t, foundCase2).True()
	})

	t.Run("referenceableCases filters by title query", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $query: String) {
				referenceableCases(workspaceId: $workspaceId, query: $query) {
					id
					title
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"query":       "Case One",
		}

		rec := executeGraphQLRequest(t, srv, query, variables)
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			ReferenceableCases []struct {
				ID    int    `json:"id"`
				Title string `json:"title"`
			} `json:"referenceableCases"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Array(t, result.ReferenceableCases).Length(1)
		gt.Value(t, result.ReferenceableCases[0].ID).Equal(int(createdCase1.ID))
		gt.Value(t, result.ReferenceableCases[0].Title).Equal("Referenceable Case One")
	})

	t.Run("caseRefsByIds resolves given IDs and omits private cases", func(t *testing.T) {
		query := `
			query($workspaceId: String!, $ids: [Int!]!) {
				caseRefsByIds(workspaceId: $workspaceId, ids: $ids) {
					id
					title
					status
					workspaceId
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			// Request case1, case2, and the private case; only the public ones should be returned.
			"ids": []int{int(createdCase1.ID), int(createdCase2.ID), int(createdPrivate.ID)},
		}

		rec := executeGraphQLRequest(t, srv, query, variables)
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			CaseRefsByIds []struct {
				ID          int    `json:"id"`
				Title       string `json:"title"`
				Status      string `json:"status"`
				WorkspaceID string `json:"workspaceId"`
			} `json:"caseRefsByIds"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		// Private case must be excluded; only the two public cases should be returned.
		gt.Array(t, result.CaseRefsByIds).Length(2)
		foundCase1, foundCase2 := false, false
		for _, c := range result.CaseRefsByIds {
			gt.Value(t, c.WorkspaceID).Equal(testWorkspaceID)
			gt.Value(t, c.ID).NotEqual(int(createdPrivate.ID))
			if c.ID == int(createdCase1.ID) {
				foundCase1 = true
				gt.Value(t, c.Title).Equal("Referenceable Case One")
			}
			if c.ID == int(createdCase2.ID) {
				foundCase2 = true
				gt.Value(t, c.Title).Equal("Referenceable Case Two")
			}
		}
		gt.Bool(t, foundCase1).True()
		gt.Bool(t, foundCase2).True()
	})

	t.Run("referenceWorkspaceId exposed via fieldConfiguration for case_ref field", func(t *testing.T) {
		// Build a workspace registry with a case_ref field that points at otherWS.
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			FieldSchema: &config.FieldSchema{
				Fields: []config.FieldDefinition{
					{
						ID:                 "ref-field",
						Name:               "Related Case",
						Type:               types.FieldTypeCaseRef,
						Required:           false,
						ReferenceWorkspace: otherWS,
					},
				},
				Labels: config.EntityLabels{Case: "Case"},
			},
		})

		// Build a server that uses this registry.
		uc := usecase.New(repo, registry)
		resolver := gqlctrl.NewResolver(repo, uc)
		srv := handler.NewDefaultServer(
			gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: resolver}),
		)
		gqlHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			loaders := gqlctrl.NewDataLoaders(repo, nil, nil)
			ctx := gqlctrl.WithDataLoaders(r.Context(), loaders)
			srv.ServeHTTP(w, r.WithContext(ctx))
		})
		registryHandler, err := httpctrl.New(gqlHandler)
		gt.NoError(t, err).Required()

		fieldQuery := `
			query($workspaceId: String!) {
				fieldConfiguration(workspaceId: $workspaceId) {
					fields {
						id
						type
						referenceWorkspaceId
					}
				}
			}
		`
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
		}

		rec := executeGraphQLRequest(t, registryHandler, fieldQuery, variables)
		gt.Value(t, rec.Code).Equal(http.StatusOK)

		resp := parseGraphQLResponse(t, rec)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			FieldConfiguration struct {
				Fields []struct {
					ID                   string  `json:"id"`
					Type                 string  `json:"type"`
					ReferenceWorkspaceID *string `json:"referenceWorkspaceId"`
				} `json:"fields"`
			} `json:"fieldConfiguration"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()

		gt.Array(t, result.FieldConfiguration.Fields).Length(1)
		field := result.FieldConfiguration.Fields[0]
		gt.Value(t, field.ID).Equal("ref-field")
		gt.Value(t, field.Type).Equal("CASE_REF")
		gt.Value(t, field.ReferenceWorkspaceID).NotNil().Required()
		gt.Value(t, *field.ReferenceWorkspaceID).Equal(otherWS)
	})
}

// TestGraphQLHandler_CaseRefWrite drives the WRITE path end-to-end through the
// updateCase GraphQL mutation: setting a case_ref field value referencing a
// public case in the target workspace succeeds and round-trips, while
// referencing a private or non-existent case is rejected at the GraphQL
// boundary by verifyCaseRefsExist (not just at the usecase layer).
func TestGraphQLHandler_CaseRefWrite(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()

	// Target (reference) workspace with one public and one private case.
	otherWS := fmt.Sprintf("ref-ws-%d", time.Now().UnixNano())
	pubRef, err := repo.Case().Create(ctx, otherWS, &model.Case{
		ReporterID: "U-TEST-DEFAULT", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Title: "Public Target", AssigneeIDs: []string{},
	})
	gt.NoError(t, err).Required()
	privRef, err := repo.Case().Create(ctx, otherWS, &model.Case{
		ReporterID: "U-TEST-DEFAULT", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Title: "Private Target", IsPrivate: true, ChannelUserIDs: []string{"UMEMBER"}, AssigneeIDs: []string{},
	})
	gt.NoError(t, err).Required()

	// Base case in testWorkspaceID whose case_ref field we mutate.
	baseCase, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
		ReporterID: "U-TEST-DEFAULT", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Title: "Base", AssigneeIDs: []string{},
	})
	gt.NoError(t, err).Required()

	// Registry: testWorkspaceID has a case_ref field pointing at otherWS.
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "ref_field", Name: "Related Case", Type: types.FieldTypeCaseRef, ReferenceWorkspace: otherWS},
			},
			Labels: config.EntityLabels{Case: "Case"},
		},
	})

	uc := usecase.New(repo, registry)
	resolver := gqlctrl.NewResolver(repo, uc)
	srv := handler.NewDefaultServer(gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: resolver}))
	gqlHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loaders := gqlctrl.NewDataLoaders(repo, nil, nil)
		ctx := gqlctrl.WithDataLoaders(r.Context(), loaders)
		srv.ServeHTTP(w, r.WithContext(ctx))
	})
	handlerWS, err := httpctrl.New(gqlHandler)
	gt.NoError(t, err).Required()

	mutation := `
		mutation($workspaceId: String!, $input: UpdateCaseInput!) {
			updateCase(workspaceId: $workspaceId, input: $input) {
				id
				fields { fieldId value }
			}
		}
	`
	setRef := func(value string) (*graphQLResponse, *httptest.ResponseRecorder) {
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"id":     baseCase.ID,
				"fields": []map[string]interface{}{{"fieldId": "ref_field", "value": value}},
			},
		}
		rec := executeGraphQLRequest(t, handlerWS, mutation, variables)
		return parseGraphQLResponse(t, rec), rec
	}

	t.Run("sets and round-trips a public case reference", func(t *testing.T) {
		resp, rec := setRef(fmt.Sprintf("%d", pubRef.ID))
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		gt.Array(t, resp.Errors).Length(0)

		var result struct {
			UpdateCase struct {
				Fields []struct {
					FieldID string      `json:"fieldId"`
					Value   interface{} `json:"value"`
				} `json:"fields"`
			} `json:"updateCase"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &result)).Required()
		gt.Array(t, result.UpdateCase.Fields).Length(1).Required()
		gt.Value(t, result.UpdateCase.Fields[0].FieldID).Equal("ref_field")
		gt.Value(t, result.UpdateCase.Fields[0].Value).Equal(fmt.Sprintf("%d", pubRef.ID))

		// Persisted value matches.
		stored, err := repo.Case().Get(ctx, testWorkspaceID, baseCase.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.FieldValues["ref_field"].Value).Equal(fmt.Sprintf("%d", pubRef.ID))
	})

	t.Run("rejects a private case reference", func(t *testing.T) {
		resp, _ := setRef(fmt.Sprintf("%d", privRef.ID))
		gt.Number(t, len(resp.Errors)).Greater(0)
	})

	t.Run("rejects a non-existent case reference", func(t *testing.T) {
		resp, _ := setRef("99999999")
		gt.Number(t, len(resp.Errors)).Greater(0)
	})
}

// TestGraphQLHandler_TextPatternWrite drives a text field with a
// validation.pattern through the updateCase mutation: a mismatching value is a
// BAD_USER_INPUT whose message names the pattern, and nothing is written.
func TestGraphQLHandler_TextPatternWrite(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()

	baseCase, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
		ReporterID: "U-TEST-DEFAULT", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Title: "Base", AssigneeIDs: []string{},
		FieldValues: map[string]model.FieldValue{
			"ticket_id": {FieldID: "ticket_id", Type: types.FieldTypeText, Value: "SEC-1"},
		},
	})
	gt.NoError(t, err).Required()

	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "ticket_id", Name: "Ticket ID", Type: types.FieldTypeText,
					Validation: config.FieldValidation{Pattern: "[A-Z]{2,5}-[0-9]+"}},
			},
			Labels: config.EntityLabels{Case: "Case"},
		},
	})

	uc := usecase.New(repo, registry)
	resolver := gqlctrl.NewResolver(repo, uc)
	srv := handler.NewDefaultServer(gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: resolver}))
	// Mirror serve.go so extensions.code reaches the response body.
	srv.SetErrorPresenter(func(ctx context.Context, err error) *gqlerror.Error {
		gqlErr := graphql.DefaultErrorPresenter(ctx, err)
		if gqlErr.Extensions == nil {
			gqlErr.Extensions = map[string]any{}
		}
		maps.Copy(gqlErr.Extensions, gqlctrl.ErrorExtensions(err))
		return gqlErr
	})
	gqlHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loaders := gqlctrl.NewDataLoaders(repo, nil, nil)
		ctx := gqlctrl.WithDataLoaders(r.Context(), loaders)
		srv.ServeHTTP(w, r.WithContext(ctx))
	})
	h, err := httpctrl.New(gqlHandler)
	gt.NoError(t, err).Required()

	mutation := `
		mutation($workspaceId: String!, $input: UpdateCaseInput!) {
			updateCase(workspaceId: $workspaceId, input: $input) {
				id
				fields { fieldId value }
			}
		}
	`
	setTicket := func(value string) *graphQLResponse {
		variables := map[string]interface{}{
			"workspaceId": testWorkspaceID,
			"input": map[string]interface{}{
				"id":     baseCase.ID,
				"fields": []map[string]interface{}{{"fieldId": "ticket_id", "value": value}},
			},
		}
		return parseGraphQLResponse(t, executeGraphQLRequest(t, h, mutation, variables))
	}

	t.Run("a mismatching value is a bad user input and is not written", func(t *testing.T) {
		resp := setTicket("sec-1234")
		gt.Array(t, resp.Errors).Length(1).Required()
		gt.Value(t, resp.Errors[0].Extensions["code"]).Equal(gqlctrl.ErrCodeBadUserInput)
		gt.String(t, resp.Errors[0].Message).Contains("[A-Z]{2,5}-[0-9]+")

		stored, err := repo.Case().Get(ctx, testWorkspaceID, baseCase.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.FieldValues["ticket_id"].Value).Equal("SEC-1")
	})

	t.Run("a matching value is written", func(t *testing.T) {
		resp := setTicket("SEC-1234")
		gt.Array(t, resp.Errors).Length(0)

		stored, err := repo.Case().Get(ctx, testWorkspaceID, baseCase.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.FieldValues["ticket_id"].Value).Equal("SEC-1234")
	})
}

// errorCodes returns the extensions.code of every error in resp.
func errorCodes(resp *graphQLResponse) []string {
	codes := make([]string, 0, len(resp.Errors))
	for _, e := range resp.Errors {
		code, _ := e.Extensions["code"].(string)
		codes = append(codes, code)
	}
	return codes
}

// TestGraphQLHandler_WorkspaceAccessControl drives the per-workspace policy
// through the same wiring serve.go uses: the field middleware plus the
// usecases that read across workspaces. ws-b's policy allows nobody.
func TestGraphQLHandler_WorkspaceAccessControl(t *testing.T) {
	const (
		wsA  = "ws-a"
		wsB  = "ws-b"
		user = "U0ALICE"
	)
	repo := memory.New()
	ctx := context.Background()

	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: wsA, Name: "Workspace A"},
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "ref_field", Name: "Related Case", Type: types.FieldTypeCaseRef, ReferenceWorkspace: wsB},
			},
			Labels: config.EntityLabels{Case: "Case"},
		},
	})
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: wsB, Name: "Workspace B"}})

	groups := model.NewWorkspaceGroupRegistry()
	groups.Register(&model.WorkspaceGroup{ID: "mixed", Name: "Mixed", MemberIDs: []string{wsA, wsB}})
	groups.Register(&model.WorkspaceGroup{ID: "only-b", Name: "Only B", MemberIDs: []string{wsB}})

	access := newDenyingWorkspaceAccess(t, registry, wsB)
	uc := usecase.New(repo, registry, usecase.WithWorkspaceAccess(access), usecase.WithWorkspaceGroups(groups))
	srv := handler.NewDefaultServer(gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: gqlctrl.NewResolver(repo, uc)}))
	srv.AroundFields(gqlctrl.WorkspaceAccessMiddleware(uc.WorkspaceAccess))
	srv.SetErrorPresenter(func(ctx context.Context, err error) *gqlerror.Error {
		gqlErr := graphql.DefaultErrorPresenter(ctx, err)
		if gqlErr.Extensions == nil {
			gqlErr.Extensions = map[string]any{}
		}
		maps.Copy(gqlErr.Extensions, gqlctrl.ErrorExtensions(err))
		return gqlErr
	})
	gqlHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := gqlctrl.WithDataLoaders(r.Context(), gqlctrl.NewDataLoaders(repo, nil, nil))
		srv.ServeHTTP(w, r.WithContext(ctx))
	})
	h, err := httpctrl.New(gqlHandler)
	gt.NoError(t, err).Required()

	now := time.Now().UTC()
	caseA, err := repo.Case().Create(ctx, wsA, &model.Case{
		ReporterID: user, CreatedAt: now, UpdatedAt: now, Title: "Case in A",
		Status: types.CaseStatusOpen, AssigneeIDs: []string{user},
	})
	gt.NoError(t, err).Required()
	caseB, err := repo.Case().Create(ctx, wsB, &model.Case{
		ReporterID: user, CreatedAt: now, UpdatedAt: now, Title: "Case in B",
		Status: types.CaseStatusOpen, AssigneeIDs: []string{user},
	})
	gt.NoError(t, err).Required()
	_, err = repo.Action().Create(ctx, wsA, &model.Action{CaseID: caseA.ID, Title: "Action in A", AssigneeID: user})
	gt.NoError(t, err).Required()
	_, err = repo.Action().Create(ctx, wsB, &model.Action{CaseID: caseB.ID, Title: "Action in B", AssigneeID: user})
	gt.NoError(t, err).Required()

	casesQuery := `query($workspaceId: String!) { cases(workspaceId: $workspaceId) { id title } }`

	t.Run("denied workspace query is FORBIDDEN, allowed one succeeds", func(t *testing.T) {
		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, casesQuery, map[string]any{"workspaceId": wsB}, user))
		gt.Value(t, errorCodes(resp)).Equal([]string{gqlctrl.ErrCodeForbidden})

		resp = parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, casesQuery, map[string]any{"workspaceId": wsA}, user))
		gt.Array(t, resp.Errors).Length(0)
		var data struct {
			Cases []struct {
				Title string `json:"title"`
			} `json:"cases"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &data)).Required()
		gt.Array(t, data.Cases).Length(1).Required()
		gt.Value(t, data.Cases[0].Title).Equal("Case in A")
	})

	t.Run("denied workspace mutation is FORBIDDEN and writes nothing", func(t *testing.T) {
		before, err := repo.Case().List(ctx, wsB)
		gt.NoError(t, err).Required()
		mutation := `mutation($workspaceId: String!, $input: CreateCaseInput!) {
			createCase(workspaceId: $workspaceId, input: $input) { id }
		}`
		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, mutation, map[string]any{
			"workspaceId": wsB,
			"input":       map[string]any{"title": "Should not exist"},
		}, user))
		gt.Value(t, errorCodes(resp)).Equal([]string{gqlctrl.ErrCodeForbidden})
		after, err := repo.Case().List(ctx, wsB)
		gt.NoError(t, err).Required()
		gt.Value(t, len(after)).Equal(len(before))
	})

	t.Run("workspaces and groups hide the denied workspace", func(t *testing.T) {
		query := `query { workspaces { id } workspaceGroups { id workspaces { id } } }`
		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, query, nil, user))
		gt.Array(t, resp.Errors).Length(0)
		var data struct {
			Workspaces []struct {
				ID string `json:"id"`
			} `json:"workspaces"`
			WorkspaceGroups []struct {
				ID         string `json:"id"`
				Workspaces []struct {
					ID string `json:"id"`
				} `json:"workspaces"`
			} `json:"workspaceGroups"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &data)).Required()
		gt.Array(t, data.Workspaces).Length(1).Required()
		gt.Value(t, data.Workspaces[0].ID).Equal(wsA)
		gt.Array(t, data.WorkspaceGroups).Length(1).Required()
		gt.Value(t, data.WorkspaceGroups[0].ID).Equal("mixed")
		gt.Array(t, data.WorkspaceGroups[0].Workspaces).Length(1).Required()
		gt.Value(t, data.WorkspaceGroups[0].Workspaces[0].ID).Equal(wsA)
	})

	t.Run("dashboard excludes the denied workspace", func(t *testing.T) {
		query := `query {
			myOpenCases { workspaceId case { title } }
			myDueActions { workspaceId action { title } }
		}`
		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, query, nil, user))
		gt.Array(t, resp.Errors).Length(0)
		var data struct {
			MyOpenCases []struct {
				WorkspaceID string `json:"workspaceId"`
				Case        struct {
					Title string `json:"title"`
				} `json:"case"`
			} `json:"myOpenCases"`
			MyDueActions []struct {
				WorkspaceID string `json:"workspaceId"`
				Action      struct {
					Title string `json:"title"`
				} `json:"action"`
			} `json:"myDueActions"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &data)).Required()
		gt.Array(t, data.MyOpenCases).Length(1).Required()
		gt.Value(t, data.MyOpenCases[0].WorkspaceID).Equal(wsA)
		gt.Value(t, data.MyOpenCases[0].Case.Title).Equal("Case in A")
		gt.Array(t, data.MyDueActions).Length(1).Required()
		gt.Value(t, data.MyDueActions[0].WorkspaceID).Equal(wsA)
		gt.Value(t, data.MyDueActions[0].Action.Title).Equal("Action in A")
	})

	t.Run("favorites hide and refuse the denied workspace", func(t *testing.T) {
		gt.NoError(t, repo.UserPreference().Set(ctx, &model.UserPreference{
			UserID: user, FavoriteWorkspaceIDs: []string{wsA, wsB}, CreatedAt: now, UpdatedAt: now,
		})).Required()

		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, `query { favoriteWorkspaceIds }`, nil, user))
		gt.Array(t, resp.Errors).Length(0)
		var data struct {
			FavoriteWorkspaceIDs []string `json:"favoriteWorkspaceIds"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &data)).Required()
		gt.Value(t, data.FavoriteWorkspaceIDs).Equal([]string{wsA})

		mutation := `mutation($ids: [String!]!) { setFavoriteWorkspaces(workspaceIds: $ids) }`
		resp = parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, mutation, map[string]any{"ids": []string{wsA, wsB}}, user))
		gt.Value(t, errorCodes(resp)).Equal([]string{gqlctrl.ErrCodeForbidden})
	})

	t.Run("a case_ref into the denied workspace cannot be written", func(t *testing.T) {
		mutation := `mutation($workspaceId: String!, $input: UpdateCaseInput!) {
			updateCase(workspaceId: $workspaceId, input: $input) { id }
		}`
		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, mutation, map[string]any{
			"workspaceId": wsA,
			"input": map[string]any{
				"id":     caseA.ID,
				"fields": []map[string]any{{"fieldId": "ref_field", "value": fmt.Sprintf("%d", caseB.ID)}},
			},
		}, user))
		gt.Value(t, errorCodes(resp)).Equal([]string{gqlctrl.ErrCodeForbidden})
		stored, err := repo.Case().Get(ctx, wsA, caseA.ID)
		gt.NoError(t, err).Required()
		_, hasRef := stored.FieldValues["ref_field"]
		gt.Bool(t, hasRef).False()
	})

	t.Run("a context without a token is not checked", func(t *testing.T) {
		resp := parseGraphQLResponse(t, executeGraphQLRequest(t, h, casesQuery, map[string]any{"workspaceId": wsB}))
		gt.Array(t, resp.Errors).Length(0)
	})
}

// channelNameSlack is a Slack fake that answers GetChannelNames from a map
// and records every lookup.
type channelNameSlack struct {
	mockSlackServiceForCommand
	names   map[string]string
	lookups [][]string
}

func (s *channelNameSlack) GetChannelNames(_ context.Context, ids []string) (map[string]string, error) {
	s.lookups = append(s.lookups, ids)
	out := map[string]string{}
	for _, id := range ids {
		if name, ok := s.names[id]; ok {
			out[id] = name
		}
	}
	return out, nil
}

// TestGraphQLHandler_FieldValueDisplay drives FieldValue.display end to end:
// the fields resolvers stamp the definition's semantic, the display resolver
// batches through the dataloader into UseCases.ResolveTextDisplays, and the
// raw value stays untouched beside it.
func TestGraphQLHandler_FieldValueDisplay(t *testing.T) {
	const wsID = "semantic-ws"
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: wsID, Name: "Semantic"},
		FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
			{ID: "channel", Name: "Channel", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
			{ID: "note", Name: "Note", Type: types.FieldTypeText},
		}},
		MemoConfig: &config.MemoConfig{
			Description: "memo",
			FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
				{ID: "channel", Name: "Channel", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
			}},
		},
	})
	slackFake := &channelNameSlack{names: map[string]string{"C0123ABCD": "general"}}
	uc := usecase.New(repo, registry,
		usecase.WithLLMClient(&gollemmock.LLMClientMock{}),
		usecase.WithSlackService(slackFake),
	)
	srv := handler.NewDefaultServer(gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: gqlctrl.NewResolver(repo, uc)}))
	h, err := httpctrl.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := gqlctrl.WithDataLoaders(r.Context(), gqlctrl.NewDataLoaders(repo, slackFake, uc))
		srv.ServeHTTP(w, r.WithContext(ctx))
	}))
	gt.NoError(t, err).Required()

	ctx := context.Background()
	named, err := repo.Case().Create(ctx, wsID, &model.Case{
		Title: "named", ReporterID: "U-REP", Status: types.CaseStatusOpen,
		FieldValues: map[string]model.FieldValue{
			"channel": {FieldID: "channel", Type: types.FieldTypeText, Value: "C0123ABCD"},
			"note":    {FieldID: "note", Type: types.FieldTypeText, Value: "C0123ABCD"},
		},
	})
	gt.NoError(t, err).Required()
	unnamed, err := repo.Case().Create(ctx, wsID, &model.Case{
		Title: "unnamed", ReporterID: "U-REP", Status: types.CaseStatusOpen,
		FieldValues: map[string]model.FieldValue{
			"channel": {FieldID: "channel", Type: types.FieldTypeText, Value: "C9999ZZZZ"},
		},
	})
	gt.NoError(t, err).Required()

	type display struct {
		Label *string `json:"label"`
		URL   *string `json:"url"`
	}
	type fieldValue struct {
		FieldID string   `json:"fieldId"`
		Value   any      `json:"value"`
		Display *display `json:"display"`
	}
	byID := func(fields []fieldValue) map[string]fieldValue {
		out := map[string]fieldValue{}
		for _, f := range fields {
			out[f.FieldID] = f
		}
		return out
	}

	const casesQuery = `query($ws: String!) { cases(workspaceId: $ws) { id fields { fieldId value display { label url } } } }`
	resp := parseGraphQLResponse(t, executeGraphQLRequest(t, h, casesQuery, map[string]any{"ws": wsID}))
	gt.Array(t, resp.Errors).Length(0).Required()
	var data struct {
		Cases []struct {
			ID     int          `json:"id"`
			Fields []fieldValue `json:"fields"`
		} `json:"cases"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &data)).Required()
	gt.Array(t, data.Cases).Length(2).Required()

	seen := map[int64]bool{}
	for _, c := range data.Cases {
		fields := byID(c.Fields)
		seen[int64(c.ID)] = true
		switch int64(c.ID) {
		case named.ID:
			ch := fields["channel"]
			gt.Value(t, ch.Value).Equal("C0123ABCD")
			gt.Value(t, ch.Display).NotNil().Required()
			gt.Value(t, *ch.Display.Label).Equal("#general")
			gt.Value(t, *ch.Display.URL).Equal("https://slack.com/archives/C0123ABCD")
			// A text field without a semantic gets no display even when its
			// value happens to look like a channel ID.
			gt.Value(t, fields["note"].Display).Nil()
		case unnamed.ID:
			ch := fields["channel"]
			// The name could not be resolved: a display with a null label
			// (shown as an error by the client) and no link.
			gt.Value(t, ch.Value).Equal("C9999ZZZZ")
			gt.Value(t, ch.Display).NotNil().Required()
			gt.Value(t, ch.Display.Label).Nil()
			gt.Value(t, ch.Display.URL).Nil()
		}
	}
	gt.Bool(t, seen[named.ID] && seen[unnamed.ID]).True()
	// Both cases' channel values were resolved in one Slack lookup.
	gt.Array(t, slackFake.lookups).Length(1).Required()
	gt.Array(t, slackFake.lookups[0]).Length(2)

	memoCtx := auth.ContextWithToken(ctx, &auth.Token{Sub: "U-REP"})
	memo, err := uc.Memo.CreateMemo(memoCtx, wsID, usecase.CreateMemoInput{
		CaseID:      named.ID,
		Title:       "memo",
		FieldValues: map[string]model.FieldValue{"channel": {FieldID: "channel", Value: "C0123ABCD"}},
	})
	gt.NoError(t, err).Required()

	const memoQuery = `query($ws: String!, $caseID: Int!, $id: ID!) { memo(workspaceId: $ws, caseID: $caseID, id: $id) { fields { fieldId value display { label url } } } }`
	resp = parseGraphQLResponse(t, executeGraphQLRequest(t, h, memoQuery, map[string]any{
		"ws": wsID, "caseID": named.ID, "id": string(memo.ID),
	}))
	gt.Array(t, resp.Errors).Length(0).Required()
	var memoData struct {
		Memo struct {
			Fields []fieldValue `json:"fields"`
		} `json:"memo"`
	}
	gt.NoError(t, json.Unmarshal(resp.Data, &memoData)).Required()
	ch := byID(memoData.Memo.Fields)["channel"]
	gt.Value(t, ch.Value).Equal("C0123ABCD")
	gt.Value(t, ch.Display).NotNil().Required()
	gt.Value(t, *ch.Display.Label).Equal("#general")
}

// existingChannelSlack answers the Slack calls of the existing-channel create
// flow from fixed tables; every other Service method is the no-op command mock.
type existingChannelSlack struct {
	mockSlackServiceForCommand
	channels     map[string]*slacksvc.ChannelInfo
	members      map[string][]string
	userChannels []slacksvc.Channel
	listCalls    int
}

func (s *existingChannelSlack) GetChannelInfo(_ context.Context, channelID string) (*slacksvc.ChannelInfo, error) {
	info, ok := s.channels[channelID]
	if !ok {
		return nil, slacksvc.ErrChannelUnavailable
	}
	return info, nil
}

func (s *existingChannelSlack) GetConversationMembers(_ context.Context, channelID string) ([]string, error) {
	return s.members[channelID], nil
}

func (s *existingChannelSlack) ListMemberChannels(_ context.Context, _, _ string) ([]slacksvc.Channel, error) {
	s.listCalls++
	return s.userChannels, nil
}

// TestGraphQLHandler_ExistingSlackChannel drives the existing-channel feature
// through the GraphQL wiring serve.go uses: the flag on fieldConfiguration,
// createCase with existingSlackChannelID, the isPrivate input-shape check, and
// the candidate query behind the workspace access middleware.
func TestGraphQLHandler_ExistingSlackChannel(t *testing.T) {
	const (
		wsOn     = "ws-on"
		wsOff    = "ws-off"
		wsDenied = "ws-denied"
		user     = "U0ALICE"
	)
	repo := memory.New()
	ctx := context.Background()
	seedSlackUsersHTTP(t, repo, user)

	registry := model.NewWorkspaceRegistry()
	for _, ws := range []struct {
		id    string
		allow bool
	}{{wsOn, true}, {wsOff, false}, {wsDenied, true}} {
		registry.Register(&model.WorkspaceEntry{
			Workspace:                 model.Workspace{ID: ws.id, Name: ws.id},
			FieldSchema:               &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}},
			CaseMode:                  model.CaseModeChannel,
			AllowExistingSlackChannel: ws.allow,
		})
	}

	slackFake := &existingChannelSlack{
		channels: map[string]*slacksvc.ChannelInfo{
			"C-PUBLIC":  {ID: "C-PUBLIC", Name: "incident-db", IsMember: true},
			"G-PRIVATE": {ID: "G-PRIVATE", Name: "incident-payroll", IsPrivate: true, IsMember: true},
		},
		members: map[string][]string{
			"C-PUBLIC":  {user},
			"G-PRIVATE": {user},
		},
		userChannels: []slacksvc.Channel{{ID: "C-PUBLIC", Name: "incident-db"}},
	}
	access := newDenyingWorkspaceAccess(t, registry, wsDenied)
	uc := usecase.New(repo, registry,
		usecase.WithLLMClient(&gollemmock.LLMClientMock{}),
		usecase.WithSlackService(slackFake),
		usecase.WithWorkspaceAccess(access))
	srv := handler.NewDefaultServer(gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: gqlctrl.NewResolver(repo, uc)}))
	srv.AroundFields(gqlctrl.WorkspaceAccessMiddleware(uc.WorkspaceAccess))
	srv.SetErrorPresenter(func(ctx context.Context, err error) *gqlerror.Error {
		gqlErr := graphql.DefaultErrorPresenter(ctx, err)
		if gqlErr.Extensions == nil {
			gqlErr.Extensions = map[string]any{}
		}
		maps.Copy(gqlErr.Extensions, gqlctrl.ErrorExtensions(err))
		return gqlErr
	})
	h, err := httpctrl.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := gqlctrl.WithDataLoaders(r.Context(), gqlctrl.NewDataLoaders(repo, slackFake, uc))
		srv.ServeHTTP(w, r.WithContext(ctx))
	}))
	gt.NoError(t, err).Required()

	createMutation := `mutation($workspaceId: String!, $input: CreateCaseInput!) {
		createCase(workspaceId: $workspaceId, input: $input) { id slackChannelID isPrivate }
	}`
	type createdCase struct {
		CreateCase struct {
			SlackChannelID string `json:"slackChannelID"`
			IsPrivate      bool   `json:"isPrivate"`
		} `json:"createCase"`
	}
	caseCount := func(ws string) int {
		cases, err := repo.Case().List(ctx, ws)
		gt.NoError(t, err).Required()
		return len(cases)
	}

	t.Run("fieldConfiguration reports whether the workspace allows it", func(t *testing.T) {
		query := `query($workspaceId: String!) { fieldConfiguration(workspaceId: $workspaceId) { allowExistingSlackChannel } }`
		for ws, want := range map[string]bool{wsOn: true, wsOff: false} {
			resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, query, map[string]any{"workspaceId": ws}, user))
			gt.Array(t, resp.Errors).Length(0).Required()
			var data struct {
				FieldConfiguration struct {
					AllowExistingSlackChannel bool `json:"allowExistingSlackChannel"`
				} `json:"fieldConfiguration"`
			}
			gt.NoError(t, json.Unmarshal(resp.Data, &data)).Required()
			gt.Value(t, data.FieldConfiguration.AllowExistingSlackChannel).Equal(want)
		}
	})

	t.Run("createCase links the existing channel", func(t *testing.T) {
		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, createMutation, map[string]any{
			"workspaceId": wsOn,
			"input":       map[string]any{"title": "Linked", "existingSlackChannelID": "C-PUBLIC"},
		}, user))
		gt.Array(t, resp.Errors).Length(0).Required()
		var data createdCase
		gt.NoError(t, json.Unmarshal(resp.Data, &data)).Required()
		gt.Value(t, data.CreateCase.SlackChannelID).Equal("C-PUBLIC")
		gt.Bool(t, data.CreateCase.IsPrivate).False()
	})

	t.Run("a private channel makes a private case", func(t *testing.T) {
		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, createMutation, map[string]any{
			"workspaceId": wsOn,
			"input":       map[string]any{"title": "Private linked", "existingSlackChannelID": "G-PRIVATE"},
		}, user))
		gt.Array(t, resp.Errors).Length(0).Required()
		var data createdCase
		gt.NoError(t, json.Unmarshal(resp.Data, &data)).Required()
		gt.Value(t, data.CreateCase.SlackChannelID).Equal("G-PRIVATE")
		gt.Bool(t, data.CreateCase.IsPrivate).True()
	})

	t.Run("isPrivate with an existing channel is rejected whatever its value", func(t *testing.T) {
		before := caseCount(wsOn)
		for _, isPrivate := range []bool{true, false} {
			resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, createMutation, map[string]any{
				"workspaceId": wsOn,
				"input":       map[string]any{"title": "Rejected", "existingSlackChannelID": "C-PUBLIC", "isPrivate": isPrivate},
			}, user))
			gt.Value(t, errorCodes(resp)).Equal([]string{gqlctrl.ErrCodeBadUserInput})
		}
		gt.Number(t, caseCount(wsOn)).Equal(before)
	})

	t.Run("an already linked channel is SLACK_CHANNEL_ALREADY_LINKED", func(t *testing.T) {
		before := caseCount(wsOn)
		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, createMutation, map[string]any{
			"workspaceId": wsOn,
			"input":       map[string]any{"title": "Duplicate", "existingSlackChannelID": "C-PUBLIC"},
		}, user))
		gt.Value(t, errorCodes(resp)).Equal([]string{gqlctrl.ErrCodeSlackChannelLinked})
		gt.Number(t, caseCount(wsOn)).Equal(before)
	})

	t.Run("a disabled workspace rejects existingSlackChannelID", func(t *testing.T) {
		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, createMutation, map[string]any{
			"workspaceId": wsOff,
			"input":       map[string]any{"title": "Off", "existingSlackChannelID": "C-OTHER"},
		}, user))
		gt.Value(t, errorCodes(resp)).Equal([]string{gqlctrl.ErrCodeBadUserInput})
		gt.Number(t, caseCount(wsOff)).Equal(0)
	})

	t.Run("candidates are FORBIDDEN in a denied workspace and listed in an allowed one", func(t *testing.T) {
		query := `query($workspaceId: String!) { existingSlackChannelCandidates(workspaceId: $workspaceId) { id name isPrivate } }`
		before := slackFake.listCalls
		resp := parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, query, map[string]any{"workspaceId": wsDenied}, user))
		gt.Value(t, errorCodes(resp)).Equal([]string{gqlctrl.ErrCodeForbidden})
		gt.Number(t, slackFake.listCalls).Equal(before)

		// C-PUBLIC is linked by the earlier subtest, so add an unlinked one.
		slackFake.userChannels = append(slackFake.userChannels, slacksvc.Channel{ID: "C-FREE", Name: "free"})
		resp = parseGraphQLResponse(t, executeGraphQLRequestWithAuth(t, h, query, map[string]any{"workspaceId": wsOn}, user))
		gt.Array(t, resp.Errors).Length(0).Required()
		var data struct {
			Candidates []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				IsPrivate bool   `json:"isPrivate"`
			} `json:"existingSlackChannelCandidates"`
		}
		gt.NoError(t, json.Unmarshal(resp.Data, &data)).Required()
		gt.Array(t, data.Candidates).Length(1).Required()
		gt.Value(t, data.Candidates[0].ID).Equal("C-FREE")
		gt.Value(t, data.Candidates[0].Name).Equal("free")
	})
}
