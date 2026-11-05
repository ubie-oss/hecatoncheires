package kernel

import (
	"context"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/trace"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/budget"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
)

// Budgets are the ceilings each class of Process runs under. Root and sub-agent
// are separate because a sub-agent is one investigation and a root run is the
// whole turn; giving them one number would either starve the turn or let a
// single task spend it all.
//
// They are also bounded by different quantities: a root run's spend ceiling is
// money, resolved per run from the model it generates through, while a
// sub-agent's is tokens. See the budget package for why.
type Budgets struct {
	// Root applies to every Process an application entry point spawns.
	Root budget.Root
	// Task applies to the sub-agent Processes a plan-execute run spawns.
	Task budget.Config
}

// Validate enforces both ceilings.
func (b Budgets) Validate() error {
	if err := b.Root.Validate(); err != nil {
		return goerr.Wrap(err, "root budget is invalid")
	}
	if err := b.Task.Validate(); err != nil {
		return goerr.Wrap(err, "task budget is invalid")
	}
	return nil
}

// Deps is everything the Kernel is built from. Every field is required.
type Deps struct {
	// Repo is the durable Process store.
	Repo agentkit.Repository
	// History persists each Process's conversation as immutable versions.
	History agentkit.HistoryStore
	// LLM is the default model. A strategy that binds a model role to something
	// else does so through WithModelRole; an unbound role falls back here.
	LLM gollem.LLMClient
	// Trace is where each claim's archive is written.
	Trace trace.Repository
	// Tools carries the clients and usecases the tool factory builds from.
	Tools ToolDeps
	// Budgets are the ceilings Strategy.Limit answers with.
	Budgets Budgets
	// Models is which model each run generates through and what it may spend.
	// Required: without it a run has neither a priced ceiling nor a way to reach
	// a model other than the default one.
	Models ModelPolicy
	// Agents is the registry the application filled with its strategies before
	// building the Kernel. agentkit requires every Register to complete before
	// the first Spawn or Serve, so registration is the caller's job and this is
	// the finished result.
	Agents *agentkit.Registry
	// Slots is the deployment-wide concurrency gate. Optional: nil leaves every
	// run ungated, which is what a deployment that configured no limit wants.
	// Only runs whose Scope sets SlotGated are subject to it.
	Slots SlotGate
}

// Validate enforces the required-field contract so a wiring mistake fails at
// startup rather than at the first mention.
func (d *Deps) Validate() error {
	if d == nil {
		return goerr.New("kernel deps is nil")
	}
	if d.Repo == nil {
		return goerr.New("agent process repository is required")
	}
	if d.History == nil {
		return goerr.New("history store is required")
	}
	if d.LLM == nil {
		return goerr.New("llm client is required")
	}
	if d.Trace == nil {
		return goerr.New("trace repository is required")
	}
	if d.Agents == nil {
		return goerr.New("agent registry is required")
	}
	if d.Models.IsZero() {
		return goerr.New("model policy is required")
	}
	if err := d.Tools.Validate(); err != nil {
		return goerr.Wrap(err, "tool deps are invalid")
	}
	if err := d.Budgets.Validate(); err != nil {
		return goerr.Wrap(err, "budgets are invalid")
	}
	return nil
}

// Serve runs the agent worker. Every worker in this application starts here
// rather than calling Kernel.Serve directly, so the guard below cannot be
// forgotten at a call site: it is prepended to whatever the caller passes.
//
// Caller options come after it and can therefore still override it, which is
// what a test needs when it is measuring the guard itself. Production code has
// no reason to.
func Serve(ctx context.Context, k *agentkit.Kernel, opts ...agentkit.ServeOption) error {
	if k == nil {
		return goerr.New("agent kernel is required")
	}
	all := append([]agentkit.ServeOption{NoDuplicateSideEffects()}, opts...)
	if err := k.Serve(ctx, all...); err != nil {
		return goerr.Wrap(err, "run the agent worker")
	}
	return nil
}

// NoDuplicateSideEffects is the Serve option every worker in this application
// MUST pass; Serve above applies it for you. It is not tuning — it is a
// correctness requirement of the tools these agents carry.
//
// A transition runs its effect and is checkpointed afterwards, so a claim that
// dies in between leaves a Process whose last checkpoint still asks for the call
// that already happened. agentkit's default lets three such takeovers re-run it;
// its own documentation says callers that cannot tolerate duplicated side effects
// set the bound to 0. This application cannot: core__create_action,
// case__update_case, memo / knowledge creation and slack__post_message all take
// effect on the first call and carry no idempotency key, so a re-run means a
// second Action, a second post, a second record.
//
// The cost is that a run whose instance dies mid-transition fails instead of
// resuming — which is exactly what the previous runtime did with a crashed turn,
// so nothing regresses. Remove this only once every side-effecting tool is
// idempotent under a replayed (process, call) pair.
func NoDuplicateSideEffects() agentkit.ServeOption {
	return agentkit.WithMaxUncleanReclaims(0)
}

// Build assembles the Kernel: the tool factory, the claim bracket that carries
// the request-scoped context and the trace sinks, the prompt-cache setting every
// LLM call runs under, and the effect middlewares that record LLM and tool calls.
func Build(d Deps) (*agentkit.Kernel, error) {
	if err := d.Validate(); err != nil {
		return nil, goerr.Wrap(err, "validate kernel deps")
	}
	d.Models.durableUsage.bind(d.Repo)

	factory, err := NewToolFactory(d.Tools)
	if err != nil {
		return nil, goerr.Wrap(err, "build agent tool factory")
	}

	probe, err := NewToolSetProbe(d.Tools)
	if err != nil {
		return nil, goerr.Wrap(err, "build cascade tool probe")
	}
	opts := []agentkit.KernelOption{
		agentkit.WithToolFactory(factory),
		// The role rewrite goes FIRST, so every middleware after it — and the
		// resolution agentkit itself performs — sees the role the run's model
		// selects rather than the one the strategy asked for.
		agentkit.WithGenerateMiddleware(modelRoleMiddleware(d.Models)),
		agentkit.WithClaimMiddleware(durableUsageClaimMiddleware(d.Models)),
		agentkit.WithStepMiddleware(durableUsageStepMiddleware(d.Models)),
		agentkit.WithGenerateMiddleware(durableUsageGenerateMiddleware(d.Models)),
		agentkit.WithToolCallMiddleware(durableUsageToolMiddleware(d.Models)),
		agentkit.WithToolCallMiddleware(cascadeWriteMiddleware(d.Models, probe)),
		// The gate goes OUTSIDE the observability bracket: a claim refused for
		// want of capacity did nothing, so opening a trace and a run-scoped
		// logger for it would file an empty archive on every backoff.
		agentkit.WithClaimMiddleware(slotGuard(d.Slots)),
		agentkit.WithClaimMiddleware(claimMiddleware(d)),
		agentkit.WithGenerateMiddleware(promptCacheMiddleware()),
		agentkit.WithGenerateMiddleware(generateMiddleware()),
		agentkit.WithToolCallMiddleware(toolCallMiddleware()),
		// Registered second, so it runs INSIDE the trace bracket: the timeline and
		// the archive then record the same enriched message the model is given.
		// Reversing these two is caught by
		// TestARejectedToolCallTellsTheModelWhatItSent.
		agentkit.WithToolCallMiddleware(toolArgsFeedbackMiddleware()),
		// Registered inside both of the above for the same reason: the timeline and
		// the archive must record the enriched message the model was given. The two
		// feedback middlewares cover disjoint error classes — argument rejections
		// against everything else — so neither renders into the other's output.
		agentkit.WithToolCallMiddleware(toolErrorValuesMiddleware()),
		agentkit.WithLogger(logging.Default()),
	}
	// One role binding per model a Job may name. They can only be given here:
	// agentkit resolves a role by pointer identity against the map the Kernel was
	// built with, so a binding added later would never be found.
	opts = append(opts, d.Models.kernelOptions()...)

	k, err := agentkit.New(d.Repo, d.Models.WrapClient(d.Models.defaultRef, d.LLM), d.Agents, opts...)
	if err != nil {
		return nil, goerr.Wrap(err, "build agent kernel")
	}
	return k, nil
}
