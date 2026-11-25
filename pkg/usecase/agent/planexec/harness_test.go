package planexec_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/planexec"
)

// scriptedHarness answers each run with the next scripted text and records what
// it was handed.
type scriptedHarness struct {
	mu      sync.Mutex
	answers []string
	got     []harness.Request
}

func (h *scriptedHarness) Run(_ context.Context, req harness.Request) (*harness.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, req)
	i := len(h.got) - 1
	if i >= len(h.answers) {
		i = len(h.answers) - 1
	}
	return &harness.Result{Text: h.answers[i]}, nil
}

func (h *scriptedHarness) requests() []harness.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]harness.Request(nil), h.got...)
}

// noLLM fails the test on any Generate: a harness-driven turn makes none.
func noLLM(t *testing.T) gollem.LLMClient {
	t.Helper()
	return &mock.LLMClientMock{
		NewSessionFunc: func(context.Context, ...gollem.SessionOption) (gollem.Session, error) {
			t.Error("a harness-driven turn must not open an LLM session")
			return nil, context.Canceled
		},
	}
}

// TestHarnessReplacesTheWholeTurnWithOneRun: with a harness installed nothing is
// planned — one child runs the turn, on the host's own prompt and the whole
// palette, and its answer is the turn's output.
func TestHarnessReplacesTheWholeTurnWithOneRun(t *testing.T) {
	h := &scriptedHarness{answers: []string{"triaged and posted"}}
	ctx := harness.WithRunner(context.Background(), h)
	rt := newTextRuntime(t, noLLM(t), generousBudget(), nil, nil)

	in := textInput()
	in.TaskContext = "channel C1 thread 1.2"
	in.AllowSubAgentWrites = true
	proc := rt.runWithContext(ctx, t, in, nil)

	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)
	out := decodeText(t, proc.Output)
	gt.Value(t, out.Kind).Equal(planexec.OutputFinal)
	gt.Value(t, out.Text).Equal("triaged and posted")

	reqs := h.requests()
	gt.Array(t, reqs).Length(1)
	gt.Value(t, reqs[0].Prompt).Equal(in.UserInput)
	gt.True(t, strings.HasPrefix(reqs[0].SystemPrompt, in.SystemPrompt))
	gt.True(t, strings.Contains(reqs[0].SystemPrompt, "channel C1 thread 1.2"))
	// The run is told the writes are its own to perform, and not handed the
	// planner protocol.
	gt.True(t, strings.Contains(reqs[0].SystemPrompt, "exactly once"))
	gt.False(t, strings.Contains(reqs[0].SystemPrompt, "Planner protocol"))
	gt.Value(t, len(reqs[0].OutputSchema)).Equal(0)
}

// TestHarnessStructuredOutputIsValidatedAndRerun: a structured host's answer goes
// through the same decode and Validate as the normal terminal output, and a
// read-only turn is re-run with the reason when it is rejected.
func TestHarnessStructuredOutputIsValidatedAndRerun(t *testing.T) {
	h := &scriptedHarness{answers: []string{`{"title":""}`, `{"title":"login is broken"}`}}
	ctx := harness.WithRunner(context.Background(), h)
	rt := newRuntime(t, noLLM(t), generousBudget(), nil, nil, planexec.Config[caseDraft]{})

	proc := rt.runWithContext(ctx, t, textInput(), nil)

	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)
	out, err := planexec.DecodeOutput[caseDraft](proc.Output)
	gt.NoError(t, err).Required()
	gt.Value(t, out.Kind).Equal(planexec.OutputFinal)
	gt.Value(t, out.Data.Title).Equal("login is broken")

	reqs := h.requests()
	gt.Array(t, reqs).Length(2)
	gt.True(t, len(reqs[0].OutputSchema) > 0)
	// The re-run carries the request again plus why the first answer was rejected.
	gt.True(t, strings.HasPrefix(reqs[1].Prompt, textInput().UserInput))
	gt.True(t, strings.Contains(reqs[1].Prompt, "title is required"))
}

// TestHarnessDoesNotRerunATurnThatWrites: re-running repeats every tool call, so a
// turn allowed to write reports a rejected output instead of posting twice.
func TestHarnessDoesNotRerunATurnThatWrites(t *testing.T) {
	h := &scriptedHarness{answers: []string{`{"title":""}`}}
	ctx := harness.WithRunner(context.Background(), h)
	rt := newRuntime(t, noLLM(t), generousBudget(), nil, nil, planexec.Config[caseDraft]{})

	in := textInput()
	in.AllowSubAgentWrites = true
	proc := rt.runWithContext(ctx, t, in, nil)

	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)
	out, err := planexec.DecodeOutput[caseDraft](proc.Output)
	gt.NoError(t, err).Required()
	gt.Value(t, out.Kind).Equal(planexec.OutputFallback)
	gt.Array(t, h.requests()).Length(1)
}
