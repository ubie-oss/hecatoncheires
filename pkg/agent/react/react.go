// Package react is the tool-calling agent loop, written as an agentkit
// Strategy: generate, run the tool calls the model asked for, feed the results
// back, repeat until it answers without asking for a tool.
//
// It splits that loop finer than a conventional implementation would: one
// transition is either ONE LLM call or ONE tool call, never both and never
// several. Every transition is checkpointed before the next starts, so a crash
// costs at most one call, and a run can move between instances at any point.
//
// The bundled agentkit strategy/simple is deliberately not used. It fixes the
// system prompt at registration time (this application's prompts are built per
// case), and it runs every pending tool call inside one transition (which is
// the opposite of the split above).
//
// It lives under pkg/agent rather than pkg/usecase because it is runtime
// machinery the kernel drives: it holds no business rule, reaches no
// repository, and its method set is fixed by agentkit.Strategy — Init, Step,
// EncodeState and the rest take what the library passes them, not a request
// context.
package react

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/toolargs"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

// Phases. A transition reads the phase, does one thing, and writes the phase it
// leaves behind.
const (
	// phaseGenerate asks the model what to do next.
	phaseGenerate = "generate"
	// phaseTool runs one tool call the model asked for.
	phaseTool = "tool"
)

// Input is the launch input. Both fields are required: this strategy knows
// nothing about cases or workspaces, so the host supplies the finished prompts.
type Input struct {
	// SystemPrompt is the agent's persistent context.
	SystemPrompt string `json:"system_prompt"`
	// Prompt is the first user message.
	Prompt string `json:"prompt"`
	// OutputSchema is a JSON Schema the final answer must match. Only a run
	// driven by an external harness (pkg/agent/harness) honours it; this
	// strategy's own loop answers in prose and ignores it.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
}

// Output is what a finished run produces.
type Output struct {
	// Texts is every text block the model emitted across the run, in order.
	Texts []string `json:"texts"`
}

// Text joins the emitted blocks into the single reply a host posts.
func (o Output) Text() string { return strings.Join(o.Texts, "\n") }

// state is the checkpointed state. Everything the next transition needs must be
// here: Step runs from the top every time, including after a crash.
type state struct {
	Phase        string `json:"phase"`
	SystemPrompt string `json:"system_prompt"`
	// Prompt is cleared once it has been sent. A later Generate continues from
	// the conversation instead, which is what follows a tool result.
	Prompt string `json:"prompt,omitempty"`
	// Pending are the tool calls the model asked for and this run has not made
	// yet. They are consumed one per transition.
	Pending []*gollem.FunctionCall `json:"pending,omitempty"`
	// ToolsAnswered records that the drained Pending calls were answered in the
	// conversation, so the next Generate continues from it and sends no input.
	ToolsAnswered bool     `json:"tools_answered,omitempty"`
	Texts         []string `json:"texts,omitempty"`
	// ReserveToolRounds counts how many times the model asked for tools AFTER the
	// budget crossed into its reserve. It decides which of the two reserve
	// instructions the next call carries, so it has to survive a handoff like
	// everything else the next transition needs.
	ReserveToolRounds int `json:"reserve_tool_rounds,omitempty"`
	// OutputSchema is Input.OutputSchema, carried for a harness-driven run.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
}

// Option configures the strategy.
type Option func(*config)

type config struct {
	role agentkit.ModelRole
}

// WithRole selects the model role every Generate runs under. Omitting it uses
// the Kernel's default model.
func WithRole(r agentkit.ModelRole) Option {
	return func(c *config) { c.role = r }
}

// Register registers the strategy under name and returns the typed handle.
//
// limiter is the budget this agent answers Limit with; it is required, because
// a run with no ceiling is a run that can spend without bound.
func Register(reg *agentkit.Registry, name agentkit.AgentName, version int,
	limiter agentkit.Limiter, opts ...agentkit.RegisterOption[Output], // nolint:revive // options mirror agentkit.Register
) (agentkit.Agent[Input], error) {
	return RegisterWithOptions(reg, name, version, limiter, nil, opts...)
}

// RegisterWithOptions is Register plus strategy options.
func RegisterWithOptions(reg *agentkit.Registry, name agentkit.AgentName, version int,
	limiter agentkit.Limiter, strategyOpts []Option, opts ...agentkit.RegisterOption[Output],
) (agentkit.Agent[Input], error) {
	if limiter == nil {
		return agentkit.Agent[Input]{}, goerr.Wrap(agentkit.ErrInvalidAgentDef,
			"react: a limiter is required", goerr.V("agent", name))
	}
	var cfg config
	for _, o := range strategyOpts {
		o(&cfg)
	}
	return agentkit.Register(reg, name, version, &strategy{limiter: limiter, role: cfg.role, version: version}, opts...)
}

type strategy struct {
	limiter agentkit.Limiter
	role    agentkit.ModelRole
	version int
}

func (s *strategy) Version() int { return s.version }

func (s *strategy) Limit(ctx context.Context, proc *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
	return s.limiter(ctx, proc, m)
}

func (s *strategy) Init(in Input) (state, error) {
	if in.SystemPrompt == "" {
		return state{}, goerr.New("react: system prompt is required")
	}
	if in.Prompt == "" {
		return state{}, goerr.New("react: prompt is required")
	}
	return state{
		Phase: phaseGenerate, SystemPrompt: in.SystemPrompt, Prompt: in.Prompt,
		OutputSchema: in.OutputSchema,
	}, nil
}

func (s *strategy) Step(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output], error) {
	// An installed harness runs the whole loop in one transition; see stepHarness.
	if h := harness.FromContext(ctx); h != nil {
		return s.stepHarness(ctx, sys, st, h)
	}
	switch st.Phase {
	case phaseGenerate:
		return s.stepGenerate(ctx, sys, st)
	case phaseTool:
		return s.stepTool(ctx, sys, st)
	default:
		return st, agentkit.Decision[Output]{}, goerr.New("react: unknown phase", goerr.V("phase", st.Phase))
	}
}

// stepGenerate makes the one LLM call this transition is allowed.
func (s *strategy) stepGenerate(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output], error) {
	// A call that follows the tool phase sends NOTHING: stepTool answered each call
	// in the conversation, and an empty input continues from it — which is the shape
	// a provider requires, since the results already answer the model's call turn.
	// Text here would land as a second user turn behind them, which is not what the
	// model's call is waiting for.
	//
	// Anything else must send a turn: with no answered calls behind it, gollem
	// appends no user content for an empty input, so the request would END on the
	// previous model turn and the provider rejects that outright.
	var input []gollem.Input
	switch {
	case st.Prompt != "":
		input = []gollem.Input{gollem.Text(st.Prompt)}
	case !st.ToolsAnswered:
		input = []gollem.Input{gollem.Text(continueInstruction)}
	}

	// A budget that is close to its ceiling has to be TOLD to the model, not
	// merely enforced against it. Enforcement alone produces the worst outcome:
	// the run is cut off mid-investigation with nothing done and nothing said.
	// Given the notice, the model can spend what is left on the call the task
	// still needs and then on a result.
	//
	// It rides in the system prompt for this call, not as an input: the crossing
	// happens mid-run, exactly when a turn is likely to be reporting tool results,
	// and such a turn may carry nothing else.
	// Read once and reuse: LimitStatus moves in lockstep with Metrics and is
	// re-evaluated around every effect, so two reads either side of one are two
	// different verdicts. Nothing between here and the increment below performs an
	// effect today, and capturing keeps it that way if something later does.
	notice := sys.LimitStatus()
	inReserve := notice.Kind() == agentkit.LimitKindNotice
	systemPrompt := st.SystemPrompt
	var directive reserveDirective
	if inReserve {
		directive = reserveDirectiveFor(notice.Message(), st.ReserveToolRounds)
		systemPrompt += "\n\n" + directive.instruction
	}

	opts := []agentkit.GenerateOption{agentkit.WithSystemPrompt(systemPrompt)}
	if s.role != nil {
		opts = append(opts, agentkit.WithRole(s.role))
	}
	if directive.disableToolCalls {
		opts = append(opts, agentkit.WithLLMOptions(gollem.WithToolCallsDisabled()))
	}

	res, err := sys.Session().Generate(ctx, input, opts...)
	if err != nil {
		return st, agentkit.Decision[Output]{}, goerr.Wrap(err, "react: generate")
	}
	// This call continued from them, so the next one starts a fresh turn. Cleared
	// only after it succeeded: a failed transition is retried from the checkpoint,
	// and a retry has to continue from the conversation the same way.
	st.ToolsAnswered = false

	st.Prompt = ""
	st.Texts = append(st.Texts, res.Texts...)

	if len(res.FunctionCalls) == 0 {
		return st, agentkit.Done(Output{Texts: st.Texts}), nil
	}

	if inReserve {
		st.ReserveToolRounds++
	}
	st.Pending = res.FunctionCalls
	st.Phase = phaseTool
	return st, agentkit.Continue[Output](), nil
}

// continueInstruction is the user turn a call sends when it has nothing new to
// say. It must not restate the request: everything the model needs is already in
// the conversation, and repeating it invites the same answer again.
const continueInstruction = "Continue from what you have so far."

// reserveToolRoundsMax is how many tool rounds the reserve pays for: one. Past
// it the model is told to stop asking AND the call is sent with tool calls
// disabled (tool_choice "none"): the instruction alone did not stop a model,
// which went on calling tools — a side-effecting post among them — after it had
// been told not to. The tools stay declared on the request; only calling them is
// forbidden, so the cached tool and system prefix stays valid.
//
// A call made anyway is still run and answered — see stepTool — because a
// provider is not bound to honour the setting and an unanswered function call
// breaks the conversation for every later request.
const reserveToolRoundsMax = 1

// reserveInstruction is what the model is told on the first call after the run
// crossed into its budget reserve.
//
// It names ONE action and offers no alternative. The conditional it replaced
// ("if something is outstanding call it, otherwise answer now") handed the model
// a way to skip straight to the answer with the side effect the task was for
// still unperformed — which is the exact outcome the reserve exists to prevent.
// Nothing here can force the call: a model that answers with text instead ends
// the run, and re-asking it would spend the reserve on asking.
const reserveInstruction = "This run has spent its working budget; only a small reserve is left. " +
	"Do not start any new investigation. THIS turn is your final tool call: make the one call this " +
	"task still needs in order to take effect. Do not write your result on this turn — the call's " +
	"outcome comes back on the next turn, and that is where you write it."

// reserveSpentInstruction is what the model is told once the reserve's one tool
// round has been spent. It asks for the result to be SHORT rather than complete:
// what is left has to cover this call and nothing more.
const reserveSpentInstruction = "The budget reserve is spent: you have already made your final " +
	"tool call. Do not call any tool again and do not look anything else up. Write your result now " +
	"from what you already have, as briefly as it can be understood."

// reserveDirective is what one call inside the reserve is told, and whether it
// may call tools at all. The two are decided together so that the call told
// "do not call any tool again" is the call that cannot, and no other.
type reserveDirective struct {
	instruction      string
	disableToolCalls bool
}

// reserveDirectiveFor turns a budget notice into a directive the model can act
// on. The limiter's message says what is nearly spent; the instruction says what
// to do about it. toolRoundsUsed is how many tool rounds this run has already
// spent inside the reserve, which decides whether the final one is still
// available: the first move must keep its tools, since its whole point is the
// call the task still needs.
func reserveDirectiveFor(msg string, toolRoundsUsed int) reserveDirective {
	d := reserveDirective{instruction: reserveInstruction}
	if toolRoundsUsed >= reserveToolRoundsMax {
		d = reserveDirective{instruction: reserveSpentInstruction, disableToolCalls: true}
	}
	if msg != "" {
		d.instruction = msg + "\n" + d.instruction
	}
	return d
}

// stepTool runs exactly one pending tool call and answers it in the conversation.
//
// It goes through the session's CallTool, which appends the result into the
// trailing tool message when there is one. That is what keeps the N results of a
// parallel call turn in ONE turn, which is the only shape a provider accepts for
// them — a result per turn is rejected outright ("the number of function response
// parts is equal to the number of function call parts of the function call turn"),
// and once the conversation holds the split every later call in the run is
// rejected too.
//
// The arguments pass through toolargs.Coerce first, which is the only point
// before gollem's own validation where a single value sent for an array-typed
// argument can still be read as the batch of one the model meant.
//
// A tool that fails is not a transition failure: the failure is recorded as this
// call's response, so the call is still answered and the model gets to react to
// it on its next turn — which is how the previous runtime behaved too. The one
// error that must not be swallowed is a refusal from the budget: continuing past
// it would spend beyond the ceiling.
//
// That refusal check is DEFENSIVE rather than a path budget.Config can take.
// agentkit evaluates Limit against "committed metrics plus whatever this
// transition has already spent", and this transition spends nothing before the
// call — so a metrics-based limiter that let the transition begin also lets the
// call through, and the run is stopped at the next transition boundary instead.
// The check is kept because Limit is an arbitrary function: a stateful or
// time-based one can refuse here, and swallowing that would spend past a ceiling
// its owner had already declared closed. Do not remove it on the grounds that no
// test drives it; no test can, with the limiter this application ships.
func (s *strategy) stepTool(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output], error) {
	if len(st.Pending) == 0 {
		st.Phase = phaseGenerate
		return st, agentkit.Continue[Output](), nil
	}

	call := st.Pending[0]
	st.Pending = st.Pending[1:]
	if call == nil {
		return st, agentkit.Continue[Output](), nil
	}

	if _, err := sys.Session().CallTool(ctx, toolargs.Coerce(sys.Tools(), *call)); err != nil {
		if errors.Is(err, agentkit.ErrLimitExceeded) {
			return st, agentkit.Decision[Output]{}, goerr.Wrap(err, "react: tool call refused by the budget",
				goerr.V("tool", call.Name))
		}
		// Reported as well as fed back. The model needs the failure to react to;
		// an operator needs it to tell a broken tool from a model that chose not
		// to use its result, and the run timeline that also records it is only
		// written for a run that keeps a run record.
		//
		// Every failure is handed to errutil.Handle, including the ones that
		// never page: an error tagged errutil.TagBenign — a tool refusing what
		// the model sent, which its own next call can repair — is demoted to an
		// INFO log inside Handle. Keeping that decision there rather than here
		// is deliberate; a per-tool skip would have to be written at each call
		// site and would be forgotten at the next one.
		errutil.Handle(ctx, goerr.Wrap(err, "react: tool call",
			goerr.V("tool", call.Name), goerr.V("call_id", call.ID)), "react: tool call")
	}
	// Answered either way: a failed call is recorded in the conversation as an error
	// result, which is what the model reacts to on its next turn.
	st.ToolsAnswered = true

	if len(st.Pending) == 0 {
		st.Phase = phaseGenerate
	}
	return st, agentkit.Continue[Output](), nil
}

func (s *strategy) EncodeState(st state) ([]byte, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, goerr.Wrap(err, "react: encode state")
	}
	return raw, nil
}

func (s *strategy) DecodeState(_ int, raw []byte) (state, error) {
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return state{}, goerr.Wrap(err, "react: decode state")
	}
	return st, nil
}

func (s *strategy) EncodeOutput(out Output) ([]byte, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, goerr.Wrap(err, "react: encode output")
	}
	return raw, nil
}

// DecodeOutput reads back the bytes a finished Process stored. agentkit hands a
// parent only its child's raw output, so a caller collecting sub-agent results
// needs this; the kernel itself never calls it.
func DecodeOutput(raw []byte) (Output, error) {
	var out Output
	if err := json.Unmarshal(raw, &out); err != nil {
		return Output{}, goerr.Wrap(err, "react: decode output")
	}
	return out, nil
}
