// Package harness delegates a complete run to an external agent session.
package harness

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

// Request is one agent run handed to the harness.
type Request struct {
	// SystemPrompt and Prompt are the finished prompts the agent would have been
	// given.
	SystemPrompt string
	Prompt       string
	// Tools are the tools the run may call, exactly as the kernel built them for
	// its scope. Empty makes the run a single tool-less answer.
	Tools []gollem.Tool
	// CallTool executes one call. It is the kernel's own tool syscall, so limits,
	// metrics and every tool middleware apply as they do in a normal run. Required
	// when Tools is not empty.
	CallTool func(ctx context.Context, call gollem.FunctionCall) (map[string]any, error)
	// OutputSchema is a JSON Schema the final answer must match, or nil for prose.
	OutputSchema json.RawMessage
	// Label identifies the run to the harness worker.
	Label string
	// Role says which side of an evaluation the request belongs to. Empty means
	// RoleAgent: the thing being evaluated. The other roles identify the judge and simulators.
	Role string
}

// The roles a Request can carry.
const (
	RoleAgent   = "agent"
	RoleJudge   = "judge"
	RoleToolSim = "tool-sim"
	RoleUserSim = "user-sim"
)

func (r Request) role() string {
	if r.Role == "" {
		return RoleAgent
	}
	return r.Role
}

// Result is what a finished run produced.
type Result struct {
	// Text is the final answer, as prose or a JSON value for a schema-bound request.
	Text      string
	ToolCalls int
	Duration  time.Duration
}

// Runner runs one request to completion.
type Runner interface {
	Run(ctx context.Context, req Request) (*Result, error)
}

type runnerKey struct{}

// WithRunner sets the runner for the current evaluation.
func WithRunner(ctx context.Context, runner Runner) context.Context {
	return context.WithValue(ctx, runnerKey{}, struct{ Runner }{Runner: runner})
}

// FromContext returns the runner for the current evaluation, if configured.
func FromContext(ctx context.Context) Runner {
	value, _ := ctx.Value(runnerKey{}).(struct{ Runner })
	return value.Runner
}

// Completer answers single tool-less questions through a Runner. It is what the
// eval's judge and simulators are given in this mode, so they take the same one
// path an agent run does.
type Completer struct {
	Runner Runner
	// Role is stamped on every request this completer sends.
	Role string
}

// ForRole returns the same completer for another role. The eval asks for one
// per role, so a request says whether it is the judge's or a simulator's.
func (c Completer) ForRole(role string) Completer {
	c.Role = role
	return c
}

// Complete asks one question and returns the answer: prose, or the JSON object
// when a schema is given.
func (c Completer) Complete(ctx context.Context, systemPrompt, userPrompt string, schema *gollem.Parameter) (string, error) {
	if c.Runner == nil {
		return "", goerr.New("harness: the completer has no runner")
	}
	raw, err := SchemaJSON(schema)
	if err != nil {
		return "", goerr.Wrap(err, "harness: encode the response schema")
	}
	res, err := c.Runner.Run(ctx, Request{
		SystemPrompt: systemPrompt, Prompt: userPrompt, OutputSchema: raw,
		Role: c.Role, Label: c.Role,
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(res.Text) == "" {
		return "", goerr.New("harness: the run returned nothing")
	}
	return res.Text, nil
}

// RefusingLLM is the LLM client a harness-driven process is built with. Every
// model call in this mode goes to the harness as a whole run, so a call that
// reaches this client is a path the mode does not cover — and failing where it
// happens is how such a path gets found, rather than it quietly reaching a
// provider with a key that happens to be exported.
type RefusingLLM struct {
	// ModelName is reported through gollem.ModelNamer so run records still name
	// the model the definition declares.
	ModelName string
}

var _ gollem.LLMClient = RefusingLLM{}

var errDirectLLMCall = goerr.New("harness: a direct LLM call was made while an external harness is active; harness mode does not cover this path")

// NewSession always fails.
func (RefusingLLM) NewSession(context.Context, ...gollem.SessionOption) (gollem.Session, error) {
	return nil, errDirectLLMCall
}

// GenerateEmbedding always fails.
func (RefusingLLM) GenerateEmbedding(context.Context, int, []string) ([][]float64, error) {
	return nil, errDirectLLMCall
}

// Model reports the configured model name.
func (r RefusingLLM) Model() string { return r.ModelName }

// SchemaJSON renders a gollem parameter as a JSON Schema document.
func SchemaJSON(p *gollem.Parameter) (json.RawMessage, error) {
	if p == nil {
		return nil, nil
	}
	return json.Marshal(parameterSchema(p))
}

// toolInputSchema renders a tool's parameters as the JSON Schema of its input
// object.
func toolInputSchema(spec gollem.ToolSpec) map[string]any {
	return objectSchema(spec.Parameters)
}

func objectSchema(props map[string]*gollem.Parameter) map[string]any {
	out := map[string]any{"type": "object"}
	properties := make(map[string]any, len(props))
	var required []string
	for name, p := range props {
		if p == nil {
			continue
		}
		properties[name] = parameterSchema(p)
		if p.Required {
			required = append(required, name)
		}
	}
	out["properties"] = properties
	if len(required) > 0 {
		sort.Strings(required)
		out["required"] = required
	}
	return out
}

func parameterSchema(p *gollem.Parameter) map[string]any {
	out := map[string]any{}
	if p.Title != "" {
		out["title"] = p.Title
	}
	if p.Description != "" {
		out["description"] = p.Description
	}
	if len(p.AnyOf) > 0 {
		alts := make([]any, 0, len(p.AnyOf))
		for _, alt := range p.AnyOf {
			if alt != nil {
				alts = append(alts, parameterSchema(alt))
			}
		}
		out["anyOf"] = alts
		return out
	}

	out["type"] = string(p.Type)
	if len(p.Enum) > 0 {
		enum := make([]any, len(p.Enum))
		for i, v := range p.Enum {
			enum[i] = v
		}
		out["enum"] = enum
	}
	switch p.Type {
	case gollem.TypeObject:
		if p.Properties != nil {
			obj := objectSchema(p.Properties)
			out["properties"] = obj["properties"]
			if req, ok := obj["required"]; ok {
				out["required"] = req
			}
		}
		if p.AdditionalProperties != nil {
			out["additionalProperties"] = parameterSchema(p.AdditionalProperties)
		}
	case gollem.TypeArray:
		if p.Items != nil {
			out["items"] = parameterSchema(p.Items)
		}
		if p.MinItems != nil {
			out["minItems"] = *p.MinItems
		}
		if p.MaxItems != nil {
			out["maxItems"] = *p.MaxItems
		}
	case gollem.TypeString:
		if p.MinLength != nil {
			out["minLength"] = *p.MinLength
		}
		if p.MaxLength != nil {
			out["maxLength"] = *p.MaxLength
		}
		if p.Pattern != "" {
			out["pattern"] = p.Pattern
		}
	case gollem.TypeNumber, gollem.TypeInteger:
		if p.Minimum != nil {
			out["minimum"] = *p.Minimum
		}
		if p.Maximum != nil {
			out["maximum"] = *p.Maximum
		}
	}
	return out
}
