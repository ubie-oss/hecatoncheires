package planexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"

	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/toolargs"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// Phases. A transition reads the phase, does one thing, and writes the phase it
// leaves behind.
const (
	// phasePlan asks the planner what to do first.
	phasePlan = "plan"
	// phaseCollect folds the finished children into observations. No LLM call.
	phaseCollect = "collect"
	// phaseReplan asks the planner what to do next, given the observations.
	phaseReplan = "replan"
	// phaseFinal produces the terminal output.
	phaseFinal = "final"
	// phasePlannerTool runs ONE tool call the planner asked for before it decides,
	// or that the terminal call asked for before it writes the answer.
	// It is a phase of its own, rather than a loop inside the calling transition,
	// for the same reason every other step is: one transition is one LLM call or
	// one tool call, so a crash costs at most one of them.
	phasePlannerTool = "planner_tool"
	// phaseAnswer folds a human's answer back into the run. It is reached only by
	// a host that asked to wait in-band (Input.SuspendOnQuestion); every other host
	// ends the turn on a question instead.
	phaseAnswer = "answer"
)

// Model roles. A host binds them to a specific model through the Kernel; an
// unbound role falls back to the Kernel's default model.
var (
	// RolePlanner is the model that plans and replans.
	RolePlanner = agentkit.DefineModelRole("hecatoncheires.planner")
	// RoleFinalizer is the model that writes the terminal output.
	RoleFinalizer = agentkit.DefineModelRole("hecatoncheires.finalizer")
)

// Input is the launch input: what the host decided before the run existed.
// Everything the runtime supplies itself — history, traces, tools, the question
// channel — is deliberately absent.
type Input struct {
	// HandoffTool lets a host offer an explicit, effect-free terminal handoff.
	// The host owns the tool schema and decides whether a fresh stage is safe.
	// Nil preserves the ordinary planning lifecycle.
	HandoffTool *gollem.ToolSpec `json:"handoff_tool,omitempty"`

	// SystemPrompt is the host's base persona prompt. The planner prompt is
	// rendered around it.
	SystemPrompt string `json:"system_prompt"`
	// UserInput is the first user message.
	UserInput string `json:"user_input"`
	// LanguageLabel ("Japanese", "English", …) drives the user-facing-language
	// directive. Empty omits it.
	LanguageLabel string `json:"language_label,omitempty"`
	// KnownToolIDs is the toolset-id vocabulary the planner may assign to a task.
	// It is both the prompt's enumeration and the JSON schema's enum.
	KnownToolIDs []string `json:"known_tool_ids"`
	// TaskContext is an opaque block the host renders into EVERY sub-agent's
	// system prompt. planexec neither parses nor validates it.
	//
	// It exists because a sub-agent's prompt is otherwise built from the planner's
	// task text alone, while its tools are pinned to the run's subject: a task
	// handed the Slack read tools and told to "read the case thread" has no way to
	// know the channel id or thread ts, so it invents them and the call fails
	// (slack__get_messages then reports "requires both channel_id and ts"). The
	// host puts the identifiers its tools are pinned to here — see
	// .claude/rules/architecture.md § "Agent tool wiring": a prompt that names a
	// tool and the context that tool needs must ship together.
	TaskContext string `json:"task_context,omitempty"`

	AllowQuestion       bool `json:"allow_question,omitempty"`
	AllowDirect         bool `json:"allow_direct,omitempty"`
	AllowSubAgentWrites bool `json:"allow_sub_agent_writes,omitempty"`
	// SuspendOnQuestion keeps the run open across the human's reply instead of
	// ending the turn on it: the run parks on a question await and continues when
	// the host calls Kernel.Respond with the answer.
	//
	// A host should set it ONLY when its own record spans the wait — an
	// interactive Job, whose run id covers the whole exchange. For a Slack thread
	// it is the wrong trade: the run would hold the thread's subject for as long as
	// the person takes to answer, blocking every later turn on that thread.
	SuspendOnQuestion bool `json:"suspend_on_question,omitempty"`

	// Progress locates the thread milestone lines are drawn into. A zero value
	// draws nothing.
	Progress ProgressTarget `json:"progress,omitempty"`
}

// Validate enforces what the planner cannot run without.
func (in Input) Validate() error {
	if in.SystemPrompt == "" {
		return goerr.New("system prompt is required")
	}
	if in.UserInput == "" {
		return goerr.New("user input is required")
	}
	if len(in.KnownToolIDs) == 0 {
		return goerr.New("known tool ids must not be empty")
	}
	return nil
}

// ProgressTarget is the thread a run draws its milestones into.
type ProgressTarget struct {
	ChannelID string `json:"channel_id,omitempty"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	// MessageTS is a progress message the host already posted into the thread —
	// the create path's acknowledgement, posted before the turn is spawned so the
	// thread is not silent while it starts. The run updates that message rather
	// than posting a second one, which is what keeps a turn to ONE message. Empty
	// means the run posts its own on its first milestone.
	MessageTS string `json:"message_ts,omitempty"`
}

// isZero reports whether there is nowhere to draw.
func (t ProgressTarget) isZero() bool { return t.ChannelID == "" || t.ThreadTS == "" }

// Progress draws a run's current milestone. The host implements it; planexec
// holds no Slack dependency.
//
// It is stateless on purpose: the message id lives in the run's checkpointed
// state, so another instance picking the run up keeps drawing into the same
// message instead of starting a second one.
type Progress interface {
	// Render draws line as the single line of one message and returns its id. An
	// empty messageTS means "post a new one"; anything else means "update that
	// one". The line REPLACES whatever the message showed before — a run's
	// progress never accumulates in the thread.
	Render(ctx context.Context, target ProgressTarget, messageTS string, line string) (string, error)
}

// Asker delivers a run's question to whoever can answer it, for a host that waits
// in-band (Input.SuspendOnQuestion). The host implements it; planexec holds no
// Slack dependency.
//
// pid and key are what the answer must be Responded to — the host records the
// pair alongside whatever it posts, because the reply arrives later, out of band,
// on an instance that never saw this transition. meta is the run's Process
// metadata, which is what lets one registered Asker serve every run.
//
// It posts before the run suspends, so a transition that is replayed would post
// twice. That is why every Serve in this application bounds unclean reclaims to 0
// (agentkernel.Serve): a replay fails the run instead of re-posting.
type Asker interface {
	Ask(ctx context.Context, pid agentkit.ProcessID, meta map[string]string,
		key agentkit.AwaitKey, q Question) error
}

// OutputKind discriminates how a turn ended.
type OutputKind string

const (
	// OutputFinal is a planner-declared finalize that produced a terminal output.
	OutputFinal OutputKind = "final"
	// OutputDirect is the round-1 answer-without-investigation path.
	OutputDirect OutputKind = "direct"
	// OutputQuestion is the planner asking the user and ending the turn.
	OutputQuestion OutputKind = "question"
	// OutputFallback is a turn that reached no conclusion.
	OutputFallback OutputKind = "fallback"
	// OutputHandoff asks the host to transfer control without another tool call.
	OutputHandoff OutputKind = "handoff"
)

// Output is what a finished run produces. Exactly one of Data / Text / Question
// carries the payload, selected by Kind.
type Output[T any] struct {
	Kind          OutputKind `json:"kind"`
	HandoffReason string     `json:"handoff_reason,omitempty"`
	// Data is the validated structured output. Set only for OutputFinal on a
	// structured host.
	Data *T `json:"data,omitempty"`
	// Text is the reply for OutputDirect, and for OutputFinal on a text-only host.
	Text string `json:"text,omitempty"`
	// Question is what to ask the user. Set only for OutputQuestion.
	Question *Question `json:"question,omitempty"`
	// FallbackReason says why no conclusion was reached.
	FallbackReason string `json:"fallback_reason,omitempty"`
	// Observations is the per-round trail, carried on every kind so even a
	// fallback can report what was learnt.
	Observations []PhaseSummary `json:"observations,omitempty"`
}

// TextResult is the T for hosts whose terminal output is prose rather than a
// structured object. Validate always accepts: there is no shape to check.
type TextResult struct {
	Text string `json:"text"`
}

// Validate satisfies Validatable.
func (TextResult) Validate() error { return nil }

// Finalizer validates a decoded terminal output against context T.Validate()
// cannot see — a workspace field schema, say. A returned error is fed back to the
// model and the output regenerated.
//
// It MUST be side-effect-free: a later attempt re-runs every finalizer, so
// committing anything here would commit it several times. Committing the output
// happens after the turn, never in a finalizer.
//
// meta is the run's Process metadata, which is what lets one registered finalizer
// serve every run: the host reads its own scope back out of it (the workspace,
// the case) instead of closing over a single run's values, since a strategy is
// registered once at startup and then serves every run of that agent.
type Finalizer[T any] func(ctx context.Context, meta map[string]string, out *T) error

// Validatable is the constraint on the structured terminal-output type. The
// strategy decodes the planner's terminal JSON into T, then calls Validate() and
// regenerates on failure: gollem's response-schema check verifies the JSON shape
// only, so Validate() is where a host enforces its domain invariants (required
// fields, allowed values). A TextOnly host instantiates T as TextResult, whose
// Validate always accepts.
type Validatable interface {
	Validate() error
}

// Config is the host's terminal-output contract.
type Config[T Validatable] struct {
	// Decode turns the terminal JSON into T. nil uses encoding/json.
	Decode func([]byte) (*T, error)
	// Finalizers run in order after T.Validate(); the first rejection wins.
	Finalizers []Finalizer[T]
	// Asker delivers a question for a host that waits in-band. Required when a run
	// sets Input.SuspendOnQuestion — without one there would be nobody to show the
	// question to, and the run would park on an await nothing can answer.
	Asker Asker
	// TextOnly generates the terminal output as prose instead of JSON and puts it
	// in Output.Text.
	TextOnly bool
	// Remaining reports what this run may still spend and the total it was
	// allowed, so the planner can size the tasks it plans and each child can be
	// given a ceiling of its own.
	//
	// It is a function rather than a value because a strategy is registered once
	// at startup and then serves every run: the figures come from the run's own
	// metadata and metrics, never from anything a registration could close over.
	// Same reason Finalizers take the run's metadata.
	//
	// nil turns the per-task budget off entirely: the planner is shown no budget
	// line, `budget_usd` leaves the task schema and is not validated, and a child
	// is spawned with the metadata it would have had otherwise — its parent's, so
	// it is judged against the ROOT figure on its own metrics.
	Remaining func(meta map[string]string, metrics agentkit.Metrics) (remaining, total pricing.NanoUSD)
}

// observationsMaxBytes bounds the checkpointed observation trail.
//
// The trail is by far the largest part of the state, and the state is written to
// one Firestore document on every transition. With the default budget a run can
// reach ~21 rounds x 5 tasks x 8 KiB of summary, which would cross Firestore's
// 1 MiB document limit and strand the run with no way to commit. Dropping the
// oldest rounds keeps the recent, relevant observations and leaves a note saying
// what went.
const observationsMaxBytes = 256 * 1024

// state is the checkpointed state. Everything the next transition needs must be
// here: Step runs from the top every time, including after a crash on another
// instance.
type state struct {
	Phase string `json:"phase"`
	Round int    `json:"round"`
	Input Input  `json:"input"`
	// NextInput is the user message the next planner round receives.
	NextInput    string         `json:"next_input"`
	Observations []PhaseSummary `json:"observations,omitempty"`
	// Current are the children this round launched, in plan order.
	Current  []taskRef         `json:"current,omitempty"`
	RoundKey agentkit.AwaitKey `json:"round_key,omitempty"`
	// Direct marks the current round as the direct fast path, whose single
	// child's answer IS the turn's reply.
	Direct   bool          `json:"direct,omitempty"`
	Progress progressState `json:"progress,omitempty"`
	// FinalRetries counts terminal outputs rejected by decode / Validate /
	// finalizers.
	FinalRetries int `json:"final_retries,omitempty"`
	// Reserve records that the run crossed into its budget reserve, so it stops
	// planning and goes to produce its terminal output with what it has.
	//
	// It is re-derived from Syscalls.LimitStatus() at the top of every Step, so a
	// checkpoint written before this field existed (or by a build that names it
	// differently) costs the run nothing: the same transition sets it again.
	//
	// A checkpoint written by a build that also counted the reserve's tool round
	// (reserve_tool_rounds) decodes as well: the field is ignored, and the run's
	// next terminal call is sent with tool calls disabled, which is what that
	// build did on the reserve's second move.
	Reserve bool `json:"reserve,omitempty"`
	// PendingCalls are the tool calls the planner asked for before deciding, and
	// this run has not made yet. They are consumed one per transition.
	PendingCalls []*gollem.FunctionCall `json:"pending_calls,omitempty"`
	// AfterTool is the phase to return to once PendingCalls is drained — the
	// planning phase that asked for them.
	AfterTool string `json:"after_tool,omitempty"`
	// ToolsAnswered records that the drained PendingCalls were answered in the
	// conversation, so the planning call that follows continues from it and sends
	// no input of its own.
	ToolsAnswered bool `json:"tools_answered,omitempty"`
	// PlannerToolRounds counts how many times the planner has asked for tools
	// within one planning phase, so a model that only ever calls tools cannot spin
	// forever inside a single round.
	PlannerToolRounds int `json:"planner_tool_rounds,omitempty"`
	// AnswerKey is the await a suspended run is waiting on. It is per-round so a
	// follow-up question opens a fresh await rather than reusing a closed one.
	AnswerKey agentkit.AwaitKey `json:"answer_key,omitempty"`
}

// taskRef ties a planned task to the child Process running it. The plan fields
// are copied rather than looked up because the child's result must be
// attributable even if the round that produced it has been dropped from the
// trail.
type taskRef struct {
	TaskID             string             `json:"task_id"`
	Title              string             `json:"title"`
	AcceptanceCriteria string             `json:"acceptance_criteria"`
	ProcessID          agentkit.ProcessID `json:"process_id"`
}

// progressState is the minimum needed to keep drawing into the same message
// after the run moves to another instance.
//
// There is no line history: only the current milestone is ever drawn, and it is
// passed straight through to the host. A state written by a build that recorded
// the history decodes fine — the extra field is simply dropped.
type progressState struct {
	MessageTS string `json:"message_ts,omitempty"`
}

// Register registers the strategy under name and returns the typed handle.
//
// taskAgent is the agent each planned task runs as — a ReAct agent, spawned as a
// child. limiter is the budget this run answers Limit with; it is required,
// because a run with no ceiling is a run that can spend without bound. progress
// may be nil, which draws nothing.
func Register[T Validatable](
	reg *agentkit.Registry, name agentkit.AgentName, version int,
	taskAgent agentkit.Agent[react.Input], progress Progress,
	limiter agentkit.Limiter, cfg Config[T],
	opts ...agentkit.RegisterOption[Output[T]],
) (agentkit.Agent[Input], error) {
	if limiter == nil {
		return agentkit.Agent[Input]{}, goerr.Wrap(agentkit.ErrInvalidAgentDef,
			"planexec: a limiter is required", goerr.V("agent", name))
	}
	if taskAgent.Name() == "" {
		return agentkit.Agent[Input]{}, goerr.Wrap(agentkit.ErrInvalidAgentDef,
			"planexec: a task agent is required", goerr.V("agent", name))
	}
	s := &strategy[T]{
		limiter:   limiter,
		version:   version,
		taskAgent: taskAgent,
		progress:  progress,
		cfg:       cfg,
	}
	// The schema depends on T alone, so it is derived and checked here: a type no
	// provider can be sent fails the process at startup instead of failing every
	// run at its terminal call.
	if !cfg.TextOnly {
		schema, err := outputSchemaOf[T]()
		if err != nil {
			// Joined so the error matches agentkit.ErrInvalidAgentDef, like the
			// other registration failures, and still the gollem error behind it.
			return agentkit.Agent[Input]{}, goerr.Wrap(errors.Join(agentkit.ErrInvalidAgentDef, err),
				"planexec: invalid final output type", goerr.V("agent", name))
		}
		s.outputSchema = schema
	} else {
		schema, err := outputSchemaOf[TextResult]()
		if err != nil {
			return agentkit.Agent[Input]{}, goerr.Wrap(errors.Join(agentkit.ErrInvalidAgentDef, err),
				"planexec: invalid text output type", goerr.V("agent", name))
		}
		s.textSchema = schema
	}
	return agentkit.Register(reg, name, version, s, opts...)
}

// outputSchemaOf derives the terminal output schema of T and rejects one a
// provider would refuse: a schema gollem cannot build or validate, or one
// holding a map. Which model a run uses is operator configuration, and Claude
// structured outputs and OpenAI strict mode both reject a map, so a map is
// refused whatever the provider.
func outputSchemaOf[T any]() (*gollem.Parameter, error) {
	var zero T
	schema, err := gollem.ToSchema(zero)
	if err != nil {
		return nil, goerr.Wrap(err, "derive the final output schema from its type")
	}
	if err := schema.Validate(); err != nil {
		return nil, goerr.Wrap(err, "validate the final output schema")
	}
	if path, found := findMap(schema, ""); found {
		return nil, goerr.New(
			fmt.Sprintf("the final output schema holds a map at %q, which structured outputs cannot express", path),
			goerr.V("path", path))
	}
	return schema, nil
}

// findMap returns the path of the first schema node carrying
// AdditionalProperties, walking Properties in name order and then Items. Paths
// join property names with "." and mark an array element with "[]", e.g.
// "items[].m".
func findMap(p *gollem.Parameter, path string) (string, bool) {
	if p == nil {
		return "", false
	}
	if p.AdditionalProperties != nil {
		return path, true
	}
	names := make([]string, 0, len(p.Properties))
	for name := range p.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := name
		if path != "" {
			child = path + "." + name
		}
		if found, ok := findMap(p.Properties[name], child); ok {
			return found, true
		}
	}
	return findMap(p.Items, path+"[]")
}

type strategy[T Validatable] struct {
	limiter   agentkit.Limiter
	version   int
	taskAgent agentkit.Agent[react.Input]
	progress  Progress
	cfg       Config[T]
	// outputSchema is the terminal output schema derived from T at Register; nil
	// when cfg.TextOnly.
	outputSchema *gollem.Parameter
	// textSchema is TextResult's schema, set only when cfg.TextOnly. A text
	// terminal call continuing from a tool round sends it (see stepFinal).
	textSchema *gollem.Parameter
}

func (s *strategy[T]) Version() int { return s.version }

func (s *strategy[T]) Limit(ctx context.Context, proc *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
	return s.limiter(ctx, proc, m)
}

func (s *strategy[T]) Init(in Input) (state, error) {
	if err := in.Validate(); err != nil {
		return state{}, goerr.Wrap(err, "planexec: invalid input")
	}
	return state{
		Phase: phasePlan, Round: 1, Input: in, NextInput: in.UserInput,
		// A host that already posted a progress message hands its id in, so the
		// run's first milestone updates that message instead of posting a second.
		Progress: progressState{MessageTS: in.Progress.MessageTS},
	}, nil
}

func (s *strategy[T]) Step(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output[T]], error) {
	// A budget notice has to be TOLD to the run, not merely enforced against it:
	// enforcement alone cuts it off mid-investigation having neither performed the
	// call its task needed nor said anything. It is recorded on the state rather
	// than acted on here, because the phase that can act may be several
	// transitions away — a round waiting for its children cannot abandon them.
	if sys.LimitStatus().Kind() == agentkit.LimitKindNotice {
		st.Reserve = true
	}

	switch st.Phase {
	case phasePlan:
		return s.stepPlan(ctx, sys, st)
	case phaseCollect:
		return s.stepCollect(ctx, sys, st)
	case phaseReplan:
		return s.stepReplan(ctx, sys, st)
	case phaseFinal:
		return s.stepFinal(ctx, sys, st)
	case phasePlannerTool:
		return s.stepPlannerTool(ctx, sys, st)
	case phaseAnswer:
		return s.stepAnswer(ctx, sys, st)
	default:
		return st, agentkit.Decision[Output[T]]{}, goerr.New("planexec: unknown phase",
			goerr.V("phase", st.Phase))
	}
}

// stepPlan makes the opening planner call and launches the first round.
func (s *strategy[T]) stepPlan(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output[T]], error) {
	if st.Reserve {
		// Nothing has been investigated, but the budget is already nearly gone.
		// Answering from an empty trail beats spending the remainder on a plan
		// that could never be executed.
		st.Phase = phaseFinal
		return st, agentkit.Continue[Output[T]](), nil
	}

	st = s.note(ctx, st, "🧭 Planning")

	// Read ONCE, before the generate, and used for both the line the planner is
	// told and the bound its plan is validated against. Re-reading it after the
	// call would validate against a figure smaller than the one the planner was
	// given — by the cost of that very call — and reject a plan that did exactly
	// what it was told. A call continuing from answered tool calls is told no
	// figure (see plannerInput) and is still validated against this fresh one.
	remaining, total, budgeted := s.remainingBudget(sys)

	prompt, err := s.plannerPrompt(st)
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, err
	}
	// The opening round may not ask a question: it has nothing to ask about yet.
	schema := planSchema(schemaOptions{
		knownToolIDs: st.Input.KnownToolIDs,
		allowDirect:  st.Input.AllowDirect,
		withBudget:   budgeted,
	})

	res, err := sys.Session().Generate(ctx, plannerInput(st, budgetLine(remaining, total, budgeted)), append([]agentkit.GenerateOption{
		agentkit.WithSystemPrompt(prompt),
		agentkit.WithSchema(schema),
		agentkit.WithRole(RolePlanner),
	}, toolCallOptions(st)...)...)
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: planner generate")
	}
	// This call continued from them, so the next one starts a fresh turn. Cleared
	// only after it succeeded: a failed transition is retried from the checkpoint,
	// and a retry has to continue from the conversation the same way.
	st.ToolsAnswered = false

	if next, diverted := s.divertToTools(st, res, phasePlan); diverted {
		return next, agentkit.Continue[Output[T]](), nil
	}

	plan, perr := parsePlanResult([]byte(strings.Join(res.Texts, "\n")),
		st.Input.KnownToolIDs, st.Input.AllowDirect, budgetBound(remaining, budgeted))
	if perr != nil {
		next, err := s.carryCorrection(ctx, st, perr, "planexec: planner output rejected")
		if err != nil {
			return st, agentkit.Decision[Output[T]]{}, err
		}
		return next, agentkit.Continue[Output[T]](), nil
	}

	st.PlannerToolRounds = 0
	switch {
	case plan.Finalize != nil:
		// The turn ends without a phase, through the same terminal call a
		// replan's finalize leads to.
		st.Phase = phaseFinal
		return st, agentkit.Continue[Output[T]](), nil
	case plan.Direct != nil:
		return s.launchDirect(ctx, sys, st, plan.Direct)
	default:
		return s.launchRound(ctx, sys, st, plan.Tasks)
	}
}

// plannerInput is the user turn a planning call sends. budgetLine is the
// allowance line to open it with, or "" for none.
//
// A call that follows the tool phase sends NOTHING, and that case is checked
// first. stepPlannerTool answered each call in the conversation, so the model's
// call turn is already answered and an empty input continues from there. Text
// would land as a second user turn behind the results, which is not what the
// model's call is waiting for. Nor does any text move into the system prompt
// instead, which stays the same for the whole run (see plannerPrompt): a call
// past plannerToolRoundsMax is stopped by tool_choice alone. The state that would
// mix the two does not arise: divertToTools clears NextInput on the way in. The
// allowance line is dropped with the rest: the planner reads the figure off the
// last turn that carried one (see budgetPrefix).
//
// Any other call must send a turn. gollem appends no user content for an empty
// input, so with no answered calls behind it the request would END on the previous
// model turn, which a provider rejects outright. With no NextInput either,
// plannerContinue keeps the request well-formed without re-asking the original
// request, which would ask the same question again as though nothing had been
// learnt.
func plannerInput(st state, budgetLine string) []gollem.Input {
	if st.ToolsAnswered {
		return nil
	}
	text := st.NextInput
	if text == "" {
		text = plannerContinue
	}
	return withBudgetLine(budgetLine, gollem.Text(text))
}

// withBudgetLine opens a user turn with the allowance line, followed by texts.
// Each is its own input rather than joined into one string so each text stays
// exactly what the caller built; every provider gollem supports puts consecutive
// text inputs in ONE user turn, so the request shape is unchanged.
func withBudgetLine(budgetLine string, texts ...gollem.Text) []gollem.Input {
	out := make([]gollem.Input, 0, len(texts)+1)
	if budgetLine != "" {
		out = append(out, gollem.Text(budgetLine))
	}
	for _, t := range texts {
		out = append(out, t)
	}
	return out
}

// plannerContinue is the user turn a planning call sends when it has nothing new
// to say. It must not restate the request: everything the planner needs is already
// in the conversation, and repeating it invites the same answer again.
const plannerContinue = "Continue from what you have so far, and give your next decision."

// plannerToolRoundsMax is the number of tool rounds a planning phase may take
// before the planner has to decide with what it has.
//
// It exists because the planner's tool calls are free of the round budget — they
// are not sub-agent work — so a model that answers every prompt with another
// lookup would otherwise only be stopped by the step ceiling, having produced
// nothing.
//
// Past it a call is sent with tool calls disabled (tool_choice "none"; see
// toolDirectiveFor), and is told nothing: the call that needs stopping is usually
// the one continuing from answered tool calls, which sends no user turn, and the
// system prompt stays the same for the whole run (see plannerPrompt). An
// instruction was never enough on its own anyway: a model told to stop went on
// calling tools, among them a Slack post whose only text was a placeholder, which
// ran and reached the case thread — twice in one run, once per planning phase
// that carried the notice.
//
// A call returned anyway is still run and answered (see divertToTools), since a
// provider is not bound to honour the setting. MaxSteps, which every one of these
// calls charges, remains the hard stop.
const plannerToolRoundsMax = 4

// divertToTools sends the run to the tool phase when the planner asked for tools
// instead of deciding, and reports whether it did.
//
// It diverts whenever there are calls — including past plannerToolRoundsMax. That
// is not optional: the model's function-call turn is already in the conversation,
// and a provider rejects the next request unless every call in it has a matching
// response ("the number of function response parts is equal to the number of
// function call parts"). Dropping the calls produced exactly that error, retried
// until the run failed. Past the bound the next call is sent with tool calls
// disabled, but a provider may still return a call, and that call is answered
// like any other.
func (s *strategy[T]) divertToTools(st state, res *agentkit.GenerateResult, from string) (state, bool) {
	if len(res.FunctionCalls) == 0 {
		return st, false
	}
	st.PendingCalls = res.FunctionCalls
	st.AfterTool = from
	st.PlannerToolRounds++
	st.Phase = phasePlannerTool
	// The next planning call continues from the conversation, which by then carries
	// the tool results; re-sending the original input would ask the same question
	// again as though nothing had been learnt.
	st.NextInput = ""
	return st, true
}

// stepPlannerTool runs exactly one tool call the planner asked for and answers it
// in the conversation.
//
// It goes through the session's CallTool, which appends the result into the
// trailing tool message when there is one. That is what keeps the N results of a
// parallel call turn in ONE turn, which is the only shape a provider accepts for
// them — a result per turn is rejected outright ("the number of function response
// parts is equal to the number of function call parts of the function call turn"),
// and once the conversation holds the split every later call in the run is
// rejected too.
//
// The arguments pass through toolargs.Coerce first, for the same reason they do
// in react.stepTool: it is the last point at which a single value sent for an
// array-typed argument can be read as the batch of one the model meant, rather
// than rejected by gollem before the tool sees it.
//
// A tool that fails is not a transition failure: the failure is recorded as this
// call's response, so the planner gets to react to it on its next call and the
// call is still answered. A refusal from the budget is the one error that must not
// be swallowed — continuing past it would spend beyond a ceiling its owner
// declared closed.
func (s *strategy[T]) stepPlannerTool(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output[T]], error) {
	if len(st.PendingCalls) == 0 {
		st.Phase = st.AfterTool
		return st, agentkit.Continue[Output[T]](), nil
	}

	call := st.PendingCalls[0]
	st.PendingCalls = st.PendingCalls[1:]
	if call != nil && st.Input.HandoffTool != nil && call.Name == st.Input.HandoffTool.Name {
		if err := st.Input.HandoffTool.ValidateArgs(call.Arguments); err != nil {
			return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: invalid handoff request")
		}
		reason, _ := call.Arguments["reason"].(string)
		return st, agentkit.Done(Output[T]{Kind: OutputHandoff, HandoffReason: reason, Observations: st.Observations}), nil
	}
	if call != nil {
		if _, err := sys.Session().CallTool(ctx, toolargs.Coerce(sys.Tools(), *call)); err != nil {
			if errors.Is(err, agentkit.ErrLimitExceeded) {
				return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err,
					"planexec: planner tool call refused by the budget",
					goerr.V("tool", call.Name))
			}
			// Reported as well as fed back: the planner needs the failure to react
			// to, and an operator needs it to tell a broken tool from a model that
			// chose not to use its result. An error tagged errutil.TagBenign — a
			// tool refusing what the planner sent — is demoted to an INFO log
			// inside Handle rather than skipped here; see react's stepTool.
			errutil.Handle(ctx, goerr.Wrap(err, "planexec: planner tool call",
				goerr.V("tool", call.Name), goerr.V("call_id", call.ID)),
				"planexec: planner tool call")
		}
		// Answered either way: a failed call is recorded in the conversation as an
		// error result, which is what the planner reacts to on its next call.
		st.ToolsAnswered = true
	}

	// A planner that has spent its allowance is stopped by the call this returns
	// to, which is sent with tool calls disabled (see toolCallOptions). It is not
	// told so: that call continues from the results and sends no user turn, and
	// the system prompt does not change within a run (see plannerPrompt).
	if len(st.PendingCalls) == 0 {
		st.Phase = st.AfterTool
	}
	return st, agentkit.Continue[Output[T]](), nil
}

// stepCollect folds the finished children into observations. It makes no LLM
// call, which is why it is its own transition: the children's results are
// committed before anything is asked of the model.
func (s *strategy[T]) stepCollect(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output[T]], error) {
	aw, ok := sys.Await(st.RoundKey)
	if !ok {
		return st, agentkit.Decision[Output[T]]{}, goerr.New("planexec: round await is missing",
			goerr.V("key", string(st.RoundKey)), goerr.V("round", st.Round))
	}
	if aw.Status != agentkit.AwaitResponded {
		return st, agentkit.Decision[Output[T]]{}, goerr.New("planexec: round await is not resolved",
			goerr.V("key", string(st.RoundKey)), goerr.V("status", string(aw.Status)))
	}

	if st.Direct {
		// The direct path's single child IS the answer, so its text is read straight
		// off the child rather than through collectResults: that path truncates at
		// subAgentSummaryMaxBytes to bound what is fed back into the planner's
		// context, and this text is not fed to the planner — it is the reply. A
		// user-facing message that ends in "…[truncated]" is a defect, not a bound.
		text := directReplyText(st.Current, aw.Results)
		if text == "" {
			// A direct child that produced nothing is a dead end, not an answer:
			// replying with an empty message would look like the agent ignored the
			// user.
			return st, agentkit.Done(Output[T]{
				Kind:           OutputFallback,
				FallbackReason: "the direct reply produced no text",
				Observations:   st.Observations,
			}), nil
		}
		st = s.note(ctx, st, "✅ Answered directly")
		return st, agentkit.Done(Output[T]{
			Kind: OutputDirect, Text: text, Observations: st.Observations,
		}), nil
	}

	results := collectResults(st.Current, aw.Results)
	tasks := make([]TaskPlan, 0, len(st.Current))
	for _, ref := range st.Current {
		tasks = append(tasks, TaskPlan{
			ID: ref.TaskID, Title: ref.Title, AcceptanceCriteria: ref.AcceptanceCriteria,
		})
	}
	st.Observations = appendObservations(st.Observations,
		PhaseSummary{Phase: st.Round, Tasks: tasks, Results: results})
	st = s.note(ctx, st, roundSummaryLine(st.Round, results))

	st.NextInput = formatObservationsAsUserTurn(tasks, results)
	st.Current = nil
	st.RoundKey = ""
	st.Phase = phaseReplan
	return st, agentkit.Continue[Output[T]](), nil
}

// stepReplan asks the planner what to do with the observations so far.
func (s *strategy[T]) stepReplan(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output[T]], error) {
	if st.Reserve {
		st.Phase = phaseFinal
		return st, agentkit.Continue[Output[T]](), nil
	}

	st = s.note(ctx, st, "🧭 Re-planning")

	// See stepPlan: read once, before the generate, and shared by the line the
	// planner is told and the bound its plan is validated against.
	remaining, total, budgeted := s.remainingBudget(sys)

	prompt, err := s.plannerPrompt(st)
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, err
	}
	schema := replanSchema(schemaOptions{
		knownToolIDs:  st.Input.KnownToolIDs,
		allowQuestion: st.Input.AllowQuestion,
		withBudget:    budgeted,
	})

	res, err := sys.Session().Generate(ctx, plannerInput(st, budgetLine(remaining, total, budgeted)), append([]agentkit.GenerateOption{
		agentkit.WithSystemPrompt(prompt),
		agentkit.WithSchema(schema),
		agentkit.WithRole(RolePlanner),
	}, toolCallOptions(st)...)...)
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: replanner generate")
	}
	st.ToolsAnswered = false // see stepPlan: cleared only once the call that continued from them succeeded

	if next, diverted := s.divertToTools(st, res, phaseReplan); diverted {
		return next, agentkit.Continue[Output[T]](), nil
	}

	rr, perr := parseReplanResult([]byte(strings.Join(res.Texts, "\n")),
		st.Input.KnownToolIDs, st.Input.AllowQuestion, budgetBound(remaining, budgeted))
	if perr != nil {
		next, err := s.carryCorrection(ctx, st, perr, "planexec: replanner output rejected")
		if err != nil {
			return st, agentkit.Decision[Output[T]]{}, err
		}
		return next, agentkit.Continue[Output[T]](), nil
	}

	st.PlannerToolRounds = 0
	switch {
	case rr.Question != nil:
		st = s.note(ctx, st, "❓ Asked a question")
		if st.Input.SuspendOnQuestion {
			return s.askAndWait(ctx, sys, st, rr.Question)
		}
		// The turn ENDS on a question rather than waiting on an await. Holding the
		// run open while a person takes minutes or hours would pin its subject and
		// block every later turn on the thread; the answer arrives as a fresh run
		// that inherits this one's history.
		return st, agentkit.Done(Output[T]{
			Kind: OutputQuestion, Question: rr.Question, Observations: st.Observations,
		}), nil
	case rr.Finalize != nil:
		st.Phase = phaseFinal
		return st, agentkit.Continue[Output[T]](), nil
	default:
		st.Round++
		return s.launchRound(ctx, sys, st, rr.Tasks)
	}
}

// RenderAnswers turns a human's answers into the user turn a suspended run
// continues from. The host calls it and passes the result to Kernel.Respond, so
// the encoding of an answer stays the host's business and planexec only sees a
// string.
//
// Each answer is labelled with the question it belongs to, because the planner
// sees them one round after it asked and an unlabelled list of values is
// ambiguous when several items were asked at once.
func RenderAnswers(q Question, answers []QuestionAnswer) string {
	var sb strings.Builder
	sb.WriteString("# User answers\n\n")
	byID := make(map[string]QuestionItem, len(q.Items))
	for _, it := range q.Items {
		byID[it.ID] = it
	}
	for _, ans := range answers {
		if item, ok := byID[ans.ID]; ok {
			fmt.Fprintf(&sb, "## %s — %s\n", ans.ID, item.Text)
		} else {
			fmt.Fprintf(&sb, "## %s\n(unknown question id; answer kept verbatim)\n", ans.ID)
		}
		switch {
		case ans.FreeText != "":
			fmt.Fprintf(&sb, "Answer (free_text): %s\n\n", ans.FreeText)
		case len(ans.Choices) > 0:
			fmt.Fprintf(&sb, "Answer (multi_select): %s\n\n", strings.Join(ans.Choices, ", "))
		case ans.Choice != "":
			fmt.Fprintf(&sb, "Answer (select): %s\n\n", ans.Choice)
		default:
			sb.WriteString("Answer: (none provided)\n\n")
		}
	}
	sb.WriteString("Use these answers to decide the next action.\n")
	return sb.String()
}

// askAndWait delivers the question and parks the run until it is answered.
//
// The question is handed to the host BEFORE the suspend so the host can record
// the await key next to whatever it posts — the answer arrives out of band, and
// without the key there is nothing to Respond to.
func (s *strategy[T]) askAndWait(ctx context.Context, sys agentkit.Syscalls, st state,
	q *Question,
) (state, agentkit.Decision[Output[T]], error) {
	if s.cfg.Asker == nil {
		return st, agentkit.Decision[Output[T]]{}, goerr.New(
			"planexec: a run that waits on its question needs an asker")
	}
	// Per-round, because agentkit closes an await once it is answered: a follow-up
	// question reusing the key would be responding to a closed one.
	key := agentkit.AwaitKey(fmt.Sprintf("question:%d", st.Round))
	if err := s.cfg.Asker.Ask(ctx, sys.ProcessID(), sys.Metadata(), key, *q); err != nil {
		return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: deliver the question")
	}

	payload, err := json.Marshal(q)
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: encode the question")
	}
	st.AnswerKey = key
	st.Phase = phaseAnswer
	return st, agentkit.Suspend[Output[T]](agentkit.Question(key, payload)), nil
}

// stepAnswer folds the human's answer into the next planning round.
//
// The answer is opaque: the host encodes whatever the person said as the user
// turn the planner should read next. planexec knows nothing about forms, options
// or Slack — only that a string came back.
func (s *strategy[T]) stepAnswer(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output[T]], error) {
	aw, ok := sys.Await(st.AnswerKey)
	if !ok {
		return st, agentkit.Decision[Output[T]]{}, goerr.New("planexec: the question await is missing",
			goerr.V("key", string(st.AnswerKey)))
	}
	if aw.Status != agentkit.AwaitResponded {
		return st, agentkit.Decision[Output[T]]{}, goerr.New("planexec: the question await is not answered",
			goerr.V("key", string(st.AnswerKey)), goerr.V("status", string(aw.Status)))
	}

	answer := strings.TrimSpace(string(aw.Response))
	if answer == "" {
		// An empty answer is not a reason to fail: the person may have submitted the
		// form with nothing filled in, and the planner can ask again or proceed.
		answer = "The user submitted the form without providing an answer."
	}
	st = s.note(ctx, st, "💬 Received the answer")
	st.AnswerKey = ""
	st.NextInput = answer
	st.Round++
	st.Phase = phaseReplan
	return st, agentkit.Continue[Output[T]](), nil
}

// stepFinal produces the terminal output.
func (s *strategy[T]) stepFinal(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output[T]], error) {
	st = s.note(ctx, st, "📝 Writing the answer")

	// The same system prompt as every planning call of the run (see plannerPrompt).
	// Nothing about this call is added to it, whichever branch below is taken.
	prompt, err := s.plannerPrompt(st)
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, err
	}

	// A terminal call reached straight from a tool phase sends no user turn: its
	// answered calls are the turn the model is waiting on, for the reason
	// plannerInput gives. The final prompt is not sent anywhere else on that branch.
	// What it would carry is already in the conversation or on the request:
	//
	//   - every observation trail was sent as a user turn: stepCollect hands
	//     formatObservationsAsUserTurn to the next replan call. A tool round can
	//     only follow a call that was made, so a run reaching here with
	//     ToolsAnswered set has made that replan call already. A replan that
	//     diverts here before making it has had no tool round since the round
	//     launched (ToolsAnswered is cleared by the plan or replan call that
	//     launched it), so it takes the other branch, whose final prompt carries
	//     the whole trail. A run reaching here from stepPlan has no observations.
	//   - the output's shape is the response schema below (WithSchema), and the
	//     language directive is in the system prompt. A text host has no output
	//     schema of its own, so this branch sends TextResult's (textOutput): with
	//     no final prompt in the conversation, the last instruction the model holds
	//     is the planner's "emit one decision object", and without a schema a reply
	//     like {"finalize": ...} would be returned as the user-facing text.
	//
	// The other branch sends the turn, opened by the allowance line: outside the
	// budget reserve this call may still ask for a tool, so it has to know what is
	// left to spend on one. A retry carries the rejection reason instead of the final prompt: the
	// model already has the request in the conversation.
	textOutput := s.cfg.TextOnly && st.ToolsAnswered
	var input []gollem.Input
	if !st.ToolsAnswered {
		userPrompt := st.NextInput
		if st.FinalRetries == 0 || userPrompt == "" {
			rendered, err := renderFinalUserPrompt(finalPromptInput{
				Observations:    renderObservationsForFinal(st.Observations),
				StructuredFinal: !s.cfg.TextOnly,
				Language:        st.Input.LanguageLabel,
			})
			if err != nil {
				return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: render final prompt")
			}
			userPrompt = rendered
		}
		input = withBudgetLine(budgetLine(s.remainingBudget(sys)), gollem.Text(userPrompt))
	}

	opts := []agentkit.GenerateOption{
		agentkit.WithSystemPrompt(prompt),
		agentkit.WithRole(RoleFinalizer),
	}
	switch {
	case !s.cfg.TextOnly:
		opts = append(opts, agentkit.WithSchema(s.outputSchema))
	case textOutput:
		opts = append(opts, agentkit.WithSchema(s.textSchema))
	}
	opts = append(opts, toolCallOptions(st)...)

	res, err := sys.Session().Generate(ctx, input, opts...)
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: final generate")
	}
	st.ToolsAnswered = false

	// The model may look something up before it can write the answer, and the
	// terminal call is no exception: agentkit's managed session declares the
	// claim's tools on EVERY call, so a model that has been calling tools for the
	// whole run can well answer this one with another call. Run them and come back
	// here, exactly as a planning phase does — reading that as "no answer" is what
	// turned a lookup into a fallback with nothing to say. The same round bound and
	// the step ceiling apply.
	if next, diverted := s.divertToTools(st, res, phaseFinal); diverted {
		return next, agentkit.Continue[Output[T]](), nil
	}

	body := strings.Join(res.Texts, "\n")
	if textOutput && strings.TrimSpace(body) != "" {
		var tr TextResult
		err := json.Unmarshal(extractJSONObject([]byte(body)), &tr)
		if err == nil && strings.TrimSpace(tr.Text) == "" {
			// A JSON object with no text is not an answer. The usual one is a planner
			// decision ({"finalize": ...}): with no final prompt on this call, the last
			// instruction the model holds is the planner's.
			err = goerr.New("the text output has no text")
		}
		if err != nil {
			st.FinalRetries++
			if st.FinalRetries > finalOutputMaxRetry {
				return st, agentkit.Done(Output[T]{
					Kind:           OutputFallback,
					FallbackReason: err.Error(),
					Observations:   st.Observations,
				}), nil
			}
			errutil.Handle(ctx, goerr.Wrap(err, "planexec: text output rejected",
				goerr.T(errutil.TagBenign)), "planexec: text output rejected")
			// Empty, so the retry sends the full final prompt, which asks for plain
			// text: the retry call sends a user turn and no schema, and a correction
			// asking it to re-emit JSON would contradict that.
			st.NextInput = ""
			return st, agentkit.Continue[Output[T]](), nil
		}
		body = tr.Text
	}
	if strings.TrimSpace(body) == "" {
		return st, agentkit.Done(Output[T]{
			Kind:           OutputFallback,
			FallbackReason: "the final response was empty",
			Observations:   st.Observations,
		}), nil
	}

	if s.cfg.TextOnly {
		return st, agentkit.Done(Output[T]{
			Kind: OutputFinal, Text: body, Observations: st.Observations,
		}), nil
	}

	out, derr := s.decodeFinal([]byte(body))
	if derr == nil {
		derr = (*out).Validate()
	}
	if derr == nil {
		derr = s.runFinalizers(ctx, sys.Metadata(), out)
	}
	if derr != nil {
		st.FinalRetries++
		if st.FinalRetries > finalOutputMaxRetry {
			return st, agentkit.Done(Output[T]{
				Kind:           OutputFallback,
				FallbackReason: derr.Error(),
				Observations:   st.Observations,
			}), nil
		}
		// Same discipline as a rejected plan: the correction goes to the next
		// transition, not to a loop inside this one.
		errutil.Handle(ctx, goerr.Wrap(derr, "planexec: final output rejected",
			goerr.T(errutil.TagBenign)), "planexec: final output rejected")
		retry, err := renderFinalRetryInput(derr)
		if err != nil {
			return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: build the terminal output correction")
		}
		st.NextInput = retry
		return st, agentkit.Continue[Output[T]](), nil
	}

	return st, agentkit.Done(Output[T]{
		Kind: OutputFinal, Data: out, Observations: st.Observations,
	}), nil
}

// carryCorrection records a rejected planner output and puts the reason in front
// of the next planner round.
//
// The retry is deliberately a separate transition rather than a loop inside this
// one: one transition is one LLM call, so a retry loop here would spend the
// budget without checkpointing between attempts, and a crash would repeat the
// whole thing.
func (s *strategy[T]) carryCorrection(ctx context.Context, st state, cause error, msg string) (state, error) {
	errutil.Handle(ctx, goerr.Wrap(cause, msg, goerr.T(errutil.TagBenign)), msg)
	retry, err := renderPlanRetryInput(cause)
	if err != nil {
		return st, goerr.Wrap(err, "planexec: build the planner correction")
	}
	st.NextInput = retry
	return st, nil
}

// runFinalizers applies the host's validators to a decoded terminal output. The
// first rejection wins: a later finalizer would be judging an output that is
// already going to be regenerated.
func (s *strategy[T]) runFinalizers(ctx context.Context, meta map[string]string, out *T) error {
	for _, fin := range s.cfg.Finalizers {
		if fin == nil {
			continue
		}
		if err := fin(ctx, meta, out); err != nil {
			return goerr.Wrap(err, "final output rejected by finalizer")
		}
	}
	return nil
}

// decodeFinal turns the terminal JSON into T, through the host's decoder when it
// supplied one.
func (s *strategy[T]) decodeFinal(raw []byte) (*T, error) {
	body := extractJSONObject(raw)
	if s.cfg.Decode != nil {
		out, err := s.cfg.Decode(body)
		if err != nil {
			return nil, goerr.Wrap(err, "decode the final output")
		}
		if out == nil {
			return nil, goerr.New("the final output decoder returned nothing")
		}
		return out, nil
	}
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, goerr.Wrap(err, "decode the final output json")
	}
	return &out, nil
}

// launchRound spawns one child per task and suspends until they all finish.
func (s *strategy[T]) launchRound(ctx context.Context, sys agentkit.Syscalls, st state, tasks []TaskPlan) (state, agentkit.Decision[Output[T]], error) {
	if len(tasks) == 0 {
		// Plan validation rejects an empty task list, so reaching here means the
		// planner emitted a shape no branch handles. Finishing from what we have
		// beats another round that would ask the same question again.
		st.Phase = phaseFinal
		return st, agentkit.Continue[Output[T]](), nil
	}
	if len(tasks) > maxTasksPerPhase {
		tasks = tasks[:maxTasksPerPhase]
	}

	_, _, budgeted := s.remainingBudget(sys)

	refs := make([]taskRef, 0, len(tasks))
	ids := make([]agentkit.ProcessID, 0, len(tasks))
	for _, task := range tasks {
		prompt, err := buildSubAgentSystemPrompt(task, st.Input.AllowSubAgentWrites, st.Input.TaskContext)
		if err != nil {
			return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err,
				"planexec: build the task prompt", goerr.V("task_id", task.ID))
		}
		description := task.Description
		if description == "" {
			description = task.Title
		}
		// Validated as positive and as summing to no more than the remaining
		// allowance, by the parser that produced these tasks.
		allowance := budgetBound(pricing.FromUSD(task.BudgetUSD), budgeted)
		pid, err := s.spawnChild(ctx, sys, task.ID,
			react.Input{SystemPrompt: prompt, Prompt: description}, task.Tools, allowance)
		if err != nil {
			return st, agentkit.Decision[Output[T]]{}, err
		}
		refs = append(refs, taskRef{
			TaskID: task.ID, Title: task.Title,
			AcceptanceCriteria: task.AcceptanceCriteria, ProcessID: pid,
		})
		ids = append(ids, pid)
	}

	st = s.note(ctx, st, fmt.Sprintf("🔎 Investigating (%d task(s))", len(refs)))
	st.Current = refs
	st.Direct = false
	st.RoundKey = agentkit.AwaitKey(fmt.Sprintf("tasks:%d", st.Round))
	st.Phase = phaseCollect
	return st, agentkit.Suspend[Output[T]](agentkit.WaitChildren(st.RoundKey, ids...)), nil
}

// directTaskID labels the direct path's single child on the round's taskRef and in
// a spawn error. It is not a planner-assigned task id — the direct path has no
// plan — so it must not collide with one.
const directTaskID = "direct"

// launchDirect spawns the single child that answers a trivial request outright.
//
// Its system prompt is built from prompts/direct.md, NOT from the sub-agent
// template every other child gets: stepCollect publishes this child's text to the
// user verbatim, so it is the author of a user-facing message rather than an
// investigator reporting to the planner. See buildDirectSystemPrompt for what
// reusing the sub-agent prompt here actually produced.
func (s *strategy[T]) launchDirect(ctx context.Context, sys agentkit.Syscalls, st state, plan *DirectPlan) (state, agentkit.Decision[Output[T]], error) {
	prompt, err := buildDirectSystemPrompt(directPromptInput{
		HostPrompt: st.Input.SystemPrompt,
		Language:   st.Input.LanguageLabel,
		Context:    st.Input.TaskContext,
	})
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: build the direct prompt")
	}
	// The direct path spawns ONE child whose text is the turn's reply, so there is
	// nothing to divide the remaining allowance between and no planner decision to
	// respect: it gets all of it.
	remaining, _, budgeted := s.remainingBudget(sys)
	pid, err := s.spawnChild(ctx, sys, directTaskID,
		react.Input{SystemPrompt: prompt, Prompt: st.Input.UserInput}, plan.Tools,
		budgetBound(remaining, budgeted))
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, err
	}
	st.Current = []taskRef{{TaskID: directTaskID, Title: "Direct reply", ProcessID: pid}}
	st.Direct = true
	st.RoundKey = agentkit.AwaitKey(directTaskID)
	st.Phase = phaseCollect
	return st, agentkit.Suspend[Output[T]](agentkit.WaitChildren(st.RoundKey, pid)), nil
}

// spawnChild launches one child Process. label names it in a spawn error, and
// tools is the toolset subset it may call.
//
// The prompts arrive as a built react.Input rather than as two adjacent strings
// because the two callers differ precisely in which template each half comes from
// — a planned task gets the sub-agent system prompt, the direct path gets the
// user-facing reply one — and a swap between the two would compile, then be
// visible only in what the model wrote.
func (s *strategy[T]) spawnChild(ctx context.Context, sys agentkit.Syscalls,
	label string, input react.Input, tools []string, budget *pricing.NanoUSD,
) (agentkit.ProcessID, error) {
	// WithToolSets rebuilds the parent's metadata map with only the toolsets
	// replaced. SpawnChild's WithMetadata REPLACES the map rather than merging
	// into it, so building a fresh one here would drop the workspace and case the
	// child needs to have any tools at all.
	meta := agentkernel.WithToolSets(sys.Metadata(), tools)
	if scope := agentkernel.ScopeFrom(meta); scope.JobExecution != nil &&
		(scope.StageRole != agentkernel.StageRoleAdvisor || scope.JobExecution.Mode == model.JobExecutionModeOrchestrator) {
		meta = agentkernel.WithStageRole(meta, agentkernel.StageRoleSub)
	}
	// A nil budget leaves the parent's own figure in place, which is what a host
	// that wired no Config.Remaining gets: the child is judged against the run's
	// budget on its own metrics.
	if budget != nil {
		meta = agentkernel.WithBudget(meta, *budget)
	}
	pid, err := s.taskAgent.SpawnChild(ctx, sys, input, agentkit.WithMetadata(meta))
	if err != nil {
		return "", goerr.Wrap(err, "planexec: spawn the task", goerr.V("task_id", label))
	}
	return pid, nil
}

// directReplyText reads the direct path's single child's text, in full. An empty
// return means there is no reply — the child is missing, failed, or produced
// nothing — which stepCollect turns into a fallback.
//
// It bypasses collectResults deliberately: that path bounds each summary at
// subAgentSummaryMaxBytes because the summaries are fed back into the planner's
// context, and appends an "…[truncated]" marker. This text is the reply itself,
// so the bound buys nothing and the marker would be published to the user.
func directReplyText(refs []taskRef, children []agentkit.ChildResult) string {
	if len(refs) == 0 {
		return ""
	}
	for _, child := range children {
		if child.ProcessID != refs[0].ProcessID || child.Status != agentkit.ProcessSucceeded {
			continue
		}
		out, err := react.DecodeOutput(child.Output)
		if err != nil {
			return ""
		}
		return out.Text()
	}
	return ""
}

// collectResults turns the children's outcomes into TaskResults, in plan order.
func collectResults(refs []taskRef, children []agentkit.ChildResult) []TaskResult {
	byID := make(map[agentkit.ProcessID]agentkit.ChildResult, len(children))
	for _, c := range children {
		byID[c.ProcessID] = c
	}

	out := make([]TaskResult, 0, len(refs))
	for _, ref := range refs {
		res := TaskResult{
			TaskID: ref.TaskID, Title: ref.Title,
			AcceptanceCriteria: ref.AcceptanceCriteria,
		}
		child, ok := byID[ref.ProcessID]
		switch {
		case !ok:
			// The await resolved without this child. Reporting it as failed keeps
			// the planner's picture complete rather than silently short.
			res.Status = TaskStatusFailed
			res.Error = "the task produced no result"
		case child.Status == agentkit.ProcessSucceeded:
			text, err := react.DecodeOutput(child.Output)
			if err != nil {
				res.Status = TaskStatusFailed
				res.Error = "the task output could not be read: " + err.Error()
				break
			}
			res.Status = TaskStatusCompleted
			res.Summary = truncateSummary(text.Text())
		default:
			res.Status = TaskStatusFailed
			res.Error = childFailureMessage(child)
		}
		out = append(out, res)
	}
	return out
}

// childFailureMessage describes why a child did not succeed.
func childFailureMessage(child agentkit.ChildResult) string {
	if child.Failure != nil && child.Failure.Message != "" {
		return child.Failure.Message
	}
	return "the task ended as " + string(child.Status)
}

// appendObservations adds a round to the trail and drops the oldest rounds once
// the trail outgrows what a checkpoint can hold.
func appendObservations(trail []PhaseSummary, next PhaseSummary) []PhaseSummary {
	trail = append(trail, next)
	for len(trail) > 1 && observationsSize(trail) > observationsMaxBytes {
		dropped := trail[0]
		trail = trail[1:]
		// The note replaces the dropped round rather than deleting it silently, so
		// the planner can tell "nothing was found" from "that round is no longer in
		// front of me".
		trail[0].Tasks = append([]TaskPlan{{
			ID:    fmt.Sprintf("dropped-%d", dropped.Phase),
			Title: fmt.Sprintf("round %d was dropped to stay within the state size limit", dropped.Phase),
		}}, trail[0].Tasks...)
	}
	return trail
}

// observationsSize measures the trail the way the checkpoint will store it.
func observationsSize(trail []PhaseSummary) int {
	raw, err := json.Marshal(trail)
	if err != nil {
		// Unmeasurable means "assume it is too big": under-reporting would let the
		// state grow past what the store accepts, which strands the run.
		return observationsMaxBytes + 1
	}
	return len(raw)
}

// roundSummaryLine renders one round's outcome as a milestone.
func roundSummaryLine(round int, results []TaskResult) string {
	done, failed := 0, 0
	for _, r := range results {
		if r.Status == TaskStatusCompleted {
			done++
			continue
		}
		failed++
	}
	if failed == 0 {
		return fmt.Sprintf("✅ Round %d: %d task(s) done", round, done)
	}
	return fmt.Sprintf("⚠️ Round %d: %d done, %d failed", round, done, failed)
}

// budgetPrefix renders the line that tells the planner what the run may still
// spend, and out of what.
//
// It opens the USER turn of every planner and terminal call that sends one (see
// plannerInput), and is never put in the system prompt. The figure changes on
// nearly every call, and a provider's prompt cache matches an exact prefix that
// runs tools → system → messages: a figure in the system prompt made every call
// rewrite the whole conversation into the cache instead of reading what the
// previous call had already written. In a user turn it becomes part of the
// conversation, which only ever grows by appending, so earlier calls' turns stay
// a byte-identical prefix.
//
// The cost is that a call continuing from answered tool calls sends no user turn
// (§ "a parallel tool-call turn is answered in ONE call") and is therefore told
// no new figure: the planner still reads the line of the last turn that carried
// one, while its plan is validated against a figure read afresh for that call —
// lower by what the planning phase's own lookups cost. prompts/planner.md tells
// the planner so.
//
// It replaces budget.Config.Prefix, which was declared and never called while
// prompts/planner.md described the format it would have produced — so the
// planner was told to plan against a line it never received.
func budgetPrefix(remaining, total pricing.NanoUSD) string {
	return fmt.Sprintf("[budget] remaining %s of %s", remaining.USD(), total.USD())
}

// remainingBudget answers what this run may still spend, and whether the host
// asked for per-task budgets at all. ok is false when Config.Remaining is nil.
//
// It is read at the moment of the call rather than carried on the checkpointed
// state, because a round's children can spend a great deal between two of the
// parent's transitions and a stale figure would be divided up as if they had not.
func (s *strategy[T]) remainingBudget(sys agentkit.Syscalls) (remaining, total pricing.NanoUSD, ok bool) {
	if s.cfg.Remaining == nil {
		return 0, 0, false
	}
	remaining, total = s.cfg.Remaining(sys.Metadata(), sys.Metrics())
	// Floored to the cent because this ONE figure is both shown to the planner and
	// enforced against its plan. A raw amount is shown rounded to the nearest cent
	// and can therefore read higher than it is, and a plan allocating exactly what
	// the planner was told it had would then be rejected — costing a planner call
	// per attempt, paid out of the very allowance being divided.
	return remaining.FloorCent(), total, true
}

// budgetLine is what the planner is told about its allowance, or "" when the host
// wired none.
func budgetLine(remaining, total pricing.NanoUSD, ok bool) string {
	if !ok {
		return ""
	}
	return budgetPrefix(remaining, total)
}

// budgetBound turns remainingBudget's pair into what the parsers take: a pointer
// that is nil when per-task budgets are off. It exists so the nil is produced in
// ONE place — a caller writing `&remaining` on the wrong branch would enforce a
// zero allowance and reject every plan.
func budgetBound(remaining pricing.NanoUSD, ok bool) *pricing.NanoUSD {
	if !ok {
		return nil
	}
	return &remaining
}

// plannerPrompt renders the planner system prompt for the run's input.
//
// It is rendered from st.Input and the strategy's configuration alone, so every
// planning and terminal call of one run passes it byte for byte the same. The
// sameness is a requirement, not a convenience: Claude checks a thinking block against the
// system prompt, tools and messages that preceded it, so a system prompt changed
// mid-run invalidates every thinking block the conversation already holds, and
// it also discards the cached prefix of every later call. Nothing that varies
// within a run goes here — the allowance line rides on the user turn (see
// budgetPrefix), a "call no more tools" decision is sent as tool_choice
// (toolCallOptions), and the terminal output's shape as the response schema
// (stepFinal).
//
// What this guarantees is the system prompt planexec passes to gollem. For a
// Claude model without structured outputs, gollem states the response schema in
// a user message and sends the system prompt unchanged, so the differing plan,
// replan and terminal schemas do not change it on the wire either.
//
// AllocatesBudget follows Config.Remaining, the same condition that puts
// `budget_usd` into the schema, so the instruction and the field it asks for
// cannot appear without each other.
func (s *strategy[T]) plannerPrompt(st state) (string, error) {
	prompt, err := renderPlannerSystemPrompt(plannerPromptInput{
		HostPrompt:          st.Input.SystemPrompt,
		Language:            st.Input.LanguageLabel,
		KnownToolIDs:        st.Input.KnownToolIDs,
		AllowQuestion:       st.Input.AllowQuestion,
		AllowDirect:         st.Input.AllowDirect,
		StructuredFinal:     !s.cfg.TextOnly,
		AllowSubAgentWrites: st.Input.AllowSubAgentWrites,
		AllocatesBudget:     s.cfg.Remaining != nil,
		PlannerToolRounds:   plannerToolRoundsMax,
	})
	if err != nil {
		return "", goerr.Wrap(err, "planexec: render the planner prompt")
	}
	return prompt, nil
}

// toolDirective is whether a planning or terminal call may call tools.
//
// It carries no instruction text. The call that most needs to stop calling tools
// is the one continuing from answered tool calls, which sends no user turn, and
// the system prompt must stay the same for the whole run (see plannerPrompt), so
// there is no place left to put one. tool_choice "none" is the whole of the
// mechanism, and it does not depend on the model reading anything.
type toolDirective struct {
	disableToolCalls bool
}

// toolDirectiveFor decides a call's toolDirective from the state, in ONE place,
// so every call made from the same state is sent the same way.
//
// Inside the budget reserve every call is the terminal one (stepPlan and
// stepReplan divert to phaseFinal there), and it may call no tool. The planner
// is declared only read tools, so a call there could only look something up,
// and the reserve is spent on writing the output. A write the turn still needs
// is a task, and the reserve launches none: a sub-agent spends up to its own
// ceiling, which a tenth of the budget does not cover.
func toolDirectiveFor(st state) toolDirective {
	switch {
	case st.Reserve:
		return toolDirective{disableToolCalls: true}
	case st.PlannerToolRounds >= plannerToolRoundsMax:
		return toolDirective{disableToolCalls: true}
	default:
		return toolDirective{}
	}
}

// toolCallOptions is the generate option a planning or terminal call adds for
// its toolDirective: tool calls disabled (tool_choice "none") when the call may
// make none, nothing otherwise.
//
// The tools stay declared on the request. Withholding them would change the tool
// list, which invalidates the provider's cached prefix for the whole request (and
// Claude rejects a history carrying thinking blocks under a different tool list);
// changing tool_choice invalidates neither.
func toolCallOptions(st state) []agentkit.GenerateOption {
	var opts []agentkit.GenerateOption
	if st.Input.HandoffTool != nil {
		opts = append(opts, agentkit.WithTools(tool.NewControl(*st.Input.HandoffTool)))
	}
	if toolDirectiveFor(st).disableToolCalls {
		opts = append(opts, agentkit.WithLLMOptions(gollem.WithToolCallsDisabled()))
	}
	return opts
}

// note draws a milestone as the whole of the run's progress message, replacing
// whatever it showed before. Drawing is observability: a failure leaves the
// thread without that progress line but must never fail the turn.
func (s *strategy[T]) note(ctx context.Context, st state, line string) state {
	if line == "" {
		return st
	}
	if s.progress == nil || st.Input.Progress.isZero() {
		return st
	}
	// KNOWN LIMITATION, deliberately accepted, for a run that posts its OWN
	// progress message. The draw happens inside the same transition as the LLM
	// call that follows it, so a call that errors leaves a message Slack has
	// already accepted while the id it returned is discarded with the uncommitted
	// state. The retry then posts a second message, and the user sees the run's
	// progress twice.
	//
	// A run whose host handed it a message id (ProgressTarget.MessageTS) cannot
	// hit this: the id is part of the input, so the retry re-reads the same one
	// and updates the same message.
	//
	// Making the draw its own committed transition would fix the rest, at the cost
	// of a checkpoint write per progress line — roughly doubling the state writes
	// of every run. Skipping the draw on a retry does NOT fix it: the first
	// attempt's message is already posted, and the retry would simply post the
	// next line as a second message instead. Since the damage is one duplicated
	// progress message — no duplicated reply, case write or tool call — the write
	// cost is not worth paying. Revisit if Slack ever offers an idempotency key on
	// postMessage.
	ts, err := s.progress.Render(ctx, st.Input.Progress, st.Progress.MessageTS, line)
	if err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "planexec: draw the progress message"),
			"planexec: draw the progress message")
		return st
	}
	if ts != "" {
		st.Progress.MessageTS = ts
	}
	return st
}

func (s *strategy[T]) EncodeState(st state) ([]byte, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, goerr.Wrap(err, "planexec: encode state")
	}
	return raw, nil
}

func (s *strategy[T]) DecodeState(_ int, raw []byte) (state, error) {
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return state{}, goerr.Wrap(err, "planexec: decode state")
	}
	return st, nil
}

func (s *strategy[T]) EncodeOutput(out Output[T]) ([]byte, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, goerr.Wrap(err, "planexec: encode output")
	}
	return raw, nil
}

// DecodeOutput reads back the bytes a finished Process stored. A host reading a
// completed run's output needs this; the kernel itself never calls it.
func DecodeOutput[T Validatable](raw []byte) (Output[T], error) {
	var out Output[T]
	if err := json.Unmarshal(raw, &out); err != nil {
		return Output[T]{}, goerr.Wrap(err, "planexec: decode output")
	}
	return out, nil
}
