package threadcase_test

import (
	"context"
	"regexp"
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
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/agentarchive"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/threadcase"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// testModelPolicy is the one-model policy these turns are priced at: $1 / $5 per
// MTok, which is what the run-record assertions expect.
func testModelPolicy(t *testing.T) agentkernel.ModelPolicy {
	t.Helper()
	p, err := agentkernel.NewModelPolicy(agentkernel.ModelPolicyInput{
		Defs: []agentkernel.ModelDef{{
			Ref:      "test",
			Provider: agentkernel.ProviderClaude,
			Model:    "test-model",
			Rate:     pricing.Rate{Input: 1000, Output: 5000},
		}},
		DefaultRef:    "test",
		DefaultBudget: pricing.FromUSD(100),
	})
	gt.NoError(t, err).Required()
	return p
}

// durableCall records one Host call a finished turn made, so a test asserts what
// the user and the case actually got rather than that no error was returned.
type durableCall struct {
	Kind     string // "mention" | "create" | "question" | "fallback"
	Target   threadcase.Target
	Decision *threadcase.Decision
	Create   threadcase.CreatePayload
	Question threadcase.QuestionPayload
	Reason   string
}

type durableHost struct {
	mu    sync.Mutex
	calls []durableCall
	// createErr, when set, is what CreateCase returns — the persistence failure
	// that must be surfaced rather than fed back to the model.
	createErr error
	// questionErr, when set, is what AskQuestion returns — the Slack post or the
	// Session write failing, after which the thread must NOT be recorded as waiting
	// on a form that does not exist.
	questionErr error
}

func (h *durableHost) ApplyMention(_ context.Context, target threadcase.Target, d *threadcase.Decision) error {
	h.record(durableCall{Kind: "mention", Target: target, Decision: d})
	return nil
}

func (h *durableHost) CreateCase(_ context.Context, target threadcase.Target, p threadcase.CreatePayload) error {
	h.record(durableCall{Kind: "create", Target: target, Create: p})
	return h.createErr
}

func (h *durableHost) AskQuestion(_ context.Context, target threadcase.Target, q threadcase.QuestionPayload) error {
	h.record(durableCall{Kind: "question", Target: target, Question: q})
	return h.questionErr
}

func (h *durableHost) ReportFallback(_ context.Context, target threadcase.Target, reason string) error {
	h.record(durableCall{Kind: "fallback", Target: target, Reason: reason})
	return nil
}

func (h *durableHost) record(c durableCall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, c)
}

func (h *durableHost) Calls() []durableCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]durableCall, len(h.calls))
	copy(out, h.calls)
	return out
}

// kinds returns the Host calls in order, which is the turn's observable outcome.
func (h *durableHost) kinds() []string {
	calls := h.Calls()
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Kind
	}
	return out
}

// durableLLM answers with replies[i] on the i-th Generate. An extra call fails
// rather than repeating the last answer, so a strategy that looped is caught.
func durableLLM(replies ...string) gollem.LLMClient {
	client, _ := recordingDurableLLM(replies...)
	return client
}

// generateCall is one Generate as the model saw it: the system prompt its
// session was opened with, and the text of the user input it was sent.
type generateCall struct {
	SystemPrompt string
	Input        string
}

// recordingDurableLLM is durableLLM plus what each call was made with, in call
// order. This is where the host's per-run decisions actually land — the language
// directive and the turn's current time above all — so it is the observable a
// test asserting on them has to read.
func recordingDurableLLM(replies ...string) (gollem.LLMClient, func() []generateCall) {
	var n atomic.Int32
	var mu sync.Mutex
	var calls []generateCall
	client := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
			// The system prompt is a session-level setting, so it is read here and
			// attributed to the calls this session makes.
			cfg := gollem.NewSessionConfig(opts...)
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, input []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					var b strings.Builder
					for _, in := range input {
						if txt, ok := in.(gollem.Text); ok {
							b.WriteString(string(txt))
						}
					}
					mu.Lock()
					calls = append(calls, generateCall{SystemPrompt: cfg.SystemPrompt(), Input: b.String()})
					mu.Unlock()
					i := int(n.Add(1)) - 1
					if i >= len(replies) {
						return nil, goerr.New("unexpected extra generate call", goerr.V("call_index", i))
					}
					return &gollem.Response{Texts: []string{replies[i]}, InputToken: 5, OutputToken: 3}, nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
				},
			}, nil
		},
	}
	return client, func() []generateCall {
		mu.Lock()
		defer mu.Unlock()
		out := make([]generateCall, len(calls))
		copy(out, calls)
		return out
	}
}

// rfc3339Pattern matches the RFC3339 UTC instant the prompts render.
var rfc3339Pattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)

// rfc3339In returns the single RFC3339 instant in s, failing the test when there
// is none — an assertion comparing two absent values would otherwise pass.
func rfc3339In(t *testing.T, s string) string {
	t.Helper()
	found := rfc3339Pattern.FindString(s)
	gt.String(t, found).NotEqual("")
	return found
}

func durableFailingLLM() gollem.LLMClient {
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					return nil, goerr.New("the model is unreachable")
				},
				HistoryFunc: func() (*gollem.History, error) {
					return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
				},
			}, nil
		},
	}
}

type durableHarness struct {
	agent   *threadcase.Durable
	host    *durableHost
	repo    *memory.Memory
	kernel  *agentkit.Kernel
	locator agentkernel.Locator
}

// newDurableHarness wires both thread-mode agents onto a real Kernel with an
// in-process Process store. The tool factory returns nothing: which tools an agent
// gets is pkg/agent/kernel's contract, and these tests are about this host's own —
// spawn, turn lock, redelivery, and what the completion handler does.
func newDurableHarness(t *testing.T, llm gollem.LLMClient) *durableHarness {
	t.Helper()

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(newThreadWorkspace())

	procRepo := agentprocmemory.New()
	locator, err := agentkernel.NewLocator(procRepo)
	gt.NoError(t, err).Required()

	host := &durableHost{}
	tc, err := threadcase.NewDurable(repo, registry, host, locator, testModelPolicy(t))
	gt.NoError(t, err).Required()

	store := agentarchive.NewMemoryHistoryStore()
	cfg := budget.Config{MaxSteps: 64, MaxInputTokens: 100_000, MaxOutputTokens: 100_000, NoticeRatio: 0.8}
	reg := agentkit.NewRegistry()
	taskAgent, err := react.Register(reg, agentkernel.AgentTask, 1, cfg.Limiter(testModelPolicy(t).Resolve),
		agentkit.WithHistoryStore[react.Output](store))
	gt.NoError(t, err).Required()
	gt.NoError(t, tc.Register(reg, taskAgent, nil, cfg.Limiter(testModelPolicy(t).Resolve), store)).Required()

	k, err := agentkit.New(procRepo, llm, reg,
		agentkit.WithToolFactory(func(context.Context, *agentkit.Process) ([]gollem.Tool, error) {
			return nil, nil
		}))
	gt.NoError(t, err).Required()
	tc.Bind(k, nil)

	return &durableHarness{agent: tc, host: host, repo: repo, kernel: k, locator: locator}
}

// session persists the Session a turn locks on and returns it.
func (h *durableHarness) session(t *testing.T, ctx context.Context, caseID int64) *model.Session {
	t.Helper()
	ssn := newThreadSession()
	ssn.CaseID = caseID
	ssn.CreatorUserID = "U-REPORTER"
	gt.NoError(t, h.repo.Session().Put(ctx, ssn)).Required()
	return ssn
}

// mentionRequest is a ModeMention turn on an existing case.
func (h *durableHarness) mentionRequest(ssn *model.Session, triggerTS string) threadcase.TurnRequest {
	return threadcase.TurnRequest{
		Session:       ssn,
		Workspace:     newThreadWorkspace(),
		Case:          newThreadCase(),
		ChannelID:     ssn.ChannelID,
		ThreadTS:      ssn.ThreadTS,
		MentionTS:     triggerTS,
		MentionText:   "<@bot> any update?",
		MentionUserID: "U-HUMAN",
		TriggerTS:     triggerTS,
		Mode:          threadcase.ModeMention,
	}
}

// createRequest is a ModeCreate turn on a thread that is not a case yet.
func (h *durableHarness) createRequest(ssn *model.Session, triggerTS string) threadcase.TurnRequest {
	return threadcase.TurnRequest{
		Session:       ssn,
		Workspace:     newThreadWorkspace(),
		ChannelID:     ssn.ChannelID,
		ThreadTS:      ssn.ThreadTS,
		MentionTS:     triggerTS,
		MentionText:   "the deploy broke staging",
		MentionUserID: "U-HUMAN",
		TriggerTS:     triggerTS,
		Mode:          threadcase.ModeCreate,
	}
}

// awaitTerminal drives the worker until pid reaches a terminal state.
func (h *durableHarness) awaitTerminal(t *testing.T, pid agentkit.ProcessID) *agentkit.Process {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	served := make(chan error, 1)
	go func() {
		served <- agentkernel.Serve(ctx, h.kernel, agentkit.WithPollInterval(5*time.Millisecond))
	}()

	for {
		proc, err := h.kernel.GetProcess(ctx, pid)
		gt.NoError(t, err).Required()
		if proc.Status.Terminal() {
			cancel()
			<-served
			return proc
		}
		select {
		case <-ctx.Done():
			gt.NoError(t, ctx.Err()).Required()
			return proc
		case <-time.After(3 * time.Millisecond):
		}
	}
}

// spawned resolves the run a trigger key started.
func (h *durableHarness) spawned(t *testing.T, ssn *model.Session, triggerTS string) agentkit.ProcessID {
	t.Helper()
	pid, err := h.locator.ByTrigger(context.Background(),
		agentkernel.TriggerKey(ssn.ChannelID, ssn.ThreadTS, triggerTS))
	gt.NoError(t, err).Required()
	gt.Value(t, pid).NotEqual(agentkit.ProcessID(""))
	return pid
}

// run spawns a turn and drives it to completion.
func (h *durableHarness) run(t *testing.T, req threadcase.TurnRequest) *agentkit.Process {
	t.Helper()
	return h.runCtx(t, context.Background(), req)
}

// runCtx is run with the caller's context, for a test asserting on something the
// host reads off it — the request language, which only StartTurn's context carries.
func (h *durableHarness) runCtx(t *testing.T, ctx context.Context, req threadcase.TurnRequest) *agentkit.Process {
	t.Helper()
	res, err := h.agent.StartTurn(ctx, req)
	gt.NoError(t, err).Required()
	gt.Value(t, res.Status).Equal(threadcase.StatusStarted)
	return h.awaitTerminal(t, h.spawned(t, req.Session, req.TriggerTS))
}

// A mention turn that finalizes with a respond decision must hand that decision
// to the host — from the completion handler, since StartTurn returned long before
// the model answered — and stamp the session as having replied.
func TestDurableMentionRespond(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Observed the user's question.",
		replanDone,
		`{"kind":"respond","message":"Here is what I found."}`,
	))
	ssn := h.session(t, ctx, 42)

	proc := h.run(t, h.mentionRequest(ssn, "1700000001.000001"))
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.Value(t, calls[0].Kind).Equal("mention")
	gt.Value(t, calls[0].Decision).NotNil().Required()
	gt.Value(t, calls[0].Decision.Kind).Equal(threadcase.DecisionRespond)
	gt.String(t, calls[0].Decision.Message).Equal("Here is what I found.")
	// The target names the case and the thread the reply belongs in.
	gt.Value(t, calls[0].Target.CaseID).Equal(int64(42))
	gt.Value(t, calls[0].Target.ChannelID).Equal(ssn.ChannelID)
	gt.Value(t, calls[0].Target.ThreadTS).Equal(ssn.ThreadTS)

	stored, err := h.repo.Session().GetByThread(ctx, ssn.ChannelID, ssn.ThreadTS)
	gt.NoError(t, err).Required()
	gt.Value(t, stored.LastAction).Equal(model.SessionEndedWithCaseBoundReply)
	// The mention this turn processed is where the next turn's delta scan starts.
	gt.Value(t, stored.LastMentionTS).Equal("1700000001.000001")
}

// A materialize decision must reach the host with its full proposed content: the
// host, not the runtime, writes it onto the case.
func TestDurableMentionMaterialize(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Gathered the details.",
		replanDone,
		`{"kind":"materialize","title":"Staging deploy failure","description":"The 14:00 deploy failed on staging.","fields":[{"field_id":"severity","value":"high"}]}`,
	))
	ssn := h.session(t, ctx, 42)

	h.run(t, h.mentionRequest(ssn, "1700000001.000002"))

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.Value(t, calls[0].Decision).NotNil().Required()
	gt.Value(t, calls[0].Decision.Kind).Equal(threadcase.DecisionMaterialize)
	gt.String(t, calls[0].Decision.Title).Equal("Staging deploy failure")
	gt.String(t, calls[0].Decision.Description).Equal("The 14:00 deploy failed on staging.")
	gt.Array(t, calls[0].Decision.Fields).Length(1).Required()
	gt.String(t, calls[0].Decision.Fields[0].FieldID).Equal("severity")
	gt.String(t, calls[0].Decision.Fields[0].Value).Equal("high")
}

// The round-1 direct path answers in prose. It must be applied as a respond
// decision, exactly as a parsed one would be, so the host has one code path.
func TestDurableMentionDirectBecomesARespond(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		`{"direct":{"reason":"trivial question"}}`,
		"It is still open.",
	))
	ssn := h.session(t, ctx, 42)

	h.run(t, h.mentionRequest(ssn, "1700000001.000003"))

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.Value(t, calls[0].Kind).Equal("mention")
	gt.Value(t, calls[0].Decision).NotNil().Required()
	gt.Value(t, calls[0].Decision.Kind).Equal(threadcase.DecisionRespond)
	gt.String(t, calls[0].Decision.Message).Contains("still open")
}

// Every call in the turn must be told which language to answer in. The host is the
// only thing that knows — planexec renders the directive from Input.LanguageLabel
// and omits it entirely when that is empty — and a turn with no directive answers a
// Japanese thread in English, since every prompt around it is written in English.
// There is no other symptom: the turn succeeds and nothing logs.
func TestDurableMentionTellsTheTurnWhichLanguageToAnswerIn(t *testing.T) {
	ctx := i18n.ContextWithLang(context.Background(), i18n.LangJA)
	llm, calls := recordingDurableLLM(
		`{"direct":{"tools":[]}}`,
		// The reply's own wording is not the contract here — the directive handed to
		// the model is — so the fixture stays English.
		"It is still open.",
	)
	h := newDurableHarness(t, llm)
	ssn := h.session(t, ctx, 42)

	h.runCtx(t, ctx, h.mentionRequest(ssn, "1700000001.000004"))

	// The planner call and the direct-reply child: both carry the directive, and the
	// second is the one whose text is posted to the thread.
	seen := calls()
	gt.Array(t, seen).Length(2).Required()
	gt.String(t, seen[0].SystemPrompt).Contains("**Japanese**")
	gt.String(t, seen[1].SystemPrompt).Contains("**Japanese**")
}

// The turn's current time. Without it the planner resolves "today" and "by
// tomorrow" against whatever date its training suggests — the create turn that
// produced this test was told "by today" and wrote a due date a month in the
// past.
//
// It rides in the FIRST USER MESSAGE because a thread-mode turn continues the
// previous turn's conversation (Durable.inheritOpts →
// agentkit.WithInheritedHistory) and the system prompt is not part of that
// history: a resumed turn would otherwise carry the earlier messages with
// nothing saying when they were written.
//
// The planner and its sub-agents must be told the SAME instant — the sub-agent
// reads it from agent.TaskContext, which is built separately — or a task
// dispatched mid-turn resolves a relative date differently from the planner that
// wrote it. Asserting equality is what pins the two together: each side's own
// rendering is already covered by agent.PlannerMessage and agent.TaskContext.
func TestDurableMentionTellsThePlannerAndItsSubAgentsOneTime(t *testing.T) {
	ctx := context.Background()
	llm, calls := recordingDurableLLM(
		investigatePlan,
		"Read the thread.",
		replanDone,
		`{"kind":"respond","message":"It is still open."}`,
	)
	h := newDurableHarness(t, llm)
	ssn := h.session(t, ctx, 42)

	h.run(t, h.mentionRequest(ssn, "1700000001.000010"))

	seen := calls()
	gt.Array(t, seen).Length(4).Required()

	// Call 1 is the plan: the time leads its user message, and is NOT in the
	// system prompt, which the inherited history does not carry.
	planner := seen[0]
	gt.String(t, planner.Input).Contains("# Current time")
	gt.Bool(t, strings.Contains(planner.SystemPrompt, "# Current time")).False()

	// Call 2 is the sub-agent, prompted from the task context.
	child := seen[1]
	gt.String(t, child.SystemPrompt).Contains("- current_time: ")

	plannerTime := rfc3339In(t, planner.Input)
	childTime := rfc3339In(t, child.SystemPrompt)
	gt.String(t, childTime).Equal(plannerTime)
}

// A create turn's proposal must reach the host with its field values already
// typed against the workspace schema, and the session must record that the turn
// ended by replying rather than by asking.
func TestDurableCreateCommitsTheProposal(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Read the thread.",
		replanDone,
		`{"title":"Staging deploy failure","description":"The 14:00 deploy failed on staging.","fields":[{"field_id":"severity","value":"high"}]}`,
	))
	ssn := h.session(t, ctx, 0)

	proc := h.run(t, h.createRequest(ssn, "1700000001.000004"))
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.Value(t, calls[0].Kind).Equal("create")
	gt.String(t, calls[0].Create.Title).Equal("Staging deploy failure")
	gt.String(t, calls[0].Create.Description).Equal("The 14:00 deploy failed on staging.")
	gt.Map(t, calls[0].Create.Fields).HasKey("severity")
	gt.Value(t, calls[0].Create.Fields["severity"].Value).Equal("high")
	// No case exists yet, which is what distinguishes a create target.
	gt.Value(t, calls[0].Target.CaseID).Equal(int64(0))
}

// A field value the workspace schema does not allow must be fed back to the model
// and the proposal regenerated, not kill the turn. The pre-agentkit code path lost
// the whole turn to a single bad option id.
func TestDurableCreateRegeneratesARejectedProposal(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Read the thread.",
		replanDone,
		// "critical" is not an option of the severity field.
		`{"title":"Deploy failure","description":"It failed.","fields":[{"field_id":"severity","value":"critical"}]}`,
		`{"title":"Deploy failure","description":"It failed.","fields":[{"field_id":"severity","value":"high"}]}`,
	))
	ssn := h.session(t, ctx, 0)

	proc := h.run(t, h.createRequest(ssn, "1700000001.000005"))
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.Value(t, calls[0].Kind).Equal("create")
	gt.Value(t, calls[0].Create.Fields["severity"].Value).Equal("high")
}

// A persistence failure from the host is surfaced to the user; it is NOT fed back
// to the model, which cannot repair an infrastructure error by re-emitting the
// same JSON.
func TestDurableCreateReportsAPersistenceFailure(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Read the thread.",
		replanDone,
		`{"title":"Deploy failure","description":"It failed.","fields":[{"field_id":"severity","value":"high"}]}`,
	))
	h.host.createErr = goerr.New("write conflict on the case counter")
	ssn := h.session(t, ctx, 0)

	h.run(t, h.createRequest(ssn, "1700000001.000006"))

	gt.Array(t, h.host.kinds()).Equal([]string{"create", "fallback"})
	calls := h.host.Calls()
	gt.String(t, calls[1].Reason).Contains("write conflict")
}

// A planner question ends the turn: the host posts it, and the session records
// that it is waiting for an answer so a later event resumes rather than restarts.
func TestDurableQuestionEndsTheTurn(t *testing.T) {
	ctx := context.Background()
	// The opening round may not ask — it has nothing to ask about yet — so the
	// question comes on the replan round, after the first investigation.
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Read the thread; the environment is not stated.",
		`{"question":{"reason":"which environment?","items":[{"id":"env","text":"Which environment?","type":"select","options":["staging","production"]}]}}`,
	))
	ssn := h.session(t, ctx, 0)

	proc := h.run(t, h.createRequest(ssn, "1700000001.000007"))
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.Value(t, calls[0].Kind).Equal("question")
	gt.String(t, calls[0].Question.Reason).Equal("which environment?")
	gt.Array(t, calls[0].Question.Items).Length(1).Required()
	gt.String(t, calls[0].Question.Items[0].ID).Equal("env")
	gt.String(t, calls[0].Question.Items[0].Text).Equal("Which environment?")
	gt.Value(t, calls[0].Question.Items[0].Type).Equal(threadcase.QuestionItemSelect)
	gt.Array(t, calls[0].Question.Items[0].Options).Equal([]string{"staging", "production"})

	stored, err := h.repo.Session().GetByThread(ctx, ssn.ChannelID, ssn.ThreadTS)
	gt.NoError(t, err).Required()
	gt.Value(t, stored.LastAction).Equal(model.SessionEndedWithQuestion)
}

// A question the host could not deliver must NOT leave the thread recorded as
// waiting on one.
//
// Whether the Slack post failed or the Session write after it did, the outcome is
// the same from the thread's side: there is no form to answer. Recording
// post_question anyway strands the user twice over — the submit handler reads the
// missing PendingQuestion as stale and drops the answer, and a plain reply resumes
// a turn with no question in it. A turn that could not ask reached no conclusion,
// so it must report a fallback like any other.
func TestDurableQuestionDeliveryFailureFallsBack(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Read the thread; the environment is not stated.",
		`{"question":{"reason":"which environment?","items":[{"id":"env","text":"Which environment?","type":"select","options":["staging","production"]}]}}`,
	))
	h.host.questionErr = goerr.New("slack refused the form")
	ssn := h.session(t, ctx, 0)

	proc := h.run(t, h.createRequest(ssn, "1700000001.000031"))
	// The run itself succeeded — delivery is the host's half, after the turn.
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	// The user is told the turn ended, rather than being left watching for a form.
	gt.Array(t, h.host.kinds()).Equal([]string{"question", "fallback"})

	stored, err := h.repo.Session().GetByThread(ctx, ssn.ChannelID, ssn.ThreadTS)
	gt.NoError(t, err).Required()
	gt.Value(t, stored).NotNil().Required()
	gt.Value(t, stored.LastAction).NotEqual(model.SessionEndedWithQuestion)
	gt.Value(t, stored.PendingQuestion).Nil()
}

// A run the model never answers must tell the user the turn ended rather than
// leaving the thread silent.
func TestDurableReportsAFailedRun(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableFailingLLM())
	ssn := h.session(t, ctx, 42)

	proc := h.run(t, h.mentionRequest(ssn, "1700000001.000008"))
	gt.Value(t, proc.Status).Equal(agentkit.ProcessFailed)

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.Value(t, calls[0].Kind).Equal("fallback")
	gt.String(t, calls[0].Reason).NotEqual("")
}

// A mention turn records the JobRunLog the case agent page lists, opened after the
// spawn and closed by the completion handler with the usage read off the Process —
// the only place a run whose transitions span claims accumulates it.
func TestDurableMentionRecordsTheRun(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Observed the user's question.",
		replanDone,
		`{"kind":"respond","message":"Here is what I found."}`,
	))
	ssn := h.session(t, ctx, 42)

	proc := h.run(t, h.mentionRequest(ssn, "1700000001.000009"))
	sc := agentkernel.ScopeFrom(proc.Metadata)
	gt.Value(t, sc.EventType).Equal(model.EventTypeMention)
	gt.String(t, sc.JobID).NotEqual("")
	gt.String(t, sc.JobRunID).NotEqual("")

	runs, err := h.repo.JobRun().ListByCase(ctx, sc.WorkspaceID, sc.CaseID)
	gt.NoError(t, err).Required()
	gt.Array(t, runs).Length(1).Required()
	gt.Value(t, runs[0].LastRunID).Equal(sc.JobRunID)
	gt.Value(t, runs[0].LastStatus).Equal(model.JobRunStatusSuccess)

	log, err := h.repo.JobRunLog().Get(ctx,
		model.JobRunKey{WorkspaceID: sc.WorkspaceID, CaseID: sc.CaseID, JobID: sc.JobID}, sc.JobRunID)
	gt.NoError(t, err).Required()
	gt.Value(t, log.Stage).Equal(model.JobRunStageSuccess)
	// The usage totals come off the Process, so they are non-zero even though no
	// single in-process handler saw every transition.
	gt.Number(t, log.LLMCallCount).GreaterOrEqual(1)
	gt.Number(t, log.InputTokens).GreaterOrEqual(1)
	// The cost is that same usage priced at testModelPolicy's rate ($1 / $5 per
	// MTok, no cache pricing), and the model is the one the run's scope resolved
	// to. A host that recorded neither fails here rather than showing an em dash
	// on the run-detail page.
	gt.Value(t, log.CostNanoUSD).Equal(log.InputTokens*1000 + log.OutputTokens*5000)
	gt.Number(t, log.CostNanoUSD).GreaterOrEqual(1)
	gt.String(t, log.Model).Equal("test-model")
}

// A create turn keeps no run record: it runs before the case exists, and the case
// agent page lists runs BY case.
func TestDurableCreateKeepsNoRunRecord(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Read the thread.",
		replanDone,
		`{"title":"Deploy failure","description":"It failed."}`,
	))
	ssn := h.session(t, ctx, 0)

	proc := h.run(t, h.createRequest(ssn, "1700000001.000010"))
	sc := agentkernel.ScopeFrom(proc.Metadata)
	gt.String(t, sc.JobRunID).Equal("")
	gt.String(t, sc.EventType).Equal("")
	// A create turn has no case whose Actions it could manage.
	gt.Value(t, sc.ThreadModeActions).Equal(false)
}

// A second mention arriving while a turn is live must be refused as busy and name
// the session holding the thread, not queue a second run onto it.
func TestDurableRefusesASecondTurnOnTheSameThread(t *testing.T) {
	ctx := context.Background()
	// One scripted reply: the first run parks mid-plan so the thread stays held.
	h := newDurableHarness(t, durableLLM(investigatePlan))
	ssn := h.session(t, ctx, 42)

	first, err := h.agent.StartTurn(ctx, h.mentionRequest(ssn, "1700000001.000011"))
	gt.NoError(t, err).Required()
	gt.Value(t, first.Status).Equal(threadcase.StatusStarted)

	second, err := h.agent.StartTurn(ctx, h.mentionRequest(ssn, "1700000001.000012"))
	gt.NoError(t, err).Required()
	gt.Value(t, second.Status).Equal(threadcase.StatusBusy)
	gt.Value(t, second.BusyOwner).NotNil().Required()
	gt.Value(t, second.BusyOwner.ID).Equal(ssn.ID)

	// A refused turn must leave the thread untouched.
	gt.Array(t, h.host.Calls()).Length(0)

	// And it must leave the mention cursor where the turn that IS running put it.
	// The next turn's delta scan starts strictly after this value, so advancing it
	// for a refused mention hides that mention AND every thread reply before it from
	// whichever turn runs next — and a busy thread is exactly when a person keeps
	// typing. The running turn cannot cover them: its input was fixed at its own
	// spawn, before either existed.
	stored, err := h.repo.Session().GetByThread(ctx, ssn.ChannelID, ssn.ThreadTS)
	gt.NoError(t, err).Required()
	gt.Value(t, stored).NotNil().Required()
	gt.String(t, stored.LastMentionTS).Equal("1700000001.000011")
}

// The turn that actually starts owns the cursor: the next turn's delta scan must
// begin strictly after the mention this one processes.
func TestDurableAdvancesTheMentionCursorOnAStartedTurn(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(investigatePlan))
	ssn := h.session(t, ctx, 42)
	gt.String(t, ssn.LastMentionTS).Equal("")

	res, err := h.agent.StartTurn(ctx, h.mentionRequest(ssn, "1700000001.000021"))
	gt.NoError(t, err).Required()
	gt.Value(t, res.Status).Equal(threadcase.StatusStarted)

	stored, err := h.repo.Session().GetByThread(ctx, ssn.ChannelID, ssn.ThreadTS)
	gt.NoError(t, err).Required()
	gt.Value(t, stored).NotNil().Required()
	gt.String(t, stored.LastMentionTS).Equal("1700000001.000021")
}

// A re-delivered Slack event must be dropped silently, with no second run and no
// "busy" message — the precedence the pre-agentkit turn lock applied.
func TestDurableDropsARedeliveredTrigger(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(investigatePlan))
	ssn := h.session(t, ctx, 42)

	req := h.mentionRequest(ssn, "1700000001.000013")
	first, err := h.agent.StartTurn(ctx, req)
	gt.NoError(t, err).Required()
	gt.Value(t, first.Status).Equal(threadcase.StatusStarted)

	again, err := h.agent.StartTurn(ctx, req)
	gt.NoError(t, err).Required()
	gt.Value(t, again.Status).Equal(threadcase.StatusIdempotent)
	gt.Array(t, h.host.Calls()).Length(0)
}

// The run must carry the mentioning user as its access actor. Without one the
// usecase layer reads the run as a system context and bypasses private-case
// access control entirely, so this is a security invariant.
func TestDurableRecordsTheActorAndScope(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(investigatePlan))
	ssn := h.session(t, ctx, 42)

	_, err := h.agent.StartTurn(ctx, h.mentionRequest(ssn, "1700000001.000014"))
	gt.NoError(t, err).Required()

	pid := h.spawned(t, ssn, "1700000001.000014")
	proc, err := h.kernel.GetProcess(ctx, pid)
	gt.NoError(t, err).Required()

	sc := agentkernel.ScopeFrom(proc.Metadata)
	gt.Value(t, sc.ActorUserID).Equal("U-HUMAN")
	gt.Value(t, sc.WorkspaceID).Equal("support")
	gt.Value(t, sc.CaseID).Equal(int64(42))
	gt.Value(t, sc.SessionID).Equal(ssn.ID)
	gt.Value(t, proc.Agent).Equal(agentkernel.AgentCaseThread)
	// An interactive turn must never queue behind a scheduled batch.
	gt.Value(t, sc.SlotGated).Equal(false)
	// A mention on an existing case may manage its Actions, so its scope lifts
	// the thread-mode workspace's default of no Action tools.
	gt.Value(t, sc.ThreadModeActions).Equal(true)
}

// A cross-channel reaction has two threads: the case lives in the monitored
// channel while the reactor watches the thread they reacted in. Both must survive
// to the completion handler, or the outcome is posted in the wrong place.
func TestDurableCarriesBothThreadsForAReactionCreate(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Read the thread.",
		replanDone,
		`{"title":"Deploy failure","description":"It failed."}`,
	))
	ssn := h.session(t, ctx, 0)

	req := h.createRequest(ssn, "1700000001.000015")
	req.UIChannelID = "C-SOURCE"
	req.UIThreadTS = "1700000002.000001"
	h.run(t, req)

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.Value(t, calls[0].Kind).Equal("create")
	gt.Value(t, calls[0].Target.ChannelID).Equal(ssn.ChannelID)
	gt.Value(t, calls[0].Target.ThreadTS).Equal(ssn.ThreadTS)
	gt.Value(t, calls[0].Target.UIChannelID).Equal("C-SOURCE")
	gt.Value(t, calls[0].Target.UIThreadTS).Equal("1700000002.000001")
}

// For every other run the two threads coincide, and the host must see them equal
// rather than empty.
func TestDurableUITargetDefaultsToTheRunsOwnThread(t *testing.T) {
	ctx := context.Background()
	h := newDurableHarness(t, durableLLM(
		investigatePlan,
		"Observed.",
		replanDone,
		`{"kind":"respond","message":"Done."}`,
	))
	ssn := h.session(t, ctx, 42)

	h.run(t, h.mentionRequest(ssn, "1700000001.000016"))

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.Value(t, calls[0].Target.UIChannelID).Equal(ssn.ChannelID)
	gt.Value(t, calls[0].Target.UIThreadTS).Equal(ssn.ThreadTS)
}

func TestNewDurableRejectsMissingDependencies(t *testing.T) {
	registry := model.NewWorkspaceRegistry()
	registry.Register(newThreadWorkspace())

	models := testModelPolicy(t)

	_, err := threadcase.NewDurable(nil, registry, &durableHost{}, nil, models)
	gt.Error(t, err).Required()

	_, err = threadcase.NewDurable(memory.New(), nil, &durableHost{}, nil, models)
	gt.Error(t, err).Required()

	_, err = threadcase.NewDurable(memory.New(), registry, nil, nil, models)
	gt.Error(t, err).Required()
}

// An unbound host must say so rather than panicking on a nil Kernel.
func TestDurableStartTurnRefusesWhenUnbound(t *testing.T) {
	registry := model.NewWorkspaceRegistry()
	registry.Register(newThreadWorkspace())
	tc, err := threadcase.NewDurable(memory.New(), registry, &durableHost{}, nil, testModelPolicy(t))
	gt.NoError(t, err).Required()

	ssn := newThreadSession()
	_, err = tc.StartTurn(context.Background(), threadcase.TurnRequest{
		Session:   ssn,
		Workspace: newThreadWorkspace(),
		Case:      newThreadCase(),
		ChannelID: ssn.ChannelID,
		ThreadTS:  ssn.ThreadTS,
		Mode:      threadcase.ModeMention,
	})
	gt.Error(t, err).Required()
}
