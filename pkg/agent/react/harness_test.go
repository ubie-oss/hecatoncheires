package react_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
)

// fakeHarness stands in for an external harness: it makes the tool calls it was
// scripted with through the request's own caller, then answers.
type fakeHarness struct {
	calls  []gollem.FunctionCall
	answer string
	err    error

	got       harness.Request
	callErrs  []error
	callCount int
}

func (f *fakeHarness) Run(ctx context.Context, req harness.Request) (*harness.Result, error) {
	f.got = req
	for _, call := range f.calls {
		_, err := req.CallTool(ctx, call)
		f.callErrs = append(f.callErrs, err)
		f.callCount++
	}
	if f.err != nil {
		return nil, f.err
	}
	return &harness.Result{Text: f.answer, ToolCalls: f.callCount}, nil
}

// TestHarnessRunsTheWholeLoopInOneTransition pins the shape of a harness-driven
// run: no Generate at all, every tool call made through the kernel, and the
// harness's answer as the run's output.
func TestHarnessRunsTheWholeLoopInOneTransition(t *testing.T) {
	tool := &recordingTool{name: "lookup"}
	llm, generates := scriptedLLM(t) // any Generate fails the test
	rt := newRuntime(t, llm, generousBudget(), tool)

	h := &fakeHarness{
		calls: []gollem.FunctionCall{
			{ID: "1", Name: "lookup", Arguments: map[string]any{"q": "a"}},
			{ID: "2", Name: "lookup", Arguments: map[string]any{"q": "b"}},
		},
		answer: "done",
	}
	ctx := harness.WithRunner(context.Background(), h)

	schema := json.RawMessage(`{"type":"object"}`)
	proc := rt.runWithContext(ctx, t, react.Input{SystemPrompt: "sys", Prompt: "do it", OutputSchema: schema})

	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)
	out, err := react.DecodeOutput(proc.Output)
	gt.NoError(t, err).Required()
	gt.Array(t, out.Texts).Equal([]string{"done"})

	gt.Value(t, generates.Load()).Equal(int32(0))
	gt.Array(t, tool.Calls()).Length(2)
	gt.Value(t, proc.Metrics.Steps).Equal(int64(1))
	gt.Value(t, proc.Metrics.ToolCalls).Equal(int64(2))

	// The harness is handed exactly what the agent would have been given.
	gt.Value(t, h.got.SystemPrompt).Equal("sys")
	gt.Value(t, h.got.Prompt).Equal("do it")
	gt.Value(t, string(h.got.OutputSchema)).Equal(string(schema))
	gt.Array(t, h.got.Tools).Length(1)
}

// TestHarnessToolFailureIsReturnedToTheHarness: a failing tool is the harness's to
// react to, as it is the model's in the normal loop — not the end of the run.
func TestHarnessToolFailureIsReturnedToTheHarness(t *testing.T) {
	tool := &recordingTool{name: "lookup", err: goerr.New("boom")}
	llm, _ := scriptedLLM(t)
	rt := newRuntime(t, llm, generousBudget(), tool)

	h := &fakeHarness{
		calls:  []gollem.FunctionCall{{ID: "1", Name: "lookup", Arguments: map[string]any{}}},
		answer: "recovered",
	}
	ctx := harness.WithRunner(context.Background(), h)

	proc := rt.runWithContext(ctx, t, react.Input{SystemPrompt: "sys", Prompt: "do it"})

	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)
	gt.Array(t, h.callErrs).Length(1)
	gt.Error(t, h.callErrs[0])
}

// TestHarnessFailureFailsTheRun: a harness that cannot finish is a failed run, not
// an empty answer.
func TestHarnessFailureFailsTheRun(t *testing.T) {
	llm, _ := scriptedLLM(t)
	rt := newRuntime(t, llm, generousBudget())
	ctx := harness.WithRunner(context.Background(), &fakeHarness{err: goerr.New("harness is down")})

	proc := rt.runWithContext(ctx, t, react.Input{SystemPrompt: "sys", Prompt: "do it"})

	gt.Value(t, proc.Status).Equal(agentkit.ProcessFailed)
}
