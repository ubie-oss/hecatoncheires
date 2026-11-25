package http_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/adapter/policy"
	httpctrl "github.com/m-mizutani/hecatoncheires/pkg/controller/http"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpTestIdentityHeader = "x-user-assertion"
	mcpTestAudience       = "example.apps.googleusercontent.com"
	mcpTestEmail          = "reader@example.com"
)

type mcpRoundTripFunc func(*http.Request) (*http.Response, error)

func (f mcpRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newMCPTokenFixture(t *testing.T) (http.RoundTripper, func(t *testing.T, overrides map[string]any, wrongKey bool) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	gt.NoError(t, err).Required()
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	gt.NoError(t, err).Required()
	keys, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"alg": "RS256", "kid": "test-key", "kty": "RSA", "e": "AQAB",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
	}}})
	gt.NoError(t, err).Required()
	transport := mcpRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://www.googleapis.com/oauth2/v3/certs" {
			return nil, goerr.New("unexpected request for signing keys")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Cache-Control": {"max-age=3600"}},
			Body:       io.NopCloser(strings.NewReader(string(keys))),
		}, nil
	})

	return transport, func(t *testing.T, overrides map[string]any, wrongKey bool) string {
		t.Helper()
		claims := map[string]any{
			"iss": "https://accounts.google.com", "sub": "1234", "aud": mcpTestAudience,
			"exp": time.Now().Add(time.Hour).Unix(), "email": mcpTestEmail,
			"email_verified": true, "hd": "example.com",
		}
		for name, value := range overrides {
			claims[name] = value
		}
		payload, err := json.Marshal(claims)
		gt.NoError(t, err).Required()
		headers := jws.NewHeaders()
		gt.NoError(t, headers.Set(jws.KeyIDKey, "test-key")).Required()
		signer := key
		if wrongKey {
			signer = other
		}
		signed, err := jws.Sign(payload, jws.WithKey(jwa.RS256(), signer, jws.WithProtectedHeaders(headers)))
		gt.NoError(t, err).Required()
		return string(signed)
	}
}

func mcpRegoPolicy(t *testing.T, rego string) interfaces.PolicyClient {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.rego")
	gt.NoError(t, os.WriteFile(path, []byte(rego), 0600)).Required()
	pc, err := policy.New([]string{path})
	gt.NoError(t, err).Required()
	return pc
}

func TestMCP_IdentityAppliesWorkspaceAccess(t *testing.T) {
	ctx := context.Background()
	transport, issueToken := newMCPTokenFixture(t)
	repo := memory.New()
	gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
		{ID: "UREADER", Email: mcpTestEmail},
		{ID: "UOTHER", Email: "other@example.com"},
	})).Required()
	registry := model.NewWorkspaceRegistry()
	for _, ws := range []string{"visible", "restricted"} {
		registry.Register(&model.WorkspaceEntry{
			Workspace:       model.Workspace{ID: ws, Name: ws},
			ActionStatusSet: model.DefaultActionStatusSet(),
		})
	}
	workspacePolicy := mcpRegoPolicy(t, `package authz
	default allow := false
	allow if { input.workspace.id == "visible"; input.user.id == "UREADER" }
	allow if { input.workspace.id == "restricted"; input.user.id == "UOTHER" }
	`)
	access, err := usecase.NewWorkspaceAccessUseCase(registry, map[string]interfaces.PolicyClient{
		"visible": workspacePolicy, "restricted": workspacePolicy,
	}, repo.SlackUser(), usecase.WorkspaceAccessCacheConfig{Size: 8, TTL: time.Minute})
	gt.NoError(t, err).Required()
	mcpPolicy := mcpRegoPolicy(t, `package auth.mcp
	default allow := false
	allow if {
		input.slack_user_id == "UREADER"
		not input.req.header["X-User-Assertion"]
		input.tool.name != "hecaton_get_actions"
	}
	user := "UOTHER"
	`)
	caseUC := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
	actionUC := usecase.NewActionUseCase(repo, registry, nil, "", nil)
	var publicCaseID, publicActionID int64
	for _, private := range []bool{false, true} {
		c, err := repo.Case().Create(ctx, "visible", &model.Case{
			Title: "Case", ReporterID: "UREADER", IsPrivate: private, ChannelUserIDs: []string{"UREADER"},
		})
		gt.NoError(t, err).Required()
		a, err := actionUC.CreateAction(ctx, "visible", c.ID, "Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		if !private {
			publicCaseID, publicActionID = c.ID, a.ID
		}
	}

	identity, err := httpctrl.NewMCPIdentityOptionForTest(ctx, httpctrl.MCPIdentity{
		Header: mcpTestIdentityHeader, Audience: mcpTestAudience, Users: repo.SlackUser(), WorkspaceAccess: access,
	}, transport)
	gt.NoError(t, err).Required()
	for _, tc := range []struct {
		name    string
		policy  interfaces.PolicyClient
		denyGet bool
	}{
		{name: "identity only"},
		{name: "identity and policy", policy: mcpPolicy, denyGet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := httpctrl.NewMCPHandler(caseUC, actionUC, registry, tc.policy, nil, identity)
			jwt := issueToken(t, nil, false)
			var logs strings.Builder
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.ServeHTTP(w, r.WithContext(logging.With(r.Context(), logger)))
			}))
			t.Cleanup(func() {
				srv.Close()
				seen := map[string]bool{}
				for line := range strings.SplitSeq(logs.String(), "\n") {
					if line == "" {
						continue
					}
					var record map[string]any
					gt.NoError(t, json.Unmarshal([]byte(line), &record)).Required()
					if record["msg"] != "MCP tool call" || record["tool"] != "hecaton_list_cases" {
						continue
					}
					gt.Value(t, record["slack_user_id"]).Equal("UREADER")
					workspace, _ := record["workspace_id"].(string)
					outcome, _ := record["outcome"].(string)
					seen[workspace+"/"+outcome] = true
				}
				gt.Bool(t, seen["visible/success"]).True()
				gt.Bool(t, seen["restricted/denied"]).True()
				gt.Bool(t, strings.Contains(logs.String(), jwt)).False()
				gt.Bool(t, strings.Contains(logs.String(), mcpTestEmail)).False()
			})
			client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{
				Transport: mcpRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					r = r.Clone(r.Context())
					r.Header.Set(mcpTestIdentityHeader, "Bearer "+jwt)
					return srv.Client().Transport.RoundTrip(r)
				}),
			}}, nil)
			gt.NoError(t, err).Required()
			t.Cleanup(func() { gt.NoError(t, session.Close()) })

			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "hecaton_list_workspaces"})
			gt.NoError(t, err).Required()
			gt.Bool(t, res.IsError).False().Required()
			var workspaces struct {
				Workspaces []struct{ ID string }
			}
			decodeStructured(t, res, &workspaces)
			gt.Array(t, workspaces.Workspaces).Length(1).Required()
			gt.String(t, workspaces.Workspaces[0].ID).Equal("visible")

			for _, tool := range []struct {
				name string
				ids  []int64
				key  string
			}{
				{name: "hecaton_list_cases", key: "cases"},
				{name: "hecaton_get_cases", ids: []int64{publicCaseID}, key: "cases"},
				{name: "hecaton_list_actions", key: "actions"},
				{name: "hecaton_get_actions", ids: []int64{publicActionID}, key: "actions"},
			} {
				t.Run(tool.name, func(t *testing.T) {
					args := map[string]any{"workspace_id": "restricted"}
					if tool.ids != nil {
						args["ids"] = tool.ids
					}
					res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: args})
					gt.NoError(t, err).Required()
					gt.Bool(t, res.IsError).True()

					args["workspace_id"] = "visible"
					res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: args})
					gt.NoError(t, err).Required()
					deniedByMCPPolicy := tc.denyGet && tool.name == "hecaton_get_actions"
					gt.Value(t, res.IsError).Equal(deniedByMCPPolicy).Required()
					if !deniedByMCPPolicy {
						var out map[string][]json.RawMessage
						decodeStructured(t, res, &out)
						gt.Array(t, out[tool.key]).Length(1)
					}
				})
			}
		})
	}
}

func TestMCP_KeyRetrievalFailuresAreUnavailable(t *testing.T) {
	ctx := context.Background()
	_, issueToken := newMCPTokenFixture(t)
	token := issueToken(t, nil, false)
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	access, err := usecase.NewWorkspaceAccessUseCase(registry, nil, repo.SlackUser(), usecase.WorkspaceAccessCacheConfig{})
	gt.NoError(t, err).Required()
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{name: "upstream HTTP failure", code: http.StatusBadGateway},
		{name: "invalid key response", code: http.StatusOK, body: "not JSON"},
		{name: "network failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			option, err := httpctrl.NewMCPIdentityOptionForTest(ctx, httpctrl.MCPIdentity{
				Header: mcpTestIdentityHeader, Audience: mcpTestAudience, Users: repo.SlackUser(), WorkspaceAccess: access,
			}, mcpRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
				if tc.code == 0 {
					return nil, goerr.New("network failure containing untrusted text: " + token)
				}
				return &http.Response{StatusCode: tc.code, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			gt.NoError(t, err).Required()
			var logs strings.Builder
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req = req.WithContext(logging.With(req.Context(), logger))
			req.Header.Set(mcpTestIdentityHeader, token)
			resp := httptest.NewRecorder()
			httpctrl.NewMCPHandler(nil, nil, registry, nil, nil, option).ServeHTTP(resp, req)
			gt.Number(t, resp.Code).Equal(http.StatusServiceUnavailable)
			gt.Bool(t, strings.Contains(logs.String(), "MCP token verification unavailable")).True()
			gt.Bool(t, strings.Contains(logs.String()+resp.Body.String(), token)).False()
		})
	}
}

func TestMCP_IdentityRejectsUntrustedRequests(t *testing.T) {
	ctx := context.Background()
	transport, issueToken := newMCPTokenFixture(t)
	users := memory.New().SlackUser()
	gt.NoError(t, users.SaveMany(ctx, []*model.SlackUser{
		{ID: "UREADER", Email: mcpTestEmail},
		{ID: "UFIRST", Email: "duplicate@example.com"},
		{ID: "USECOND", Email: "Duplicate@example.com"},
		{ID: "UGMAIL", Email: "reader@gmail.com"},
	})).Required()
	registry := model.NewWorkspaceRegistry()
	access, err := usecase.NewWorkspaceAccessUseCase(registry, nil, users, usecase.WorkspaceAccessCacheConfig{})
	gt.NoError(t, err).Required()
	good := issueToken(t, nil, false)

	for _, tc := range []struct {
		name      string
		header    string
		values    []string
		status    int
		lookupErr bool
	}{
		{name: "missing", status: http.StatusUnauthorized},
		{name: "wrong header", header: "Authorization", values: []string{good}, status: http.StatusUnauthorized},
		{name: "multiple values", values: []string{good, good}, status: http.StatusUnauthorized},
		{name: "combined values", values: []string{good + ", " + good}, status: http.StatusUnauthorized},
		{name: "empty bearer", values: []string{"Bearer "}, status: http.StatusUnauthorized},
		{name: "raw email", values: []string{mcpTestEmail}, status: http.StatusUnauthorized},
		{name: "wrong key", values: []string{issueToken(t, nil, true)}, status: http.StatusUnauthorized},
		{name: "wrong issuer", values: []string{issueToken(t, map[string]any{"iss": "https://example.com"}, false)}, status: http.StatusUnauthorized},
		{name: "wrong audience", values: []string{issueToken(t, map[string]any{"aud": "other-client"}, false)}, status: http.StatusUnauthorized},
		{name: "expired", values: []string{issueToken(t, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}, false)}, status: http.StatusUnauthorized},
		{name: "missing subject", values: []string{issueToken(t, map[string]any{"sub": nil}, false)}, status: http.StatusUnauthorized},
		{name: "missing email", values: []string{issueToken(t, map[string]any{"email": nil}, false)}, status: http.StatusUnauthorized},
		{name: "unverified email", values: []string{issueToken(t, map[string]any{"email_verified": false}, false)}, status: http.StatusUnauthorized},
		{name: "external email", values: []string{issueToken(t, map[string]any{"hd": nil}, false)}, status: http.StatusUnauthorized},
		{name: "unknown email", values: []string{issueToken(t, map[string]any{"email": "unknown@example.com"}, false)}, status: http.StatusForbidden},
		{name: "ambiguous email", values: []string{issueToken(t, map[string]any{"email": "duplicate@example.com"}, false)}, status: http.StatusForbidden},
		{name: "lookup failure", values: []string{good}, status: http.StatusServiceUnavailable, lookupErr: true},
		{name: "raw JWT", values: []string{good}, status: http.StatusOK},
		{name: "normalized email", values: []string{issueToken(t, map[string]any{"email": "Reader@Example.com"}, false)}, status: http.StatusOK},
		{name: "Gmail identity", values: []string{issueToken(t, map[string]any{"email": "reader@gmail.com", "hd": nil}, false)}, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userRepo := users
			if tc.lookupErr {
				userRepo = mcpFailingSlackDirectory{users}
			}
			identity, err := httpctrl.NewMCPIdentityOptionForTest(ctx, httpctrl.MCPIdentity{
				Header: mcpTestIdentityHeader, Audience: mcpTestAudience, Users: userRepo, WorkspaceAccess: access,
			}, transport)
			gt.NoError(t, err).Required()
			// A permissive legacy policy must never bypass identity authentication.
			handler := httpctrl.NewMCPHandler(nil, nil, registry, allowAsMember(), nil, identity)
			body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			name := tc.header
			if name == "" {
				name = mcpTestIdentityHeader
			}
			for _, value := range tc.values {
				req.Header.Add(name, value)
			}
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			gt.Number(t, resp.Code).Equal(tc.status)
			gt.Bool(t, strings.Contains(resp.Body.String(), mcpTestEmail)).False()
		})
	}
}

type mcpFailingSlackDirectory struct{ interfaces.SlackUserRepository }

func (mcpFailingSlackDirectory) FindByEmail(context.Context, string) (*model.SlackUser, error) {
	return nil, goerr.New("directory unavailable")
}
