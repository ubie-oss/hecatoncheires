package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	SessionToolNext   = "next_request"
	SessionToolGet    = "get_request"
	SessionToolCall   = "call_tool"
	SessionToolPoll   = "poll_tool"
	SessionToolSubmit = "submit_answer"
	SessionToolStatus = "status"
)

type nextInput struct {
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"Seconds to wait for a request (0-55). Default 0"`
	Role        string `json:"role,omitempty" jsonschema:"Only take a request with this role, if set"`
}

type getInput struct {
	JSON bool   `json:"json,omitempty" jsonschema:"Return the briefing as JSON text"`
	ID   string `json:"id" jsonschema:"The request ID from next_request"`
	CLI  string `json:"cli,omitempty" jsonschema:"Command prefix for the shell-side briefing, filled in by the harness command"`
}

type callInput struct {
	ID        string         `json:"id" jsonschema:"The request ID"`
	Tool      string         `json:"tool" jsonschema:"Name of a tool from the request briefing"`
	Arguments map[string]any `json:"arguments,omitempty" jsonschema:"The tool arguments"`
}

type pollInput struct {
	ID          string `json:"id" jsonschema:"The request ID"`
	CallID      string `json:"call_id" jsonschema:"The operation ID returned by call_tool"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"Seconds to wait for completion (0-55). Default 0"`
}

type submitInput struct {
	ID     string `json:"id" jsonschema:"The request ID"`
	Answer string `json:"answer" jsonschema:"The final answer, encoded as JSON text when there is an output schema"`
}

type toolReply struct {
	CallID string `json:"call_id"`
	Done   bool   `json:"done"`
	Failed bool   `json:"failed,omitempty"`
	Text   string `json:"text,omitempty"`
}

func (s *Session) mcpServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "hecatoncheires-eval", Version: "1"}, nil)
	addSessionTool(server, SessionToolNext, "Take the next evaluation request.", func(ctx context.Context, in nextInput) (any, error) {
		return s.next(ctx, in), nil
	})
	addSessionTool(server, SessionToolGet, "Read a request's instructions, tool schemas, answer format and latest tool call ID, if any.", func(_ context.Context, in getInput) (string, error) {
		return s.brief(in)
	})
	addSessionTool(server, SessionToolCall, "Start one tool call for a request. Poll the returned call_id before starting another call or submitting.", func(_ context.Context, in callInput) (toolReply, error) {
		return s.startTool(in)
	})
	addSessionTool(server, SessionToolPoll, "Read the result of a tool call once available. While a simulator call is pending, a worker may take another request.", s.pollTool)
	addSessionTool(server, SessionToolSubmit, "Submit the final answer. An invalid answer can be corrected and resubmitted.", func(_ context.Context, in submitInput) (string, error) {
		return "accepted", s.submit(in)
	})
	addSessionTool(server, SessionToolStatus, "Show counts of waiting, in-progress and retired requests.", func(context.Context, struct{}) (any, error) {
		return s.status(), nil
	})
	return server
}

func addSessionTool[In, Out any](server *mcp.Server, name, description string, handle func(context.Context, In) (Out, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := handle(ctx, in)
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
		}
		if text, ok := any(out).(string); ok {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		}
		body, err := json.Marshal(out)
		if err != nil {
			return nil, nil, goerr.Wrap(err, "harness: encode tool response")
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
	})
}

func waitDuration(seconds int) time.Duration {
	return time.Duration(min(max(seconds, 0), 55)) * time.Second
}

func (s *Session) next(ctx context.Context, in nextInput) map[string]any {
	duration := waitDuration(in.WaitSeconds)
	timer := time.NewTimer(duration)
	defer timer.Stop()

	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return map[string]any{"done": true}
		}
		if ctx.Err() != nil {
			s.mu.Unlock()
			return map[string]any{"idle": true}
		}
		for i, t := range s.queue {
			if t.parent.Err() != nil || !time.Now().Before(t.queueUntil) || (in.Role != "" && in.Role != t.req.role()) {
				continue
			}
			s.queue = slices.Delete(s.queue, i, i+1)
			t.work, t.stopWork = context.WithTimeout(t.parent, RunTimeout)
			t.signal()
			s.mu.Unlock()
			return map[string]any{"id": t.id, "role": t.req.role(), "label": t.req.Label, "tools": len(t.tools)}
		}
		wake := s.wake
		s.mu.Unlock()

		if duration == 0 {
			return map[string]any{"idle": true}
		}
		select {
		case <-wake:
		case <-timer.C:
			return map[string]any{"idle": true}
		case <-ctx.Done():
			return map[string]any{"idle": true}
		}
	}
}

// activeTicket looks up a live, claimed request. Callers hold s.mu.
func (s *Session) activeTicket(id string) (*ticket, error) {
	t := s.tickets[id]
	if t == nil || t.work == nil || t.work.Err() != nil || t.submitted {
		return nil, goerr.New("harness: no active request for that ID")
	}
	return t, nil
}

func (s *Session) startTool(in callInput) (toolReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.activeTicket(in.ID)
	if err != nil {
		return toolReply{}, err
	}
	if t.pending != nil && !t.pending.read {
		return toolReply{}, goerr.New("harness: poll the previous tool call before starting another")
	}
	if !slices.ContainsFunc(t.tools, func(tool briefTool) bool {
		return tool.Name == in.Tool
	}) {
		return toolReply{}, goerr.New("harness: tool is not available in this request")
	}
	id, err := randomToken()
	if err != nil {
		return toolReply{}, err
	}
	if in.Arguments == nil {
		in.Arguments = make(map[string]any)
	}
	t.pending = &toolOperation{id: id, done: make(chan struct{}), function: gollem.FunctionCall{
		ID: id, Name: in.Tool, Arguments: in.Arguments,
	}}
	t.signal()
	return toolReply{CallID: id}, nil
}

func (s *Session) pollTool(ctx context.Context, in pollInput) (toolReply, error) {
	s.mu.Lock()
	t, err := s.activeTicket(in.ID)
	if err != nil {
		s.mu.Unlock()
		return toolReply{}, err
	}
	op := t.pending
	work := t.work
	s.mu.Unlock()
	if op == nil || op.id != in.CallID {
		return toolReply{}, goerr.New("harness: no tool call for that ID")
	}
	timer := time.NewTimer(waitDuration(in.WaitSeconds))
	defer timer.Stop()
	select {
	case <-op.done:
	case <-ctx.Done():
		return toolReply{}, ctx.Err()
	case <-work.Done():
	case <-timer.C:
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if op.result.Done {
		op.read = true
		reply := op.result
		reply.CallID = op.id
		return reply, nil
	}
	if err := work.Err(); err != nil {
		return toolReply{}, goerr.Wrap(err, "harness: request expired or was canceled")
	}
	return toolReply{CallID: op.id}, nil
}

func (s *Session) submit(in submitInput) error {
	s.mu.Lock()
	t, err := s.activeTicket(in.ID)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if strings.TrimSpace(in.Answer) == "" {
		return goerr.New("harness: the answer is empty")
	}
	if t.validation != nil {
		var answer any
		if err := json.Unmarshal([]byte(in.Answer), &answer); err != nil {
			return goerr.Wrap(err, "harness: the answer must be valid JSON")
		}
		if err := t.validation.Validate(answer); err != nil {
			return goerr.Wrap(err, "harness: the answer does not match the output schema")
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.activeTicket(in.ID); err != nil {
		return err
	}
	if t.pending != nil && !t.pending.read {
		return goerr.New("harness: poll the previous tool call before submitting")
	}
	t.answer = in.Answer
	t.submitted = true
	t.signal()
	return nil
}

func (s *Session) status() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]int{
		"waiting": len(s.queue), "in_progress": len(s.tickets) - len(s.queue), "finished": s.finished,
	}
}

type briefTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type briefing struct {
	ID            string          `json:"id"`
	Role          string          `json:"role"`
	Label         string          `json:"label"`
	SystemPrompt  string          `json:"system_prompt"`
	Prompt        string          `json:"prompt"`
	Tools         []briefTool     `json:"tools"`
	OutputSchema  json.RawMessage `json:"output_schema"`
	PendingCallID string          `json:"pending_call_id,omitempty"`
}

func (s *Session) brief(in getInput) (string, error) {
	s.mu.Lock()
	t, err := s.activeTicket(in.ID)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	var pendingID string
	if t.pending != nil {
		pendingID = t.pending.id
	}
	s.mu.Unlock()
	req := t.req
	tools := t.tools
	if in.JSON {
		data, err := json.Marshal(briefing{ID: t.id, Role: req.role(), Label: req.Label, SystemPrompt: req.SystemPrompt,
			Prompt: req.Prompt, Tools: tools, OutputSchema: req.OutputSchema, PendingCallID: pendingID})
		if err != nil {
			return "", goerr.Wrap(err, "harness: encode request")
		}
		return string(data), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Request %s (%s)\n\n", t.id, req.role())
	b.WriteString("Use the instructions as the system prompt for this request. Answer the task using only its listed tools and information.\n\n")
	if len(tools) > 0 {
		if in.CLI == "" {
			fmt.Fprintf(&b, "Start a tool with `%s` {id: %q, tool, arguments}. Read the result with `%s` {id, call_id}.\n", SessionToolCall, t.id, SessionToolPoll)
		} else {
			fmt.Fprintf(&b, "Invoke a tool with `%s call --args '<JSON object>' %s <tool>`. Use `--async` and `poll` if you also need to handle simulator requests.\n", in.CLI, t.id)
		}
	}
	if pendingID != "" {
		if in.CLI == "" {
			fmt.Fprintf(&b, "Recover the latest tool result with `%s` {id: %q, call_id: %q}.\n", SessionToolPoll, t.id, pendingID)
		} else {
			fmt.Fprintf(&b, "Recover the latest tool result with `%s poll %s %s`.\n", in.CLI, t.id, pendingID)
		}
	}
	if in.CLI == "" {
		fmt.Fprintf(&b, "Submit with `%s` {id: %q, answer}.\n", SessionToolSubmit, t.id)
	} else {
		fmt.Fprintf(&b, "Submit with `%s submit --file <path> %s`.\n", in.CLI, t.id)
	}
	fmt.Fprintf(&b, "\n## Instructions\n\n%s\n\n## Task\n\n%s\n", req.SystemPrompt, req.Prompt)
	if len(tools) > 0 {
		b.WriteString("\n## Tools\n")
		for _, tool := range tools {
			raw, err := json.Marshal(tool.InputSchema)
			if err != nil {
				return "", goerr.Wrap(err, "harness: encode tool schema", goerr.V("tool", tool.Name))
			}
			fmt.Fprintf(&b, "\n### %s\n\n%s\n\n```json\n%s\n```\n", tool.Name, tool.Description, raw)
		}
	}
	if len(req.OutputSchema) > 0 {
		fmt.Fprintf(&b, "\n## Output schema\n\nSubmit a JSON value matching this schema.\n\n```json\n%s\n```\n", req.OutputSchema)
	}
	return b.String(), nil
}
