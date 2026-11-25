// Package authz holds the Rego input/output document shapes used to
// authenticate and authorize MCP requests, plus the context plumbing that
// carries per-request HTTP metadata from the controller middleware down to
// the point where a tool call is evaluated against the policy.
//
// The shapes here mirror the warren convention (`data.auth.*` returning an
// `allow` boolean): a policy receives an Input describing the request and the
// tool call, and returns a Result. Keeping these as pure domain types (no
// net/http, no I/O) lets the controller build the *HTTPRequest from the real
// request while the policy adapter stays transport-agnostic.
package authz

import "context"

// HTTPRequest is the transport-level view of the inbound request exposed to
// the policy as `input.req`. It deliberately omits the body: the MCP tool
// call (name / workspace / args) is surfaced separately via ToolCall so the
// policy never has to parse JSON-RPC out of a raw body.
type HTTPRequest struct {
	Method string              `json:"method"`
	Path   string              `json:"path"`
	Header map[string][]string `json:"header"`
}

// ToolCall is the MCP-specific view exposed to the policy as `input.tool`. It
// lets a policy authorize per tool name / workspace / argument rather than
// only at the request level.
type ToolCall struct {
	Name        string         `json:"name"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
	Args        map[string]any `json:"args,omitempty"`
}

// Input is the full document passed to the Rego policy as `input`.
//
// Env carries the operator-selected allow-list of environment variables (e.g.
// a shared MCP token the policy compares the Authorization header against). It
// is tagged `masq:"secret"` so the project logger redacts it; the
// Authorization header inside Req.Header is redacted separately by the
// logger's masq.WithFieldName("Authorization") rule.
type Input struct {
	Req         *HTTPRequest      `json:"req"`
	Env         map[string]string `json:"env" masq:"secret"`
	Tool        *ToolCall         `json:"tool,omitempty"`
	SlackUserID string            `json:"slack_user_id,omitempty"` // Populated after verified identity resolution.
}

// Result is the document the policy is expected to produce under
// `data.auth.mcp`. Allow gates the call; User (optional) is the Slack user ID
// the request acts as in policy-only mode.
type Result struct {
	Allow bool   `json:"allow"`
	User  string `json:"user,omitempty"`
}

type requestCtxKey struct{}

// ContextWithRequest stores the per-request HTTP metadata so BuildInput can
// recover it when a tool call is authorized further down the stack.
func ContextWithRequest(ctx context.Context, req *HTTPRequest) context.Context {
	return context.WithValue(ctx, requestCtxKey{}, req)
}

// RequestFromContext returns the HTTP metadata stored by ContextWithRequest,
// or nil when none was stored.
func RequestFromContext(ctx context.Context) *HTTPRequest {
	req, _ := ctx.Value(requestCtxKey{}).(*HTTPRequest)
	return req
}

// BuildInput assembles the policy Input from the request metadata stored in
// ctx, the operator-provided env allow-list, and the current tool call. env
// and tool may be nil (env nil yields an empty map so policies can index it
// without a nil check).
func BuildInput(ctx context.Context, env map[string]string, tool *ToolCall) Input {
	if env == nil {
		env = map[string]string{}
	}
	return Input{
		Req:  RequestFromContext(ctx),
		Env:  env,
		Tool: tool,
	}
}
