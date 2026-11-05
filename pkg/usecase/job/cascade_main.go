package job

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/toolargs"
)

const (
	cascadeEscalationToolName = "job__request_advisor"
	cascadeMainMaxGenerations = 24
)

// cascadeMainInput fixes the trusted read palette before execution. Every tool
// absent from this list is conservatively a write, including future tools.
type cascadeMainInput struct {
	react.Input
	ReadOnlyNames []string `json:"read_only_names,omitempty"`
}

type cascadeMainState struct {
	Phase         string                 `json:"phase"`
	Input         cascadeMainInput       `json:"input"`
	Pending       []*gollem.FunctionCall `json:"pending,omitempty"`
	Started       bool                   `json:"started,omitempty"`
	ToolsAnswered bool                   `json:"tools_answered,omitempty"`
	Texts         []string               `json:"texts,omitempty"`
	Generations   int                    `json:"generations,omitempty"`
	// WriteStarted is committed in a separate, effect-free transition BEFORE
	// the first write. A crash, timeout or lost response after it can never make
	// the controller mistake an uncertain write for a safe fresh restart.
	WriteStarted bool `json:"write_started,omitempty"`
}

type cascadeMainOutput struct {
	Texts        []string `json:"texts,omitempty"`
	WriteStarted bool     `json:"write_started,omitempty"`
	Escalate     string   `json:"escalate,omitempty"`
	Hold         string   `json:"hold,omitempty"`
}

type cascadeMainStrategy struct{ limiter agentkit.Limiter }

func (*cascadeMainStrategy) Version() int { return 1 }
func (s *cascadeMainStrategy) Limit(ctx context.Context, p *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
	return s.limiter(ctx, p, m)
}
func (*cascadeMainStrategy) Init(in cascadeMainInput) (cascadeMainState, error) {
	if in.SystemPrompt == "" || in.Prompt == "" {
		return cascadeMainState{}, goerr.New("cascade main requires both prompts")
	}
	prompt, err := renderExecutionPrompt(cascadeMainPrompt, executionPromptInput{SystemPrompt: in.SystemPrompt})
	if err != nil {
		return cascadeMainState{}, err
	}
	in.SystemPrompt = prompt
	return cascadeMainState{Input: in, Phase: "generate"}, nil
}

func (s *cascadeMainStrategy) Step(ctx context.Context, sys agentkit.Syscalls, st cascadeMainState) (cascadeMainState, agentkit.Decision[cascadeMainOutput], error) {
	if len(st.Pending) > 0 {
		return s.callTool(ctx, sys, st)
	}
	// The initial stage must leave room for an advisor. This also bounds a model
	// that keeps issuing inexpensive reads without reaching a decision.
	if st.Generations >= cascadeMainMaxGenerations || sys.LimitStatus().Kind() == agentkit.LimitKindNotice {
		return finishMain(st, "main_allowance", "main allowance reached after a write")
	}
	var input []gollem.Input
	if !st.Started {
		input = []gollem.Input{gollem.Text(st.Input.Prompt)}
	} else if !st.ToolsAnswered {
		input = []gollem.Input{gollem.Text("Continue from what you have so far.")}
	}
	opts := []agentkit.GenerateOption{agentkit.WithSystemPrompt(st.Input.SystemPrompt)}
	if !st.WriteStarted {
		opts = append(opts, agentkit.WithTools(tool.NewControl(cascadeEscalationSpec())))
	}
	res, err := sys.Session().Generate(ctx, input, opts...)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, agentkit.ErrLimitExceeded) {
			return st, agentkit.Done(cascadeMainOutput{WriteStarted: st.WriteStarted, Hold: "main execution interrupted"}), nil
		}
		return finishMain(st, "generation_failed", "generation failed after a write")
	}
	st.Started, st.ToolsAnswered = true, false
	st.Generations++
	st.Texts = append(st.Texts, res.Texts...)
	st.Pending = res.FunctionCalls
	if len(st.Pending) == 0 {
		if strings.TrimSpace(strings.Join(st.Texts, "\n")) == "" {
			return finishMain(st, "empty_result", "main produced no result after a write")
		}
		return st, agentkit.Done(cascadeMainOutput{Texts: st.Texts, WriteStarted: st.WriteStarted}), nil
	}
	st.Phase = "tool"
	return st, agentkit.Continue[cascadeMainOutput](), nil
}

func (*cascadeMainStrategy) callTool(ctx context.Context, sys agentkit.Syscalls, st cascadeMainState) (cascadeMainState, agentkit.Decision[cascadeMainOutput], error) {
	call := st.Pending[0]
	if call == nil {
		st = advanceCascadeCall(st)
		return st, agentkit.Continue[cascadeMainOutput](), nil
	}
	if call.Name == cascadeEscalationToolName {
		spec := cascadeEscalationSpec()
		if err := spec.ValidateArgs(call.Arguments); err != nil {
			return finishMain(st, "invalid_advisor_request", "invalid advisor request after a write")
		}
		reason, _ := call.Arguments["reason"].(string)
		return finishMain(st, reason, "advisor restart refused after a write")
	}
	// Unknown tools cannot produce an effect, so they are a repairable model
	// mistake. Validation precedes the barrier; Run never does.
	var tool gollem.Tool
	for _, candidate := range sys.Tools() {
		if candidate.Spec().Name == call.Name {
			tool = candidate
			break
		}
	}
	if tool == nil {
		return finishMain(st, "unknown_tool", "unknown tool after a write")
	}
	coerced := toolargs.Coerce(sys.Tools(), *call)
	spec := tool.Spec()
	if err := spec.ValidateArgs(coerced.Arguments); err != nil {
		return finishMain(st, "invalid_tool_arguments", "invalid tool arguments after a write")
	}
	readOnly := slices.Contains(st.Input.ReadOnlyNames, call.Name)
	if !readOnly && !st.WriteStarted {
		st.WriteStarted = true
		return st, agentkit.Continue[cascadeMainOutput](), nil
	}
	result, err := sys.Session().CallTool(ctx, coerced)
	st = advanceCascadeCall(st)
	st.ToolsAnswered = true
	if err != nil || cascadeToolFailed(result) {
		// Even a transient read failure is held in v1. A stronger model cannot
		// repair a missing source or permissions, and no heuristic classifies a
		// provider's error string as evidence that retrying is safe.
		reason := "source or tool unavailable"
		if st.WriteStarted {
			reason = "tool failed after a write; partial effects may exist"
		}
		return st, agentkit.Done(cascadeMainOutput{Texts: st.Texts, WriteStarted: st.WriteStarted, Hold: reason}), nil
	}
	if result["status"] == "invalid_request" {
		return finishMain(st, "invalid_tool_request", "tool rejected its request after a write")
	}
	return st, agentkit.Continue[cascadeMainOutput](), nil
}

func advanceCascadeCall(st cascadeMainState) cascadeMainState {
	st.Pending = st.Pending[1:]
	if len(st.Pending) == 0 {
		st.Phase = "generate"
	}
	return st
}

func finishMain(st cascadeMainState, escalate, hold string) (cascadeMainState, agentkit.Decision[cascadeMainOutput], error) {
	out := cascadeMainOutput{Texts: st.Texts, WriteStarted: st.WriteStarted}
	if st.WriteStarted {
		out.Hold = hold
	} else {
		out.Escalate = escalate
	}
	return st, agentkit.Done(out), nil
}

// Some business adapters encode failure in a successful Go result. Only explicit
// machine-readable failure fields count; source prose never controls routing.
func cascadeToolFailed(result map[string]any) bool {
	if value, ok := result["success"].(bool); ok && !value {
		return true
	}
	if value, ok := result["is_error"].(bool); ok && value {
		return true
	}
	if value, ok := result["error"].(string); ok && value != "" {
		return true
	}
	if values, ok := result["errors"].([]any); ok && len(values) > 0 {
		return true
	}
	if values, ok := result["errors"].([]string); ok && len(values) > 0 {
		return true
	}
	// Slack bulk retrieval can return successful targets alongside failed ones.
	// Inspect only the adapter's result envelopes, never the source's messages
	// or document text, which may themselves discuss errors.
	switch values := result["results"].(type) {
	case []map[string]any:
		for _, value := range values {
			if cascadeToolFailed(value) {
				return true
			}
		}
	case []any:
		for _, value := range values {
			if envelope, ok := value.(map[string]any); ok && cascadeToolFailed(envelope) {
				return true
			}
		}
	}
	return false
}

func (*cascadeMainStrategy) EncodeState(st cascadeMainState) ([]byte, error) { return json.Marshal(st) }
func (*cascadeMainStrategy) DecodeState(_ int, raw []byte) (cascadeMainState, error) {
	var st cascadeMainState
	err := json.Unmarshal(raw, &st)
	if st.Phase == "" {
		st.Phase = "generate"
		if len(st.Pending) > 0 {
			st.Phase = "tool"
		}
	}
	return st, err
}
func (*cascadeMainStrategy) EncodeOutput(out cascadeMainOutput) ([]byte, error) {
	return json.Marshal(out)
}

func cascadeEscalationSpec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        cascadeEscalationToolName,
		Description: "Request one fresh advisor execution before any business write. Use for unresolved evidence interpretation or identity, never for a missing source or unavailable service.",
		Parameters: map[string]*gollem.Parameter{
			"reason": {Type: gollem.TypeString, Required: true, Enum: []string{"evidence_gap", "conflict", "unsupported", "uncertain_identity"}},
		},
	}
}
