package harness

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/m-mizutani/goerr/v2"
)

const (
	// SessionQueueTimeout bounds the time a request may wait for a worker.
	SessionQueueTimeout = 30 * time.Minute
	// RunTimeout bounds the time a worker has to answer after taking a request.
	RunTimeout = 15 * time.Minute
)

var _ Runner = (*Session)(nil)

// Session publishes requests for local CLI and MCP workers.
type Session struct {
	endpoint *sessionEndpoint
	closing  sync.Once
	closeErr error

	mu       sync.Mutex
	queue    []*ticket
	tickets  map[string]*ticket
	wake     chan struct{}
	finished int
	closed   bool
}

type ticket struct {
	id         string
	req        Request
	tools      []briefTool
	validation *jsonschema.Resolved
	parent     context.Context
	cancel     context.CancelFunc
	queueUntil time.Time

	// Mutable fields are guarded by Session.mu.
	wake      chan struct{}
	work      context.Context
	stopWork  context.CancelFunc
	pending   *toolOperation
	answer    string
	submitted bool
}

type toolOperation struct {
	id       string
	function gollem.FunctionCall
	done     chan struct{}
	result   toolReply
	read     bool
}

// NewSession starts a local MCP server and publishes its endpoint in dir.
func NewSession(dir string) (*Session, error) {
	s := &Session{tickets: make(map[string]*ticket), wake: make(chan struct{})}
	endpoint, err := openSessionEndpoint(dir, s)
	if err != nil {
		return nil, err
	}
	s.endpoint = endpoint
	return s, nil
}

// Close cancels outstanding evaluations and releases the rendezvous.
func (s *Session) Close() error {
	s.closing.Do(func() {
		s.stop()
		s.closeErr = s.endpoint.close()
	})
	return s.closeErr
}

func (s *Session) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for _, t := range s.tickets {
		t.cancel()
	}
	s.signal()
}

// MaxDuration is the maximum wait time for one request, including its time in the queue.
func (s *Session) MaxDuration() time.Duration { return SessionQueueTimeout + RunTimeout }

// signal wakes workers waiting for a newly queued request or session closure.
func (s *Session) signal() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// signal wakes the run after a worker takes the request, adds a tool call, or submits an answer.
func (t *ticket) signal() {
	close(t.wake)
	t.wake = make(chan struct{})
}

// Run publishes a request and executes its accepted tool calls in order, within
// the evaluation's context. Each request has its own execution loop so a tool
// can wait for another request (such as a simulator) on the same session.
func (s *Session) Run(ctx context.Context, req Request) (*Result, error) {
	if len(req.Tools) != 0 && req.CallTool == nil {
		return nil, goerr.New("harness: a tool caller is required")
	}
	validation, err := resolveSchema(req.OutputSchema)
	if err != nil {
		return nil, err
	}
	id, err := randomToken()
	if err != nil {
		return nil, err
	}

	tools := make([]briefTool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		if tool != nil {
			spec := tool.Spec()
			tools = append(tools, briefTool{Name: spec.Name, Description: spec.Description, InputSchema: toolInputSchema(spec)})
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := time.Now()
	t := &ticket{
		id: id, req: req, tools: tools, validation: validation, parent: ctx, cancel: cancel,
		wake: make(chan struct{}), queueUntil: started.Add(SessionQueueTimeout),
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrSessionDone
	}
	s.tickets[id] = t
	s.queue = append(s.queue, t)
	s.signal()
	s.mu.Unlock()
	defer s.retire(t)

	result, err := s.process(t)
	if err != nil {
		return nil, err
	}
	result.Duration = time.Since(started)
	return result, nil
}

func (s *Session) retire(t *ticket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.stopWork != nil {
		t.stopWork()
	}
	delete(s.tickets, t.id)
	s.queue = slices.DeleteFunc(s.queue, func(waiting *ticket) bool { return waiting == t })
	s.finished++
}

func (s *Session) process(t *ticket) (*Result, error) {
	queueTimeout := time.NewTimer(time.Until(t.queueUntil))
	defer queueTimeout.Stop()
	s.mu.Lock()
	for t.work == nil && t.parent.Err() == nil {
		wake := t.wake
		s.mu.Unlock()
		select {
		case <-t.parent.Done():
		case <-wake:
		case <-queueTimeout.C:
			s.mu.Lock()
			claimed := t.work != nil
			s.mu.Unlock()
			if !claimed {
				return nil, goerr.Wrap(context.DeadlineExceeded, "harness: no worker took the request in time")
			}
		}
		s.mu.Lock()
	}
	work := t.work
	s.mu.Unlock()
	if err := t.parent.Err(); err != nil {
		return nil, goerr.Wrap(err, "harness: request canceled")
	}
	return s.processClaimed(t, work)
}

func (s *Session) processClaimed(t *ticket, ctx context.Context) (*Result, error) {
	var last *toolOperation
	calls := 0
	for {
		s.mu.Lock()
		switch {
		case ctx.Err() != nil:
			s.mu.Unlock()
			return nil, goerr.Wrap(ctx.Err(), "harness: request expired or was canceled")
		case t.submitted:
			answer := t.answer
			s.mu.Unlock()
			return &Result{Text: answer, ToolCalls: calls}, nil
		case t.pending != nil && t.pending != last:
			last = t.pending
			s.mu.Unlock()

			calls++
			reply, err := invokeTool(ctx, t.req, last.function)
			s.mu.Lock()
			last.result = reply
			close(last.done)
			s.mu.Unlock()
			if err != nil {
				return nil, err
			}
		default:
			wake := t.wake
			s.mu.Unlock()
			select {
			case <-ctx.Done():
			case <-wake:
			}
		}
	}
}

func invokeTool(ctx context.Context, req Request, call gollem.FunctionCall) (toolReply, error) {
	reply := toolReply{Done: true}
	if err := ctx.Err(); err != nil {
		return reply, goerr.Wrap(err, "harness: request expired or was canceled")
	}
	result, err := req.CallTool(ctx, call)
	if err != nil {
		if errors.Is(err, agentkit.ErrLimitExceeded) {
			reply.Failed = true
			reply.Text = "the run's budget is exhausted"
			return reply, goerr.Wrap(err, "harness: tool call refused by the budget", goerr.V("tool", call.Name))
		}
		reply.Failed = true
		reply.Text = err.Error()
		return reply, nil
	}
	body, err := json.Marshal(result)
	if err != nil {
		reply.Failed = true
		reply.Text = "the tool result could not be encoded"
		return reply, nil
	}
	reply.Text = string(body)
	return reply, nil
}

func resolveSchema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, goerr.Wrap(err, "harness: decode output schema")
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, goerr.Wrap(err, "harness: resolve output schema")
	}
	return resolved, nil
}
