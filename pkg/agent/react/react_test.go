package react_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	agentprocmemory "github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/budget"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/agentarchive"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// recordingTool records every call so a test can assert what the model actually
// executed, not merely how many times something happened.
type recordingTool struct {
	name string
	// params is the declared parameter set. Nil for a tool whose arguments are
	// not what the test is about.
	params map[string]*gollem.Parameter
	err    error

	mu    sync.Mutex
	calls []map[string]any
}

func (t *recordingTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{Name: t.name, Description: "test tool", Parameters: t.params}
}

func (t *recordingTool) Run(_ context.Context, args map[string]any) (map[string]any, error) {
	t.mu.Lock()
	t.calls = append(t.calls, args)
	t.mu.Unlock()
	if t.err != nil {
		return nil, t.err
	}
	return map[string]any{"ok": true}, nil
}

func (t *recordingTool) Calls() []map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]map[string]any, len(t.calls))
	copy(out, t.calls)
	return out
}

// scriptedLLM answers with responses[i] on the i-th Generate. An extra call
// beyond the script fails the test rather than silently repeating the last
// answer.
func scriptedLLM(t *testing.T, responses ...*gollem.Response) (gollem.LLMClient, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	client := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					i := int(n.Add(1)) - 1
					if i >= len(responses) {
						return nil, goerr.New("unexpected extra generate call", goerr.V("call_index", i))
					}
					return responses[i], nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
				},
			}, nil
		},
	}
	return client, &n
}

// scriptedLLMRecordingConversation is scriptedLLM plus the conversation each
// Generate was seeded with, so a test can assert HOW tool results reached the
// model, not only that the run finished. It grows the history by one message per
// call, so a later call is seeded with the conversation the run actually built.
func scriptedLLMRecordingConversation(t *testing.T, responses ...*gollem.Response) (gollem.LLMClient, func() [][]gollem.Message) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen [][]gollem.Message
		n    atomic.Int32
	)
	client := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
			cfg := gollem.NewSessionConfig(opts...)
			var seeded []gollem.Message
			if h := cfg.History(); h != nil {
				seeded = h.Messages
			}
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					mu.Lock()
					seen = append(seen, seeded)
					mu.Unlock()
					i := int(n.Add(1)) - 1
					if i >= len(responses) {
						return nil, goerr.New("unexpected extra generate call", goerr.V("call_index", i))
					}
					return responses[i], nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					grown := make([]gollem.Message, len(seeded), len(seeded)+1)
					copy(grown, seeded)
					grown = append(grown, gollem.Message{Role: gollem.RoleAssistant})
					return &gollem.History{LLType: gollem.LLMTypeOpenAI,
						Version: gollem.HistoryVersion, Messages: grown}, nil
				},
			}, nil
		},
	}
	return client, func() [][]gollem.Message {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]gollem.Message, len(seen))
		copy(out, seen)
		return out
	}
}

// trailingToolResponses reports the results the conversation's LAST message
// carries, in order — empty unless that message is a tool message.
//
// Only the trailing message counts: a provider reads the results as the answer to
// the model turn immediately before them, so results that ended up anywhere else
// did not answer the call they belong to.
func trailingToolResponses(t *testing.T, messages []gollem.Message) []gollem.ToolResponseContent {
	t.Helper()
	if len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	if last.Role != gollem.RoleTool {
		return nil
	}
	out := make([]gollem.ToolResponseContent, 0, len(last.Contents))
	for _, c := range last.Contents {
		resp, err := c.GetToolResponseContent()
		gt.NoError(t, err).Required()
		out = append(out, *resp)
	}
	return out
}

type runtime struct {
	kernel *agentkit.Kernel
	agent  agentkit.Agent[react.Input]
}

func newRuntime(t *testing.T, llm gollem.LLMClient, cfg budget.Config, tools ...gollem.Tool) *runtime {
	t.Helper()
	return newRuntimeWithSpend(t, llm, cfg, testSpend(), tools...)
}

// newRuntimeWithSpend is newRuntime with the money resolver named, for a test
// about the money ceiling rather than the step or token ones.
func newRuntimeWithSpend(t *testing.T, llm gollem.LLMClient, cfg budget.Config,
	spend budget.LimitResolver, tools ...gollem.Tool,
) *runtime {
	t.Helper()

	reg := agentkit.NewRegistry()
	handle, err := react.Register(reg, "react-test", 1, cfg.Limiter(spend),
		agentkit.WithHistoryStore[react.Output](agentarchive.NewMemoryHistoryStore()))
	gt.NoError(t, err).Required()

	k, err := agentkit.New(agentprocmemory.New(), llm, reg,
		agentkit.WithToolFactory(func(context.Context, *agentkit.Process) ([]gollem.Tool, error) {
			return tools, nil
		}))
	gt.NoError(t, err).Required()

	return &runtime{kernel: k, agent: handle}
}

// run drives the process to a terminal state and returns it.
func (rt *runtime) run(t *testing.T, in react.Input) *agentkit.Process {
	t.Helper()
	return rt.runWithContext(context.Background(), t, in)
}

func (rt *runtime) runWithContext(parent context.Context, t *testing.T, in react.Input) *agentkit.Process {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- rt.kernel.Serve(ctx, agentkit.WithPollInterval(5*time.Millisecond)) }()

	pid, err := rt.agent.Spawn(ctx, rt.kernel, in)
	gt.NoError(t, err).Required()

	for {
		proc, err := rt.kernel.GetProcess(ctx, pid)
		gt.NoError(t, err).Required()
		if proc.Status.Terminal() {
			cancel()
			<-served
			return proc
		}
		select {
		case <-ctx.Done():
			gt.NoError(t, ctx.Err()).Required()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func textResponse(text string) *gollem.Response {
	return &gollem.Response{Texts: []string{text}, InputToken: 5, OutputToken: 3}
}

func callResponse(calls ...*gollem.FunctionCall) *gollem.Response {
	return &gollem.Response{FunctionCalls: calls, InputToken: 5, OutputToken: 3}
}

func generousBudget() budget.Config {
	return budget.Config{MaxSteps: 64, MaxInputTokens: 100_000, MaxOutputTokens: 100_000, NoticeRatio: 0.8}
}

// testSpend is what these runs are judged against in money. A limiter with no
// resolver stops every run, so one is required even where the test is about a
// step or token ceiling; this prices a run far below its allowance so the money
// arm stays out of the way. TestABudgetNoticeFromTheMoneyCeiling supplies its
// own instead.
func testSpend() budget.LimitResolver {
	return func(*agentkit.Process) budget.RunLimit {
		return budget.RunLimit{
			Budget: pricing.FromUSD(1000),
			Rate:   pricing.Rate{Input: 1, Output: 1},
		}
	}
}

// TestAnswerWithoutTools pins the shortest run: one LLM call, no tools, done.
func TestAnswerWithoutTools(t *testing.T) {
	llm, calls := scriptedLLM(t, textResponse("the answer"))
	rt := newRuntime(t, llm, generousBudget())

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "what is it"})

	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)
	out, err := react.DecodeOutput(proc.Output)
	gt.NoError(t, err).Required()
	gt.Array(t, out.Texts).Equal([]string{"the answer"})
	gt.Value(t, calls.Load()).Equal(int32(1))
	gt.Value(t, proc.Metrics.Steps).Equal(int64(1))
}

// TestOneToolCallPerTransition is the property this strategy exists for: a
// response asking for two tools becomes two separate committed transitions, so
// a crash costs at most one tool call rather than the whole round.
func TestOneToolCallPerTransition(t *testing.T) {
	tool := &recordingTool{name: "probe__ping"}
	llm, _ := scriptedLLM(t,
		callResponse(
			&gollem.FunctionCall{ID: "c1", Name: "probe__ping", Arguments: map[string]any{"n": float64(1)}},
			&gollem.FunctionCall{ID: "c2", Name: "probe__ping", Arguments: map[string]any{"n": float64(2)}},
		),
		textResponse("done"),
	)
	rt := newRuntime(t, llm, generousBudget(), tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "run twice"})

	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	// generate, tool, tool, generate.
	gt.Value(t, proc.Metrics.Steps).Equal(int64(4))
	gt.Value(t, proc.Metrics.LLMCalls).Equal(int64(2))
	gt.Value(t, proc.Metrics.ToolCalls).Equal(int64(2))

	got := tool.Calls()
	gt.Array(t, got).Length(2).Required()
	gt.Value(t, got[0]["n"]).Equal(float64(1))
	gt.Value(t, got[1]["n"]).Equal(float64(2))

	out, err := react.DecodeOutput(proc.Output)
	gt.NoError(t, err).Required()
	gt.Array(t, out.Texts).Equal([]string{"done"})
}

// A model turn asking for several tools at once is answered by ONE turn carrying
// every result, in the order asked. Answering them one turn at a time is what a
// provider rejects ("the number of function response parts is equal to the number
// of function call parts of the function call turn"), and once the conversation
// holds that split every later call in the run is rejected too.
func TestParallelToolResultsAreReportedInOneTurn(t *testing.T) {
	tool := &recordingTool{name: "probe__ping"}
	llm, conversations := scriptedLLMRecordingConversation(t,
		callResponse(
			&gollem.FunctionCall{ID: "c1", Name: "probe__ping", Arguments: map[string]any{"n": float64(1)}},
			&gollem.FunctionCall{ID: "c2", Name: "probe__ping", Arguments: map[string]any{"n": float64(2)}},
			&gollem.FunctionCall{ID: "c3", Name: "probe__ping", Arguments: map[string]any{"n": float64(3)}},
		),
		textResponse("done"),
	)
	rt := newRuntime(t, llm, generousBudget(), tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "run three"})
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	seen := conversations()
	gt.Array(t, seen).Length(2).Required()
	// The opening call has nothing behind it; the one after the tool phase is
	// answered by a single turn holding all three results.
	gt.Array(t, trailingToolResponses(t, seen[0])).Length(0)
	answers := trailingToolResponses(t, seen[1])
	gt.Array(t, answers).Length(3).Required()
	gt.Value(t, answers[0].ToolCallID).Equal("c1")
	gt.Value(t, answers[1].ToolCallID).Equal("c2")
	gt.Value(t, answers[2].ToolCallID).Equal("c3")
	gt.Value(t, answers[0].Response).Equal(map[string]any{"ok": true})
}

// TestFailingToolIsReportedToTheModel pins that a tool error does not fail the
// run: the model is told and gets to react, which is how the previous runtime
// behaved.
func TestFailingToolIsReportedToTheModel(t *testing.T) {
	tool := &recordingTool{name: "probe__ping", err: goerr.New("backend unavailable")}
	llm, conversations := scriptedLLMRecordingConversation(t,
		callResponse(&gollem.FunctionCall{ID: "c1", Name: "probe__ping", Arguments: map[string]any{}}),
		textResponse("the backend is down"),
	)
	rt := newRuntime(t, llm, generousBudget(), tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "try it"})

	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)
	gt.Array(t, tool.Calls()).Length(1)

	// The failure reaches the model as this call's answer, so the call is
	// answered and the failure is what the model reacts to.
	seen := conversations()
	gt.Array(t, seen).Length(2).Required()
	answers := trailingToolResponses(t, seen[1])
	gt.Array(t, answers).Length(1).Required()
	gt.Value(t, answers[0].ToolCallID).Equal("c1")
	gt.Bool(t, answers[0].IsError).True()
	gt.String(t, fmt.Sprint(answers[0].Response["error"])).Contains("backend unavailable")

	out, err := react.DecodeOutput(proc.Output)
	gt.NoError(t, err).Required()
	gt.Array(t, out.Texts).Equal([]string{"the backend is down"})
}

// A single value sent for an array-typed argument reaches the tool as the batch
// of one the model meant. gollem validates arguments against the spec before the
// tool runs and refuses the whole call otherwise; a model answering that refusal
// in production re-emitted the same call, so the writes it carried were never
// applied while the run still reported success.
func TestASingleValueForAnArrayArgumentStillReachesTheTool(t *testing.T) {
	tool := &recordingTool{
		name: "memo__apply_memo_changes",
		params: map[string]*gollem.Parameter{
			"archives": {Type: gollem.TypeArray, Items: &gollem.Parameter{Type: gollem.TypeString}},
		},
	}
	llm, conversations := scriptedLLMRecordingConversation(t,
		callResponse(&gollem.FunctionCall{
			ID:        "c1",
			Name:      "memo__apply_memo_changes",
			Arguments: map[string]any{"archives": "memo-1"},
		}),
		textResponse("archived"),
	)
	rt := newRuntime(t, llm, generousBudget(), tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "archive that memo"})

	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	got := tool.Calls()
	gt.Array(t, got).Length(1).Required()
	gt.Value(t, got[0]["archives"]).Equal([]any{"memo-1"})

	seen := conversations()
	gt.Array(t, seen).Length(2).Required()
	answers := trailingToolResponses(t, seen[1])
	gt.Array(t, answers).Length(1).Required()
	gt.Bool(t, answers[0].IsError).False()
}

// TestTextsAccumulateAcrossRounds pins that a model narrating between tool calls
// keeps every block, in order — the reply a host posts is built from all of them.
func TestTextsAccumulateAcrossRounds(t *testing.T) {
	tool := &recordingTool{name: "probe__ping"}
	llm, _ := scriptedLLM(t,
		&gollem.Response{
			Texts:         []string{"let me check"},
			FunctionCalls: []*gollem.FunctionCall{{ID: "c1", Name: "probe__ping", Arguments: map[string]any{}}},
		},
		textResponse("all good"),
	)
	rt := newRuntime(t, llm, generousBudget(), tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "check"})

	out, err := react.DecodeOutput(proc.Output)
	gt.NoError(t, err).Required()
	gt.Array(t, out.Texts).Equal([]string{"let me check", "all good"})
	gt.String(t, out.Text()).Equal("let me check\nall good")
}

// TestStepBudgetStopsTheRun pins that the ceiling actually ends a run that will
// not stop on its own, and that it does so as a limit failure rather than a
// generic error.
func TestStepBudgetStopsTheRun(t *testing.T) {
	tool := &recordingTool{name: "probe__ping"}
	responses := make([]*gollem.Response, 0, 20)
	for range 20 {
		responses = append(responses, callResponse(&gollem.FunctionCall{
			ID: "c", Name: "probe__ping", Arguments: map[string]any{},
		}))
	}
	llm, _ := scriptedLLM(t, responses...)
	rt := newRuntime(t, llm, budget.Config{
		MaxSteps: 6, MaxInputTokens: 100_000, MaxOutputTokens: 100_000, NoticeRatio: 0.8,
	}, tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "loop forever"})

	gt.Value(t, proc.Status).Equal(agentkit.ProcessFailed)
	gt.Value(t, proc.Failure).NotNil().Required()
	gt.Value(t, proc.Failure.Code).Equal(agentkit.FailureLimitExceeded)
	gt.Number(t, proc.Metrics.Steps).GreaterOrEqual(6)
}

// TestTokenBudgetsStopTheRun pins the two token ceilings independently of the
// step count and of each other. A run with plenty of steps left still stops once
// it has spent either allowance, and the output ceiling — the expensive one —
// bites even while the input allowance is barely touched.
func TestTokenBudgetsStopTheRun(t *testing.T) {
	testCases := map[string]struct {
		cfg    budget.Config
		input  int
		output int
	}{
		"input ceiling": {
			cfg:    budget.Config{MaxSteps: 1000, MaxInputTokens: 120, MaxOutputTokens: 100_000, NoticeRatio: 0.8},
			input:  40,
			output: 1,
		},
		"output ceiling with input to spare": {
			cfg:    budget.Config{MaxSteps: 1000, MaxInputTokens: 100_000, MaxOutputTokens: 120, NoticeRatio: 0.8},
			input:  1,
			output: 40,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			tool := &recordingTool{name: "probe__ping"}
			responses := make([]*gollem.Response, 0, 20)
			for range 20 {
				responses = append(responses, &gollem.Response{
					FunctionCalls: []*gollem.FunctionCall{{ID: "c", Name: "probe__ping", Arguments: map[string]any{}}},
					InputToken:    tc.input,
					OutputToken:   tc.output,
				})
			}
			llm, _ := scriptedLLM(t, responses...)
			rt := newRuntime(t, llm, tc.cfg, tool)

			proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "loop forever"})

			gt.Value(t, proc.Status).Equal(agentkit.ProcessFailed)
			gt.Value(t, proc.Failure).NotNil().Required()
			gt.Value(t, proc.Failure.Code).Equal(agentkit.FailureLimitExceeded)
		})
	}
}

// call is what one Generate was told: the text of its inputs, the system prompt
// in force for it, the tool-call ids the conversation answers going into it, and
// whether it was sent with tool calls disabled.
type call struct {
	text          string
	systemPrompt  string
	answered      []string
	toolsDisabled bool
}

// inputRecordingLLM is scriptedLLM plus a record of what each Generate received,
// so a test can assert what the model was actually told and in which turn. Like
// scriptedLLMRecordingConversation it grows the history by one message per call,
// because the answered calls are read off the conversation.
//
// A script entry may be an `error` instead of a `*gollem.Response`, which fails
// that call — the runtime then retries the transition from its checkpoint, and the
// next entry answers the retry.
func inputRecordingLLM(t *testing.T, responses ...any) (gollem.LLMClient, func() []call) {
	t.Helper()
	var mu sync.Mutex
	var seen []call
	var n atomic.Int32

	client := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
			// The system prompt is a session-level setting, so it is read here and
			// attributed to the calls this session makes.
			cfg := gollem.NewSessionConfig(opts...)
			var seeded []gollem.Message
			if h := cfg.History(); h != nil {
				seeded = h.Messages
			}
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, input []gollem.Input, genOpts ...gollem.GenerateOption) (*gollem.Response, error) {
					genCfg := gollem.NewGenerateConfig(genOpts...)
					rec := call{
						systemPrompt:  cfg.SystemPrompt(),
						toolsDisabled: genCfg.ToolCallsDisabled(),
					}
					var b strings.Builder
					for _, in := range input {
						if txt, ok := in.(gollem.Text); ok {
							b.WriteString(string(txt))
							b.WriteString("\n")
						}
					}
					rec.text = b.String()
					for _, resp := range trailingToolResponses(t, seeded) {
						rec.answered = append(rec.answered, resp.ToolCallID)
					}
					mu.Lock()
					seen = append(seen, rec)
					mu.Unlock()

					i := int(n.Add(1)) - 1
					if i >= len(responses) {
						return nil, goerr.New("unexpected extra generate call", goerr.V("call_index", i))
					}
					switch scripted := responses[i].(type) {
					case error:
						return nil, scripted
					case *gollem.Response:
						return scripted, nil
					default:
						return nil, goerr.New("unsupported script entry", goerr.V("call_index", i))
					}
				},
				HistoryFunc: func() (*gollem.History, error) {
					grown := make([]gollem.Message, len(seeded), len(seeded)+1)
					copy(grown, seeded)
					grown = append(grown, gollem.Message{Role: gollem.RoleAssistant})
					return &gollem.History{LLType: gollem.LLMTypeOpenAI,
						Version: gollem.HistoryVersion, Messages: grown}, nil
				},
			}, nil
		},
	}
	return client, func() []call {
		mu.Lock()
		defer mu.Unlock()
		out := make([]call, len(seen))
		copy(out, seen)
		return out
	}
}

// A Generate that FAILS after a tool round is retried from the checkpoint, and the
// retry must send the same shape: no input, with the results still answering the
// model's calls exactly once.
//
// This pins where ToolsAnswered is cleared. Clearing it before the call, or on the
// error path, would make the retry send a continuation text while the conversation
// already answers the model — and the results are in the conversation whatever the
// retry does, so sending them "again" is not an option either.
func TestAnsweredToolCallsSurviveAFailedGenerate(t *testing.T) {
	tool := &recordingTool{name: "probe__ping"}
	llm, inputs := inputRecordingLLM(t,
		callResponse(
			&gollem.FunctionCall{ID: "c1", Name: "probe__ping", Arguments: map[string]any{}},
			&gollem.FunctionCall{ID: "c2", Name: "probe__ping", Arguments: map[string]any{}},
		),
		// The call that continues from the answered round fails; agentkit retries the
		// whole transition from the last checkpoint, which the tool phase left.
		goerr.New("the provider is briefly unavailable"),
		textResponse("done"),
	)
	rt := newRuntime(t, llm, generousBudget(), tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "run both"})
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	// The tools ran once, in the transitions that committed before the failure.
	gt.Array(t, tool.Calls()).Length(2)

	seen := inputs()
	gt.Array(t, seen).Length(3).Required()
	// The failed call and the retry continue from the same answered round, and
	// neither adds a turn of its own.
	gt.Value(t, seen[1].answered).Equal([]string{"c1", "c2"})
	gt.Value(t, seen[2].answered).Equal([]string{"c1", "c2"})
	gt.String(t, seen[1].text).Equal("")
	gt.String(t, seen[2].text).Equal("")

	out, err := react.DecodeOutput(proc.Output)
	gt.NoError(t, err).Required()
	gt.Array(t, out.Texts).Equal([]string{"done"})
}

// TestBudgetNoticeReachesTheModel pins that crossing the notice ratio TELLS the
// model what to do with what is left, rather than only being enforced against it
// later. Without this the documented behaviour would be a promise the runtime
// does not keep, and a run would get cut off mid-investigation having neither
// performed the call its task needed nor said anything.
func TestBudgetNoticeReachesTheModel(t *testing.T) {
	tool := &recordingTool{name: "probe__ping"}
	call := func() *gollem.Response {
		return callResponse(&gollem.FunctionCall{ID: "c", Name: "probe__ping", Arguments: map[string]any{}})
	}
	llm, inputs := inputRecordingLLM(t, call(), call(), textResponse("here is what I have"))

	// Notice at 40% of 10 steps = 4 committed transitions. The first Generate is
	// step 0, so it must be clean; a later one must carry the notice.
	rt := newRuntime(t, llm, budget.Config{
		MaxSteps: 10, MaxInputTokens: 100_000, MaxOutputTokens: 100_000, NoticeRatio: 0.4,
	}, tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "investigate"})
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	seen := inputs()
	gt.Array(t, seen).Length(3).Required()
	// The opening call is nowhere near the ceiling, so it must not be nagged.
	gt.Bool(t, strings.Contains(seen[0].systemPrompt, "reserve")).False()
	// The last call is past the notice threshold and must carry both the
	// limiter's own message and the instruction derived from it — in the system
	// prompt, because that call sends no user turn at all: it continues from the
	// tool result the conversation already carries.
	gt.String(t, seen[2].systemPrompt).Contains("nearly exhausted")
	gt.String(t, seen[2].systemPrompt).Contains("THIS turn is your final tool call")
	gt.String(t, seen[2].systemPrompt).Contains("be helpful")
	// The reserve's own tool round has not been spent yet, so this call must NOT
	// be told to stop calling tools — being told that here is what made the run
	// skip the call its task still needed.
	gt.Bool(t, strings.Contains(seen[2].systemPrompt, "Do not call any tool again")).False()
	// For the same reason that call keeps its tools, and so does every call before
	// the reserve.
	gt.Bool(t, seen[0].toolsDisabled).False()
	gt.Bool(t, seen[1].toolsDisabled).False()
	gt.Bool(t, seen[2].toolsDisabled).False()
	// The turn itself carried the result and nothing else.
	gt.Value(t, seen[2].answered).Equal([]string{"c"})
	gt.String(t, seen[2].text).Equal("")
}

// TestTheReserveAllowsOneFinalToolCall pins the two moves the reserve is for: the
// call the task still needs, and then the result. The instruction has to flip
// between them — told "make your final tool call" a second time, a model that has
// already made it simply makes another, and the reserve is gone before anything
// is written.
func TestTheReserveAllowsOneFinalToolCall(t *testing.T) {
	tool := &recordingTool{name: "case__update_case"}
	call := func(id string) *gollem.Response {
		return callResponse(&gollem.FunctionCall{ID: id, Name: "case__update_case", Arguments: map[string]any{}})
	}
	llm, inputs := inputRecordingLLM(t, call("c1"), call("c2"), textResponse("done"))

	// Notice at 20% of 10 steps = 2 committed transitions, so the opening Generate
	// (step 0) is clean and the second one (step 2) is already in the reserve.
	rt := newRuntime(t, llm, budget.Config{
		MaxSteps: 10, MaxInputTokens: 100_000, MaxOutputTokens: 100_000, NoticeRatio: 0.2,
	}, tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "investigate"})
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	seen := inputs()
	gt.Array(t, seen).Length(3).Required()
	// The reserve's first move asks for the call and forbids writing the result.
	gt.String(t, seen[1].systemPrompt).Contains("THIS turn is your final tool call")
	gt.String(t, seen[1].systemPrompt).Contains("Do not write your result on this turn")
	// The second move carries that call's result and the opposite instruction.
	gt.String(t, seen[2].systemPrompt).Contains("The budget reserve is spent")
	gt.String(t, seen[2].systemPrompt).Contains("Do not call any tool again")
	gt.Bool(t, strings.Contains(seen[2].systemPrompt, "THIS turn is your final tool call")).False()
	// The instruction alone did not stop a model calling tools, so the second move
	// is also sent with tool calls disabled — and only the second move: the first
	// is the call the task still needs.
	gt.Bool(t, seen[0].toolsDisabled).False()
	gt.Bool(t, seen[1].toolsDisabled).False()
	gt.Bool(t, seen[2].toolsDisabled).True()
	// It carries the result and nothing else: a call answering a tool round may
	// not send a user turn as well.
	gt.Value(t, seen[2].answered).Equal([]string{"c2"})
	gt.String(t, seen[2].text).Equal("")
	// Both rounds actually ran; the reserve's instruction is not a dry run.
	gt.Array(t, tool.Calls()).Length(2)
}

// TestABudgetNoticeFromTheMoneyCeilingStillReachesTheReserve pins that a
// sub-agent out of MONEY gets the same two moves a sub-agent out of steps gets.
//
// It matters because money is the one ceiling that never stops a run: it answers
// LimitNotice however far past the allowance the run is, so if react did not act
// on that notice a spent child would simply keep generating until a token or
// step ceiling killed it — with the call its task was for still unmade. Nothing
// in react changed for this; the test fixes the behaviour so a later change to
// the notice wording or ordering cannot quietly drop it.
func TestABudgetNoticeFromTheMoneyCeilingStillReachesTheReserve(t *testing.T) {
	tool := &recordingTool{name: "knowledge__create_knowledge"}
	call := func(id string) *gollem.Response {
		return callResponse(&gollem.FunctionCall{
			ID: id, Name: "knowledge__create_knowledge", Arguments: map[string]any{},
		})
	}
	llm, inputs := inputRecordingLLM(t, call("c1"), call("c2"), textResponse("recorded"))

	// callResponse reports 5 input and 3 output tokens, so one round costs 8
	// NanoUSD at this rate: the opening Generate is clean and every later one is
	// past the allowance. The step and token ceilings are far out of reach, so
	// nothing but the money arm can produce a verdict here.
	spentAfterOneRound := func(*agentkit.Process) budget.RunLimit {
		return budget.RunLimit{Budget: 6, Rate: pricing.Rate{Input: 1, Output: 1}}
	}
	rt := newRuntimeWithSpend(t, llm, budget.Config{
		MaxSteps: 100, MaxInputTokens: 100_000, MaxOutputTokens: 100_000, NoticeRatio: 0.8,
	}, spentAfterOneRound, tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "record what you found"})
	// Out of money is not a failure. The run concludes itself.
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	seen := inputs()
	gt.Array(t, seen).Length(3).Required()
	// The limiter's own message rides in front of the instruction, so the model is
	// told which ceiling it crossed as well as what to do about it.
	gt.String(t, seen[1].systemPrompt).Contains("cost budget exhausted")
	gt.String(t, seen[1].systemPrompt).Contains("THIS turn is your final tool call")
	gt.String(t, seen[2].systemPrompt).Contains("The budget reserve is spent")
	// The write the task existed for actually happened, on both rounds.
	gt.Array(t, tool.Calls()).Length(2)
}

// TestTheReserveFirstMoveIsANudgeNotAGate pins that a model which writes its
// result instead of calling a tool ends the run there. Nothing may re-prompt it:
// there is no way to make a model call a tool, and a run with nothing left to
// call would spend the whole reserve being asked again.
func TestTheReserveFirstMoveIsANudgeNotAGate(t *testing.T) {
	tool := &recordingTool{name: "probe__ping"}
	llm, inputs := inputRecordingLLM(t,
		callResponse(&gollem.FunctionCall{ID: "c1", Name: "probe__ping", Arguments: map[string]any{}}),
		textResponse("nothing left to do; here is the result"))

	rt := newRuntime(t, llm, budget.Config{
		MaxSteps: 10, MaxInputTokens: 100_000, MaxOutputTokens: 100_000, NoticeRatio: 0.2,
	}, tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "investigate"})
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	seen := inputs()
	// Exactly two Generates: the reserve's first move was answered with text and
	// the run ended, rather than being asked for a tool call again.
	gt.Array(t, seen).Length(2).Required()
	gt.String(t, seen[1].systemPrompt).Contains("THIS turn is your final tool call")
	// The first move keeps its tools: it is asked for a call, so it cannot be the
	// call that is forbidden one.
	gt.Bool(t, seen[1].toolsDisabled).False()
	// Only the pre-reserve round ran a tool.
	gt.Array(t, tool.Calls()).Length(1)
}

// TestACallPastTheReserveBoundIsStillAnswered pins that a tool call made past
// the reserve's one round is still run and answered. Those calls are sent with
// tool calls disabled, but a provider is not bound to honour that, and the script
// here plays one that does not. Dropping the call would leave the model's
// function-call turn unanswered, which a provider rejects outright — the whole run
// then fails instead of producing the shorter result the reserve is for.
func TestACallPastTheReserveBoundIsStillAnswered(t *testing.T) {
	tool := &recordingTool{name: "probe__ping"}
	call := func(id string) *gollem.Response {
		return callResponse(&gollem.FunctionCall{ID: id, Name: "probe__ping", Arguments: map[string]any{}})
	}
	llm, inputs := inputRecordingLLM(t, call("c1"), call("c2"), call("c3"), textResponse("done"))

	rt := newRuntime(t, llm, budget.Config{
		MaxSteps: 10, MaxInputTokens: 100_000, MaxOutputTokens: 100_000, NoticeRatio: 0.2,
	}, tool)

	proc := rt.run(t, react.Input{SystemPrompt: "be helpful", Prompt: "investigate"})
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	seen := inputs()
	gt.Array(t, seen).Length(4).Required()
	// The over-budget round was told to stop, sent with tool calls disabled, and
	// answered with a call anyway; both the call it made and the one before it were
	// answered.
	gt.String(t, seen[2].systemPrompt).Contains("Do not call any tool again")
	gt.Bool(t, seen[2].toolsDisabled).True()
	gt.Value(t, seen[2].answered).Equal([]string{"c2"})
	gt.String(t, seen[3].systemPrompt).Contains("Do not call any tool again")
	gt.Bool(t, seen[3].toolsDisabled).True()
	gt.Value(t, seen[3].answered).Equal([]string{"c3"})
	gt.Array(t, tool.Calls()).Length(3)
}

func TestRegisterRequiresALimiter(t *testing.T) {
	_, err := react.Register(agentkit.NewRegistry(), "react-test", 1, nil)
	gt.Error(t, err).Is(agentkit.ErrInvalidAgentDef)
}

// TestInitRejectsEmptyPrompts pins that a host wiring bug fails at Spawn, where
// the caller sees it, rather than as an agent that runs with no instructions.
func TestInitRejectsEmptyPrompts(t *testing.T) {
	llm, _ := scriptedLLM(t)
	rt := newRuntime(t, llm, generousBudget())
	ctx := context.Background()

	testCases := map[string]react.Input{
		"no system prompt": {Prompt: "hello"},
		"no prompt":        {SystemPrompt: "be helpful"},
		"neither":          {},
	}
	for name, in := range testCases {
		t.Run(name, func(t *testing.T) {
			_, err := rt.agent.Spawn(ctx, rt.kernel, in)
			gt.Value(t, err).NotNil()
		})
	}
}

func TestDecodeOutput(t *testing.T) {
	t.Run("round trips", func(t *testing.T) {
		out, err := react.DecodeOutput([]byte(`{"texts":["a","b"]}`))
		gt.NoError(t, err).Required()
		gt.Array(t, out.Texts).Equal([]string{"a", "b"})
	})
	t.Run("reports malformed bytes", func(t *testing.T) {
		_, err := react.DecodeOutput([]byte("not json"))
		gt.Value(t, err).NotNil()
	})
}
