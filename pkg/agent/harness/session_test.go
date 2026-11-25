package harness_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type simulationTool struct{}

func (simulationTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{Name: "simulate", Parameters: map[string]*gollem.Parameter{
		"text": {Type: gollem.TypeString, Required: true},
	}}
}

func (simulationTool) Run(context.Context, map[string]any) (map[string]any, error) {
	return nil, errors.New("the evaluation must execute the requested tool")
}

type callResult struct {
	text   string
	failed bool
	err    error
}

func callMCP(ctx context.Context, c *mcp.ClientSession, name string, args map[string]any) callResult {
	result, err := c.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return callResult{err: err}
	}
	var text string
	if len(result.Content) == 1 {
		if content, ok := result.Content[0].(*mcp.TextContent); ok {
			text = content.Text
		}
	}
	return callResult{text: text, failed: result.IsError}
}

func requireCall(t *testing.T, ctx context.Context, c *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res := callMCP(ctx, c, name, args)
	if res.err != nil || res.failed {
		t.Fatalf("%s failed: %v, %s", name, res.err, res.text)
	}
	return res.text
}

func requireRefusal(t *testing.T, ctx context.Context, c *mcp.ClientSession, name string, args map[string]any) {
	t.Helper()
	res := callMCP(ctx, c, name, args)
	if res.err != nil || !res.failed {
		t.Fatalf("%s should be refused by the MCP tool: %+v", name, res)
	}
}

func decode[T any](t *testing.T, raw string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("invalid tool response: %v", err)
	}
	return value
}

type dispatch struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Idle bool   `json:"idle"`
	Done bool   `json:"done"`
}

func take(t *testing.T, ctx context.Context, c *mcp.ClientSession, role string) dispatch {
	t.Helper()
	return decode[dispatch](t, requireCall(t, ctx, c, harness.SessionToolNext, map[string]any{"wait_seconds": 2, "role": role}))
}

type outcome struct {
	result *harness.Result
	err    error
}

func run(s *harness.Session, ctx context.Context, request harness.Request) <-chan outcome {
	ch := make(chan outcome, 1)
	go func() {
		result, err := s.Run(ctx, request)
		ch <- outcome{result: result, err: err}
	}()
	return ch
}

func receive(t *testing.T, ctx context.Context, ch <-chan outcome) outcome {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-ctx.Done():
		t.Fatalf("run did not return: %v", ctx.Err())
		return outcome{}
	}
}

type operation struct {
	ID     string `json:"call_id"`
	Done   bool   `json:"done"`
	Failed bool   `json:"failed"`
	Text   string `json:"text"`
}

func TestSameMCPWorkerCanAnswerNestedToolSimulation(t *testing.T) {
	s, c := harness.ConnectInMemoryForTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	ctx = harness.WithRunner(ctx, s)
	var calls atomic.Int32

	parent := run(s, ctx, harness.Request{
		SystemPrompt: "synthetic instructions", Prompt: "synthetic task",
		OutputSchema: json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"integer"}}}`),
		Tools:        []gollem.Tool{simulationTool{}},
		CallTool: func(ctx context.Context, call gollem.FunctionCall) (map[string]any, error) {
			calls.Add(1)
			if call.ID == "" || call.Name != "simulate" || harness.FromContext(ctx) != s {
				return nil, errors.New("tool invocation lost its evaluation context")
			}
			q := harness.Completer{Runner: s, Role: harness.RoleToolSim}
			value, err := q.Complete(ctx, "simulation instructions", call.Arguments["text"].(string), nil)
			return map[string]any{"value": value}, err
		},
	})
	request := take(t, ctx, c, harness.RoleAgent)
	identifier, err := hex.DecodeString(request.ID)
	if err != nil || len(identifier) < 16 || request.Role != harness.RoleAgent {
		t.Fatalf("request claim is missing a random ID: %+v", request)
	}
	brief := decode[struct {
		SystemPrompt string `json:"system_prompt"`
		Prompt       string `json:"prompt"`
		Tools        []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}](t, requireCall(t, ctx, c, harness.SessionToolGet, map[string]any{"id": request.ID, "json": true}))
	if brief.SystemPrompt != "synthetic instructions" || brief.Prompt != "synthetic task" ||
		len(brief.Tools) != 1 || brief.Tools[0].Name != "simulate" {
		t.Fatal("briefing differs from the published request")
	}

	callArgs := map[string]any{"id": request.ID, "tool": "simulate", "arguments": map[string]any{"text": "synthetic simulator task"}}
	requireRefusal(t, ctx, c, harness.SessionToolCall, map[string]any{"id": request.ID, "tool": "unknown"})
	op := decode[operation](t, requireCall(t, ctx, c, harness.SessionToolCall, callArgs))
	if op.ID == "" || op.Done {
		t.Fatalf("tool start did not return a pending operation: %+v", op)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			result := callMCP(ctx, c, harness.SessionToolCall, callArgs)
			if result.err != nil || !result.failed {
				t.Errorf("a duplicate in-flight call was not refused")
			}
		})
	}
	wg.Wait()

	submit := func(id, answer string) string {
		t.Helper()
		return requireCall(t, ctx, c, harness.SessionToolSubmit, map[string]any{"id": id, "answer": answer})
	}
	requireRefusal(t, ctx, c, harness.SessionToolSubmit, map[string]any{"id": request.ID, "answer": `{"answer":1}`})
	child := take(t, ctx, c, harness.RoleToolSim)
	if child.ID == "" || child.ID == request.ID || child.Role != harness.RoleToolSim {
		t.Fatalf("failed to claim nested simulator request: %+v", child)
	}
	submit(child.ID, "synthetic simulator response")
	poll := map[string]any{"id": request.ID, "call_id": op.ID, "wait_seconds": 2}
	tool := decode[operation](t, requireCall(t, ctx, c, harness.SessionToolPoll, poll))
	if !tool.Done || tool.Failed || tool.Text != `{"value":"synthetic simulator response"}` {
		t.Fatalf("unexpected tool result: %+v", tool)
	}
	if repeated := decode[operation](t, requireCall(t, ctx, c, harness.SessionToolPoll, poll)); repeated != tool {
		t.Fatal("polling the same operation should be idempotent")
	}
	requireRefusal(t, ctx, c, harness.SessionToolSubmit, map[string]any{"id": request.ID, "answer": `{"answer":"wrong type"}`})
	submit(request.ID, `{"answer":1}`)
	result := receive(t, ctx, parent)
	if result.err != nil || result.result.Text != `{"answer":1}` || result.result.ToolCalls != 1 || calls.Load() != 1 {
		t.Fatalf("unexpected outcome: %+v, %d callback calls", result, calls.Load())
	}
	requireRefusal(t, ctx, c, harness.SessionToolGet, map[string]any{"id": request.ID})
}

func TestCloseCancelsRunningAndWaitingRequests(t *testing.T) {
	s, c := harness.ConnectInMemoryForTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	started := make(chan struct{})
	claimed := run(s, ctx, harness.Request{
		Tools: []gollem.Tool{simulationTool{}},
		CallTool: func(ctx context.Context, _ gollem.FunctionCall) (map[string]any, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	who := take(t, ctx, c, harness.RoleAgent)
	requireCall(t, ctx, c, harness.SessionToolCall, map[string]any{"id": who.ID, "tool": "simulate"})

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatalf("tool never started: %v", ctx.Err())
	}
	waiting := run(s, ctx, harness.Request{Role: harness.RoleJudge})
	harness.StopForTest(s)

	result := receive(t, ctx, claimed)
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("active callback did not see cancellation: %v", result.err)
	}
	result = receive(t, ctx, waiting)
	if !errors.Is(result.err, context.Canceled) && !errors.Is(result.err, harness.ErrSessionDone) {
		t.Fatalf("idle request did not see closure: %v", result.err)
	}
	if !take(t, ctx, c, "").Done {
		t.Fatal("closed session did not tell workers to stop")
	}
}

func TestCancelRetiresUnclaimedRequest(t *testing.T) {
	s, c := harness.ConnectInMemoryForTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	runCtx, cancelRun := context.WithCancel(ctx)
	finished := run(s, runCtx, harness.Request{Prompt: "synthetic task"})
	cancelRun()
	if result := receive(t, ctx, finished); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("expected canceled Run: %v", result.err)
	}
	if !decode[dispatch](t, requireCall(t, ctx, c, harness.SessionToolNext, map[string]any{})).Idle {
		t.Fatal("a canceled request was still available")
	}
	if harness.FromContext(ctx) != nil || harness.FromContext(harness.WithRunner(ctx, nil)) != nil {
		t.Fatal("runner context readback should allow an unconfigured runner")
	}
}

func TestToolBudgetStopsRequest(t *testing.T) {
	s, c := harness.ConnectInMemoryForTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	finished := run(s, ctx, harness.Request{
		Tools: []gollem.Tool{simulationTool{}},
		CallTool: func(context.Context, gollem.FunctionCall) (map[string]any, error) {
			return nil, agentkit.ErrLimitExceeded
		},
	})
	request := take(t, ctx, c, "")
	requireCall(t, ctx, c, harness.SessionToolCall, map[string]any{"id": request.ID, "tool": "simulate"})
	if result := receive(t, ctx, finished); !errors.Is(result.err, agentkit.ErrLimitExceeded) {
		t.Fatalf("expected a budget error, got %v", result.err)
	}
	requireRefusal(t, ctx, c, harness.SessionToolSubmit, map[string]any{"id": request.ID, "answer": "late answer"})
}

func TestEndpointURLRejectsNonLocalAndAmbiguousURLs(t *testing.T) {
	token := strings.Repeat("a", 48)
	for _, target := range []string{
		"http://localhost:123/" + token,
		"http://127.0.0.1:0/" + token,
		"https://127.0.0.1:123/" + token,
		"http://user@127.0.0.1:123/" + token,
		"http://127.0.0.1:123/" + token + "?",
		"http://127.0.0.1:123/" + token + "#fragment",
		"http://127.0.0.1:123/unknown",
	} {
		if harness.CheckLoopbackForTest(target) == nil {
			t.Fatal("accepted an invalid endpoint")
		}
	}
	if err := harness.CheckLoopbackForTest("http://127.0.0.1:123/" + token); err != nil {
		t.Fatalf("refused a valid local endpoint: %v", err)
	}
}

func TestWorkerRecoversToolCallAfterLosingOperationResponses(t *testing.T) {
	s, c := harness.ConnectInMemoryForTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var effects atomic.Int32
	finished := run(s, ctx, harness.Request{
		Tools: []gollem.Tool{simulationTool{}},
		CallTool: func(context.Context, gollem.FunctionCall) (map[string]any, error) {
			effects.Add(1)
			return map[string]any{"value": "synthetic tool result"}, nil
		},
	})
	request := take(t, ctx, c, "")
	args := map[string]any{"id": request.ID, "tool": "simulate"}

	// The worker lost the call_tool response and has no operation ID.
	requireCall(t, ctx, c, harness.SessionToolCall, args)
	requireRefusal(t, ctx, c, harness.SessionToolCall, args)
	recoverID := func() string {
		t.Helper()
		brief := decode[struct {
			PendingCallID string `json:"pending_call_id"`
		}](t, requireCall(t, ctx, c, harness.SessionToolGet, map[string]any{"id": request.ID, "json": true}))
		if brief.PendingCallID == "" {
			t.Fatal("request briefing did not recover the tool call ID")
		}
		return brief.PendingCallID
	}
	pendingID := recoverID()
	poll := map[string]any{"id": request.ID, "call_id": pendingID, "wait_seconds": 2}
	markdown := requireCall(t, ctx, c, harness.SessionToolGet, map[string]any{"id": request.ID})
	if !strings.Contains(markdown, pendingID) {
		t.Fatal("Markdown briefing omitted the tool call ID")
	}

	// The first poll marked the result as read, but its response was also lost.
	requireCall(t, ctx, c, harness.SessionToolPoll, poll)
	if recovered := recoverID(); recovered != pendingID {
		t.Fatal("the result could not be recovered after polling")
	}
	result := decode[operation](t, requireCall(t, ctx, c, harness.SessionToolPoll, poll))
	if !result.Done || result.Failed || result.Text != `{"value":"synthetic tool result"}` {
		t.Fatalf("unexpected recovered tool result: %+v", result)
	}
	requireCall(t, ctx, c, harness.SessionToolSubmit, map[string]any{"id": request.ID, "answer": "synthetic answer"})
	if outcome := receive(t, ctx, finished); outcome.err != nil || outcome.result.ToolCalls != 1 || effects.Load() != 1 {
		t.Fatalf("expected exactly one tool effect: %d, error=%v", effects.Load(), outcome.err)
	}
}
