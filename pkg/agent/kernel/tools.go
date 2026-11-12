package kernel

import (
	"context"
	"slices"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casemulti"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casewriter"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/core"
	githubtool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/github"
	knowledgetool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/knowledge"
	memotool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/memo"
	notiontool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/notion"
	slacktool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slackpost"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/webfetch"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/wsmeta"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

// ToolDeps carries the clients and usecases every toolset is built from. It is
// the kernel-side counterpart of the per-host dependency bundles the old
// runtime assembled per turn: the kernel is built once at startup, and the
// factory below narrows these down to one Process's scope on every claim.
//
// Optional fields may be nil; the corresponding toolset then resolves to
// nothing, which is what lets one wiring serve deployments that configure
// different integrations.
type ToolDeps struct {
	Repo     interfaces.Repository    // required
	Registry *model.WorkspaceRegistry // required

	SlackBot       slacktool.BotService
	SlackSearch    slacktool.SearchService
	SlackRetriever slacktool.MessageRetriever
	// SlackPoster backs the channel-pinned poster an unattended run reports
	// through. It is a narrower interface than SlackBot on purpose: an LLM holding
	// the post tool must not reach the wider Slack surface.
	SlackPoster slackpost.Poster
	// SlackLimits bounds how much of a Slack read tool's result reaches the
	// model context. The zero value leaves both bounds disabled.
	SlackLimits    slacktool.Limits
	NotionClient   notiontool.Client
	GitHubClient   *githubtool.Client
	WebFetchClient *webfetch.Client

	// JiraTools carries the already-expanded Jira read tools. gollem exposes no
	// helper to turn a ToolSet into []Tool, so the CLI expands it once at
	// startup and hands the result through as a plain slice.
	JiraTools []gollem.Tool

	ActionUC     core.ActionMutator
	ActionStepUC core.ActionStepMutator
	CaseUC       casewriter.CaseMutator
	CaseRefUC    core.CaseRefReader

	CaseMultiUC       casemulti.CaseUsecase
	CaseMultiActionUC casemulti.ActionUsecase

	MemoUC memotool.MemoMutator

	KnowledgeAccessor knowledgetool.KnowledgeAccessor
	KnowledgeMutator  knowledgetool.KnowledgeMutator

	// WorkspaceAccess limits the workspace metadata tools to what the run's
	// actor may access. Optional: nil filters nothing.
	WorkspaceAccess interfaces.WorkspaceAuthorizer
}

// Validate enforces the required-field contract.
func (d *ToolDeps) Validate() error {
	if d == nil {
		return goerr.New("tool deps is nil")
	}
	if d.Repo == nil {
		return goerr.New("repository is required")
	}
	if d.Registry == nil {
		return goerr.New("workspace registry is required")
	}
	return nil
}

// defaultToolSets is the full palette each agent kind is entitled to. It is
// what Scope.ToolSets == ["*"] expands to.
//
// A sub-agent carries an explicit subset instead, chosen by the planner from
// the same vocabulary, so this table is also the ceiling on what any task in a
// run can ask for.
//
// A plan-execute agent's root Process — the planner — is declared only the
// read half of its entry (agent.ReadOnlyToolSetIDs): its writes go through the
// tasks it plans.
//
// An agent kind whose palette has not been established yet is an error rather
// than a fallback. The Job and assist tool sets are deliberately NARROWER than
// the case-channel one — a Job gets core.NewWriterForJob, which withholds
// archive / unarchive / delete_action_step precisely because an unattended run
// must not act on a misjudgement — so defaulting them to a wider palette would
// silently hand an unattended agent destructive tools. Those palettes are added
// with the hosts that use them.
func defaultToolSets(name agentkit.AgentName) ([]string, error) {
	switch name {
	case AgentCaseChannel:
		return agent.KnownToolSetIDsCaseChannel, nil
	case AgentCaseThread:
		return agent.KnownToolSetIDsThreadWrite, nil
	case AgentCaseThreadCreate:
		return agent.KnownToolSetIDsNoCore, nil
	case AgentWorkspace:
		return agent.KnownToolSetIDsWorkspaceChannel, nil
	case AgentAssist:
		return agent.KnownToolSetIDsAssist, nil
	case AgentJob, AgentJobSimple:
		return agent.KnownToolSetIDsJob, nil
	case AgentProposal:
		return agent.KnownToolSetIDsProposal, nil
	case AgentTask:
		return agent.KnownToolSetIDs, nil
	default:
		return nil, goerr.New("no default tool palette for this agent", goerr.V("agent", name))
	}
}

// NewToolFactory returns the agentkit.ToolFactory the Kernel is built with. It
// runs once per claim and narrows ToolDeps down to the Process's own scope.
func NewToolFactory(d ToolDeps) (agentkit.ToolFactory, error) {
	if err := d.Validate(); err != nil {
		return nil, goerr.Wrap(err, "validate tool deps")
	}
	return func(ctx context.Context, proc *agentkit.Process) ([]gollem.Tool, error) {
		sc := ScopeFrom(proc.Metadata)

		// An agent that acts on a person's behalf gets nothing without one. A
		// context with no auth token is read by the usecase layer as a system
		// context and BYPASSES private-case access control, so running such an
		// agent with its normal tools would widen access rather than narrow it.
		//
		// Withdrawing the tools rather than failing is deliberate: a claim that
		// fails is requeued forever without ever consuming the retry budget, so
		// the Process would hold its Subject and block every later turn on that
		// thread. With no tools the run reaches nothing and ends on its own.
		// ValidateSpawn is what stops such a Process being created at all.
		if sc.ActorUserID == "" && RequiresActor(proc.Agent) {
			errutil.Handle(ctx, goerr.New("agent run has no actor; withholding every tool",
				goerr.V("process", proc.ID), goerr.V("agent", proc.Agent)),
				"agent run has no actor; withholding every tool")
			return nil, nil
		}

		resolver, err := d.resolverFor(ctx, sc)
		if err != nil {
			return nil, goerr.Wrap(err, "build the process tool resolver",
				goerr.V("process", proc.ID))
		}

		ids := sc.ToolSets
		if slices.Contains(ids, ToolSetsAll) {
			expanded, eErr := defaultToolSets(proc.Agent)
			if eErr != nil {
				return nil, goerr.Wrap(eErr, "expand the agent tool palette",
					goerr.V("process", proc.ID))
			}
			ids = expanded
		}
		// The palette is what the planner hands its tasks; the planner itself
		// gets only the reads, whichever ids its scope names. A sub-agent is a
		// different agent kind and keeps the explicit toolsets its task was given.
		if isPlanner(proc.Agent) {
			readOnly, rErr := agent.ReadOnlyToolSetIDs(ids)
			if rErr != nil {
				return nil, goerr.Wrap(rErr, "narrow the planner palette to its read tools",
					goerr.V("process", proc.ID), goerr.V("agent", proc.Agent))
			}
			ids = readOnly
		}
		return resolver.Resolve(ids), nil
	}, nil
}

// resolverFor narrows ToolDeps down to one Process's scope and returns the
// resolver built from it.
//
// The tool factory and ToolSetProbe MUST both go through this: the probe answers
// "which toolset ids exist for this run" and the factory decides what the run
// actually gets, so two independent constructions of the same thing would drift
// and re-create the advertised-but-absent tool this whole path exists to avoid.
func (d ToolDeps) resolverFor(ctx context.Context, sc Scope) (*agent.ToolSetResolver, error) {
	var entry *model.WorkspaceEntry
	if sc.WorkspaceID != "" {
		found, err := d.Registry.Get(sc.WorkspaceID)
		if err != nil {
			// The scope names a workspace this deployment does not configure.
			// Failing is right: handing the agent an empty tool set would let it
			// run to a confident, tool-less conclusion instead of surfacing the
			// misconfiguration.
			return nil, goerr.Wrap(err, "resolve the workspace",
				goerr.V("workspace_id", sc.WorkspaceID))
		}
		entry = found
	}

	var target *model.Case
	if sc.CaseID != 0 {
		found, err := d.Repo.Case().Get(ctx, sc.WorkspaceID, sc.CaseID)
		if err != nil {
			return nil, goerr.Wrap(err, "load the case",
				goerr.V("workspace_id", sc.WorkspaceID), goerr.V("case_id", sc.CaseID))
		}
		target = found
	}

	return agent.NewToolSetResolver(buildToolSetDeps(d, sc, entry, target)), nil
}

// ToolSetProbe answers which toolset ids actually resolve to a tool for a given
// scope. A plan-execute host asks it before Spawn and advertises only what comes
// back, so its planner is never offered an id that resolves to nothing.
//
// Without it a palette is a fixed list while the tools behind it are conditional
// on what a deployment configured and on the case the run is pinned to. The
// planner then assigns a task a toolset the sub-agent does not get — which is how
// slack__post_to_case_channel came to be requested on a deployment that had built
// no poster, and the run died on "unknown tool" instead of doing its work.
type ToolSetProbe struct {
	deps ToolDeps
}

// NewToolSetProbe builds the probe from the same ToolDeps the Kernel was built
// with. Pass the identical value; see resolverFor for why.
func NewToolSetProbe(d ToolDeps) (*ToolSetProbe, error) {
	if err := d.Validate(); err != nil {
		return nil, goerr.Wrap(err, "validate tool deps")
	}
	return &ToolSetProbe{deps: d}, nil
}

// Available returns palette with every id that resolves to no tool removed,
// preserving the caller's order.
//
// A nil probe returns the palette unchanged: a host wired without one keeps the
// behaviour it had before the probe existed rather than losing its whole
// vocabulary.
func (p *ToolSetProbe) Available(ctx context.Context, sc Scope, palette []string) ([]string, error) {
	if p == nil {
		return palette, nil
	}
	resolver, err := p.deps.resolverFor(ctx, sc)
	if err != nil {
		return nil, goerr.Wrap(err, "build the tool resolver for the scope")
	}
	out := make([]string, 0, len(palette))
	for _, id := range palette {
		if resolver.Has(id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// buildToolSetDeps flavours every toolset for one Process's scope. It is the
// single place the old per-host buildTools / buildToolResolver functions
// collapsed into, so the tools an agent kind gets are decided in one table
// rather than in five.
func buildToolSetDeps(d ToolDeps, sc Scope, entry *model.WorkspaceEntry, target *model.Case) agent.ToolSetDeps {
	deps := agent.ToolSetDeps{
		Core: core.Deps{
			Repo:         d.Repo,
			WorkspaceID:  sc.WorkspaceID,
			CaseID:       sc.CaseID,
			ActionUC:     d.ActionUC,
			ActionStepUC: d.ActionStepUC,
			CaseRefUC:    d.CaseRefUC,
		},
		Slack: slacktool.Deps{
			Bot:       d.SlackBot,
			Search:    d.SlackSearch,
			Retriever: d.SlackRetriever,
			Limits:    d.SlackLimits,
		},
		Notion:   notiontool.Deps{Client: d.NotionClient},
		GitHub:   d.GitHubClient,
		WebFetch: d.WebFetchClient,
		Jira:     d.JiraTools,
		Knowledge: knowledgetool.Deps{
			WorkspaceID: sc.WorkspaceID,
			Accessor:    d.KnowledgeAccessor,
		},
		// The workspace-metadata tools read the registry, not one workspace, which
		// is exactly why the case-draft flow needs them: it has not chosen a
		// workspace yet.
		WSMeta: wsmeta.Deps{Registry: d.Registry, SourceRepo: d.Repo.Source(), Access: d.WorkspaceAccess},
	}

	if entry != nil {
		deps.Core.StatusSet = entry.ActionStatusSet
	}

	// The Slack posting tools are pinned to the channel of the case the run is on.
	// It is taken from the Case rather than from Scope.ChannelID because the
	// scope's channel/thread pair locates the *thread a run reports into*, and an
	// unattended run (a Job, or assist) has no such thread while still having a
	// channel to write to.
	if target != nil {
		deps.Slack.ChannelID = target.SlackChannelID
		deps.SlackPost = slackpost.Deps{
			Poster:    d.SlackPoster,
			ChannelID: target.SlackChannelID,
			// A thread-mode case's output belongs in the case thread, not at the
			// monitored channel's root where it would be lost among other traffic.
			DefaultThreadTS: target.SlackThreadTS,
		}
	}

	// A thread-mode WORKSPACE withholds the whole core (action) toolset unless
	// the run's scope grants it (Scope.ThreadModeActions, set only by the
	// thread-mode mention turn and inherited by its sub-agents). Its other agents
	// — case creation, materialize, Jobs, the workspace agent — track progress
	// through the board status and have no Action guidance in their prompts.
	//
	// The workspace's mode decides it, not the case's SlackThreadTS. The two are
	// not equivalent: a thread-mode workspace can hold a case whose thread is not
	// set yet, and keying on the case would hand that case action tools its
	// workspace does not support. The pre-agentkit hosts keyed on the workspace
	// for the same reason.
	//
	// The one narrower grant is Scope.ThreadModeReads, which every Job on a
	// thread-mode workspace carries: it gets the READ half of core and never a
	// writer. That is the case_ref lookups always — a case_ref field exists in
	// either mode, and a Job that can set one through case__update_case needs
	// them to find the id — plus the action reads when the workspace enables
	// [case] actions, so it can take the case's Actions into account without
	// changing them. [case] actions is re-checked here rather than recorded at
	// spawn, so a run spawned before the option was turned off reads no Actions.
	if entry != nil && entry.IsThreadMode() && !sc.ThreadModeActions {
		switch {
		case !sc.ThreadModeReads:
			deps.OmitCore = true
		case entry.ActionsEnabled():
			deps.CoreAccess = agent.CoreAccessReadOnly
		default:
			deps.CoreAccess = agent.CoreAccessCaseRefOnly
		}
	}

	// The single-case writer tools are pinned to the case this Process runs on;
	// a Process without one (a draft, or a create turn) gets none.
	if target != nil && entry != nil {
		deps.CaseWrite = casewriter.Deps{
			CaseUC:      d.CaseUC,
			WorkspaceID: sc.WorkspaceID,
			CaseID:      sc.CaseID,
			Schema:      entry.FieldSchema,
			StatusSet:   entry.CaseStatusSet,
		}
	}

	// The cross-case tools take a case id at call time, so they need no pinned
	// case — only the actor whose access they must respect.
	if d.CaseMultiUC != nil && entry != nil {
		multi := casemulti.Deps{
			WorkspaceID: sc.WorkspaceID,
			ActorID:     sc.ActorUserID,
			CaseUC:      d.CaseMultiUC,
			Schema:      entry.FieldSchema,
		}
		if entry.IsThreadMode() {
			// A thread-mode workspace manages no Actions. Handing over the board
			// status set both withholds the cross-case action tools and swaps
			// case__close_case (which rejects a thread-bound case) for the
			// status transition.
			multi.StatusSet = entry.CaseStatusSet
		} else {
			multi.ActionUC = d.CaseMultiActionUC
		}
		deps.CaseMulti = multi
	}

	// Memo tools need the workspace's memo schema to coerce field values.
	if entry != nil && entry.MemoConfig.Enabled() {
		deps.Memo = memotool.Deps{
			Repo:        d.Repo,
			WorkspaceID: sc.WorkspaceID,
			CaseID:      sc.CaseID,
			MemoUC:      d.MemoUC,
			Schema:      entry.MemoConfig.FieldSchema,
		}
	}

	// Knowledge is workspace-wide and visible to everyone, so a run processing a
	// private case gets the read tools only: a write would carry that case's
	// contents into shared knowledge.
	//
	// The check ORs the flag recorded at spawn with the case's CURRENT state. A
	// durable Process can sit waiting for a person for hours, and a case that
	// turned private in the meantime must not still be writable through a
	// snapshot taken before the change. Keeping the spawn-time flag as well means
	// a case the factory could not load stays restricted rather than opening up.
	if !sc.PrivateCase && (target == nil || !target.IsPrivate) {
		deps.Knowledge.Mutator = d.KnowledgeMutator
	}

	return deps
}
