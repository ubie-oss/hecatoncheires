package kernel

import (
	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
)

// Agent names. These values are persisted on every Process row, so a running
// deployment always has in-flight Processes referring to them by name. Renaming
// one strands those Processes with ErrUnknownAgent and no way to finish, so the
// names are fixed for good; a change in what an agent DOES is expressed by
// bumping its strategy version, which DecodeState migrates.
const (
	// AgentCaseChannel is the ReAct agent behind a mention in a channel-mode
	// case channel.
	AgentCaseChannel agentkit.AgentName = "case-channel"
	// AgentCaseThread is the plan-execute agent behind a mention in a
	// thread-mode case thread.
	AgentCaseThread agentkit.AgentName = "case-thread"
	// AgentCaseThreadCreate is the plan-execute agent that materialises a new
	// thread-mode case from the conversation that triggered it.
	AgentCaseThreadCreate agentkit.AgentName = "case-thread-create"
	// AgentWorkspace is the cross-case plan-execute agent in a workspace
	// channel.
	AgentWorkspace agentkit.AgentName = "workspace"
	// AgentProposal is the plan-execute agent that drafts a case before one
	// exists.
	AgentProposal agentkit.AgentName = "proposal"
	// AgentJob is the plan-execute agent behind a Job configured with the
	// planexec strategy.
	AgentJob agentkit.AgentName = "job"
	// AgentJobSimple is the ReAct agent behind a Job configured with the simple
	// strategy.
	AgentJobSimple agentkit.AgentName = "job-simple"
	// AgentAssist is the ReAct agent behind the assist batch command.
	AgentAssist agentkit.AgentName = "assist"
	// AgentTask is the ReAct sub-agent a plan-execute agent spawns per planned
	// task.
	AgentTask agentkit.AgentName = "task"
)

// taskAgentVersion is the state version of the shared per-task sub-agent.
const taskAgentVersion = 1

// RegisterTaskAgent registers the ReAct sub-agent every plan-execute agent spawns
// per planned task, and returns the handle they are all built with.
//
// It lives here, and is called exactly once per registry, because agentkit keys a
// Process on the agent NAME: a second registration under AgentTask is an error,
// and giving each host its own name would mean maintaining a separate tool palette
// for each — for sub-agents that do the same thing.
func RegisterTaskAgent(reg *agentkit.Registry, limiter agentkit.Limiter,
	store agentkit.HistoryStore,
) (agentkit.Agent[react.Input], error) {
	handle, err := react.Register(reg, AgentTask, taskAgentVersion, limiter,
		agentkit.WithHistoryStore[react.Output](store))
	if err != nil {
		return agentkit.Agent[react.Input]{}, goerr.Wrap(err, "register the task sub-agent")
	}
	return handle, nil
}

// RequiresActor reports whether an agent may only run with an identified Slack
// user behind it.
//
// This is not a formality. A context with no auth token is read by the usecase
// layer as a system context and BYPASSES private-case access control entirely
// (see tokenActor in pkg/usecase/case_access.go). So for an agent working on a
// person's request, a missing actor is not "reduced access" — it is full
// access, and a private case becomes readable by someone who is not in its
// channel.
//
// Every human-triggered agent is listed. The pre-agentkit hosts injected the
// token only in the workspace agent, so the mention agents did read private
// cases for a non-member; that is the behaviour being corrected, not preserved.
//
// The unattended agents are NOT listed. A Job and the assist batch run on a
// schedule with nobody behind them, so there is no actor to name and their
// system-context access is the intended one. A sub-agent inherits its parent's
// metadata, so it carries whatever actor the parent was given.
// An actor need not be a person. A channel-mode case-channel turn that an app
// started (a Slack workflow's mention names no user) acts as the case channel
// (model.CaseChannelActorID), which reaches that one case and public cases only.
// AgentCaseThreadCreate is the scoped exception. A thread-mode case may be
// raised by an integration bot's intake post that names no human, so demanding an
// actor there would refuse a legitimate creation — the same relaxation
// Case.ValidateNew already makes for the reporter. It is safe because a create
// turn's palette (KnownToolSetIDsNoCore) carries no case-reading tool at all:
// there is no private case for a missing actor to widen access to.
func RequiresActor(name agentkit.AgentName) bool {
	switch name {
	case AgentCaseChannel, AgentCaseThread, AgentWorkspace, AgentProposal:
		return true
	default:
		return false
	}
}

// isPlanner reports whether an agent runs the plan-execute strategy, whose root
// Process is the planner rather than the agent that does the work.
//
// Such a planner is declared only the read tools of its palette (see
// NewToolFactory). A write it made itself would bypass the task it should have
// been planned as and the toolsets that task was granted; a planner that called
// a Slack post with a placeholder text in place of a decision had that text
// posted.
func isPlanner(name agentkit.AgentName) bool {
	switch name {
	case AgentJob, AgentCaseThread, AgentCaseThreadCreate, AgentWorkspace, AgentProposal:
		return true
	default:
		return false
	}
}

// ValidateSpawn checks a scope against the agent it is about to launch. A host
// calls it before Spawn.
//
// This is the enforcing check, not the tool factory's. Spawn is the last point
// where a bad scope can be reported to someone who can act on it: once the
// Process exists, a claim that refuses to run it is put back as pending with a
// backoff and never consumes the retry budget, so the row would requeue forever
// and hold its Subject with it — no later turn on that thread could start.
func ValidateSpawn(name agentkit.AgentName, sc Scope) error {
	if err := sc.Validate(); err != nil {
		return goerr.Wrap(err, "invalid agent scope", goerr.V("agent", name))
	}
	if RequiresActor(name) && sc.ActorUserID == "" {
		return goerr.New("this agent may only run with an identified actor",
			goerr.V("agent", name))
	}
	return nil
}
