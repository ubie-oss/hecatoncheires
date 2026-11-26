package react

import (
	"context"
	"errors"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/trace"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/toolargs"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

// stepHarness hands the whole run to an external harness and finishes with what
// it returns.
//
// This deliberately breaks the rule the rest of the package keeps — one
// transition is one LLM call or one tool call. The harness owns the loop, so the
// run is a single transition that makes every tool call, and a crash repeats all
// of it, side effects included. That is acceptable where a harness is installed
// (the eval command, against an in-memory store) and is the reason nothing
// installs one in a deployed process.
//
// The tool calls still go through the kernel's syscall, so limits, metrics and
// tool middleware apply to each one exactly as in the normal loop. They use the
// primitive CallTool rather than the session's: there is no conversation of this
// application's to record the results into.
func (s *strategy) stepHarness(ctx context.Context, sys agentkit.Syscalls, st state, h harness.Runner) (state, agentkit.Decision[Output], error) {
	if st.Prompt == "" {
		// The run was handed to the harness on its first transition and finished
		// there, so a state past that point belongs to a run started without one.
		return st, agentkit.Decision[Output]{}, goerr.New("react: a run already in progress cannot be handed to a harness")
	}

	tools := sys.Tools()
	res, err := h.Run(ctx, harness.Request{
		SystemPrompt: st.SystemPrompt,
		Prompt:       st.Prompt,
		Tools:        tools,
		CallTool: func(callCtx context.Context, call gollem.FunctionCall) (map[string]any, error) {
			return sys.CallTool(callCtx, toolargs.Coerce(tools, call))
		},
		OutputSchema: st.OutputSchema,
		Label:        string(sys.Agent()),
	})
	if err != nil {
		// A budget refusal keeps its meaning: the kernel records it as the limit
		// being exceeded rather than as a broken strategy.
		if errors.Is(err, agentkit.ErrLimitExceeded) {
			return st, agentkit.Decision[Output]{}, goerr.Wrap(err, "react: harness run refused by the budget")
		}
		// Failed, not returned as an error. An error makes the kernel retry the
		// transition, and this transition is the whole run: a retry would repeat
		// every tool call the harness already made, side effects included.
		errutil.Handle(ctx, goerr.Wrap(err, "react: harness run"), "react: harness run")
		return st, agentkit.Fail[Output](agentkit.FailureStrategyError, "harness run failed: "+err.Error()), nil
	}

	if handler := trace.HandlerFrom(ctx); handler != nil {
		// The external worker owns its model trace. Record its submitted answer
		// without tokens so the Job timeline and eval report can show the result.
		span := handler.StartLLMCall(ctx)
		handler.EndLLMCall(span, &trace.LLMCallData{
			Model: "external-harness", Request: &trace.LLMRequest{},
			Response: &trace.LLMResponse{Texts: []string{res.Text}},
		}, nil)
	}

	st.Prompt = ""
	st.Texts = append(st.Texts, res.Text)
	return st, agentkit.Done(Output{Texts: st.Texts}), nil
}
