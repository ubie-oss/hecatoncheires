package http

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/authz"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpServerName    = "hecatoncheires"
	mcpServerVersion = "1.0.0"
	// mcpPolicyQuery is the Rego entrypoint evaluated for every tool call.
	mcpPolicyQuery = "data.auth.mcp"
)

// MCP tool names. Every tool is prefixed with "hecaton_" so it stays
// namespaced inside a client that aggregates multiple MCP servers.
const (
	toolListWorkspaces = "hecaton_list_workspaces"
	toolListCases      = "hecaton_list_cases"
	toolGetCases       = "hecaton_get_cases"
	toolListActions    = "hecaton_list_actions"
	toolGetActions     = "hecaton_get_actions"
)

// ErrMCPAuthorizationDenied is returned to the MCP client when the Rego policy
// denies a tool call (result.allow is false).
var ErrMCPAuthorizationDenied = goerr.New("authorization denied")

// mcpHandler holds the dependencies the MCP tool handlers need. The MCP
// endpoint is read-only: it reaches the Case / Action data exclusively through
// the usecase layer (never the repository) and the workspace metadata through
// the registry, mirroring the existing controller handlers.
type mcpHandler struct {
	caseUC   *usecase.CaseUseCase
	actionUC *usecase.ActionUseCase
	registry *model.WorkspaceRegistry
	policy   interfaces.PolicyClient
	env      map[string]string
	identity *mcpIdentity
}

// MCPOption configures how the MCP handler authenticates requests.
type MCPOption func(*mcpHandler)

// NewMCPHandler builds the http.Handler that serves the MCP endpoint over
// Streamable HTTP (mounted at /mcp by the Server). It requires either verified
// identity with workspace authorization or a Rego policy.
//
// The transport runs in Stateless mode so each HTTP request carries its own
// authorization and the request context (populated by withMCPRequestContext)
// flows into the tool handlers — there is no cross-request session state held
// in process memory.
func NewMCPHandler(
	caseUC *usecase.CaseUseCase,
	actionUC *usecase.ActionUseCase,
	registry *model.WorkspaceRegistry,
	policy interfaces.PolicyClient,
	env map[string]string,
	opts ...MCPOption,
) http.Handler {
	h := &mcpHandler{
		caseUC:   caseUC,
		actionUC: actionUC,
		registry: registry,
		policy:   policy,
		env:      env,
	}
	for _, opt := range opts {
		opt(h)
	}
	if h.policy == nil && h.identity == nil {
		panic("MCP handler requires verified identity or a policy client")
	}

	server := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: mcpServerVersion}, nil)
	h.registerTools(server)

	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless: true,
			Logger:    logging.Default(),
		},
	)

	var handler http.Handler = streamable
	if h.policy != nil {
		handler = withMCPRequestContext(handler)
	}
	if h.identity != nil {
		handler = h.identity.authenticate(handler)
	}
	return handler
}

// withMCPRequestContext captures the transport-level request metadata
// (method, path, headers) and stores it on the context so the per-tool
// authorization can hand it to the Rego policy as input.req. The body is not
// captured: the tool name and arguments are surfaced to the policy separately,
// so it never has to parse JSON-RPC out of the body.
func withMCPRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := &authz.HTTPRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: map[string][]string(r.Header.Clone()),
		}
		ctx := authz.ContextWithRequest(r.Context(), req)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authorize checks the current Slack user's workspace access and any MCP policy
// before running a tool. Only policy-only deployments may select a policy user.
func (h *mcpHandler) authorize(ctx context.Context, toolName, workspaceID string, args map[string]any) (context.Context, error) {
	var slackUserID string
	if h.identity != nil {
		token, err := auth.TokenFromContext(ctx)
		if err != nil {
			return ctx, goerr.Wrap(ErrMCPAuthorizationDenied, "verified Slack identity required")
		}
		slackUserID = token.Sub
		if workspaceID != "" {
			if err := h.identity.WorkspaceAccess.Authorize(ctx, workspaceID, slackUserID); err != nil {
				return ctx, goerr.Wrap(err, "MCP workspace access check failed")
			}
		}
	}
	if h.policy == nil {
		return ctx, nil
	}

	input := authz.BuildInput(ctx, h.env, &authz.ToolCall{
		Name:        toolName,
		WorkspaceID: workspaceID,
		Args:        args,
	})
	input.SlackUserID = slackUserID

	var result authz.Result
	if err := h.policy.Query(ctx, mcpPolicyQuery, input, &result); err != nil {
		return ctx, goerr.Wrap(err, "MCP authorization policy evaluation failed", goerr.V("tool", toolName))
	}
	if !result.Allow {
		return ctx, goerr.Wrap(ErrMCPAuthorizationDenied, "MCP tool call denied by policy",
			goerr.V("tool", toolName), goerr.V("workspace_id", workspaceID))
	}
	if h.identity == nil && result.User != "" {
		ctx = auth.ContextWithToken(ctx, &auth.Token{Sub: result.User})
	}
	return ctx, nil
}

// registerTool wires one typed tool onto the server, wrapping its run function
// with the shared authorization gate. Every data-returning tool goes through
// here, so no tool can return data without passing its authorization checks.
func registerTool[In, Out any](s *mcp.Server, h *mcpHandler, name, description string, run func(context.Context, In) (Out, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: description},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
			var zero Out
			args, workspaceID := toolArgs(in)
			authCtx, err := h.authorize(ctx, name, workspaceID, args)
			if err != nil {
				return nil, zero, err
			}
			out, err := run(authCtx, in)
			if err != nil {
				return nil, zero, err
			}
			return nil, out, nil
		})
}

// toolArgs renders the typed tool input into a generic map for the policy and
// extracts the workspace_id (empty for tools that take no workspace). The
// round-trip cannot fail for the concrete input structs used here; on the
// theoretical marshal error we degrade to empty args, which the default-deny
// policy treats as an unauthorized call.
func toolArgs(in any) (map[string]any, string) {
	raw, err := json.Marshal(in)
	if err != nil {
		return map[string]any{}, ""
	}
	args := map[string]any{}
	if err := json.Unmarshal(raw, &args); err != nil {
		return map[string]any{}, ""
	}
	workspaceID, _ := args["workspace_id"].(string)
	return args, workspaceID
}
