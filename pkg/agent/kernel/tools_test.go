package kernel_test

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casemulti"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casewriter"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/core"
	memotool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/memo"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slackpost"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	slackservice "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent"
)

func testRegistry(entries ...*model.WorkspaceEntry) *model.WorkspaceRegistry {
	r := model.NewWorkspaceRegistry()
	for _, e := range entries {
		r.Register(e)
	}
	return r
}

func channelWorkspace() *model.WorkspaceEntry {
	return &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-1", Name: "Workspace One"}}
}

// threadWorkspace is the same workspace configured thread-per-case, which is what
// withholds the Action toolset.
func threadWorkspace() *model.WorkspaceEntry {
	return &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-1", Name: "Workspace One"},
		CaseMode:  model.CaseModeThread,
	}
}

func toolNames(tools []gollem.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Spec().Name)
	}
	sort.Strings(names)
	return names
}

func hasPrefixIn(names []string, prefix string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// stubKnowledgeAccessor / stubKnowledgeMutator are no-op knowledge backends;
// these tests assert which tools get built, never invoke them.
type stubKnowledgeAccessor struct{}

func (stubKnowledgeAccessor) SearchKnowledge(context.Context, string, string, []model.TagID, int) ([]*model.Knowledge, error) {
	return nil, nil
}

func (stubKnowledgeAccessor) GetKnowledge(context.Context, string, model.KnowledgeID) (*model.Knowledge, error) {
	return &model.Knowledge{}, nil
}

func (stubKnowledgeAccessor) ListTags(context.Context, string) ([]*model.Tag, error) {
	return nil, nil
}

type stubKnowledgeMutator struct{}

func (stubKnowledgeMutator) CreateTag(context.Context, string, string) (*model.Tag, error) {
	return &model.Tag{}, nil
}

func (stubKnowledgeMutator) UpdateTag(context.Context, string, model.TagID, string) (*model.Tag, error) {
	return &model.Tag{}, nil
}

func (stubKnowledgeMutator) DeleteTag(context.Context, string, model.TagID) error { return nil }

func (stubKnowledgeMutator) CreateKnowledge(context.Context, string, string, string, []model.TagID) (*model.Knowledge, error) {
	return &model.Knowledge{}, nil
}

func (stubKnowledgeMutator) UpdateKnowledge(context.Context, string, model.KnowledgeID, *string, *string, *[]model.TagID) (*model.Knowledge, error) {
	return &model.Knowledge{}, nil
}

// stubSlackBot stands in for the Slack bot client. The embedded interface is
// nil, so any method a test does not expect to be called panics rather than
// silently succeeding — these tests assert which tools get BUILT and never run
// one.
type stubSlackBot struct {
	slackservice.Service
}

// stubSlackPoster, stubActionMutator and stubCaseMutator stand in for the
// writers a tool needs to exist at all. The embedded interfaces are nil, so any
// method a test does not expect panics rather than silently succeeding — these
// tests assert which tools get BUILT and never run one.
type stubSlackPoster struct {
	slackpost.Poster
}

type stubActionMutator struct {
	core.ActionMutator
}

type stubCaseMutator struct {
	casewriter.CaseMutator
}

func newProcess(name agentkit.AgentName, sc kernel.Scope) *agentkit.Process {
	return &agentkit.Process{
		ID:       "proc-1",
		Agent:    name,
		Status:   agentkit.ProcessPending,
		RootID:   "proc-1",
		Metadata: sc.Metadata(),
	}
}

func TestNewToolFactoryValidatesDeps(t *testing.T) {
	testCases := map[string]kernel.ToolDeps{
		"no repository": {Registry: testRegistry()},
		"no registry":   {Repo: memory.New()},
	}
	for name, deps := range testCases {
		t.Run(name, func(t *testing.T) {
			factory, err := kernel.NewToolFactory(deps)
			gt.Value(t, err).NotNil()
			gt.Value(t, factory).Nil()
		})
	}
}

// TestToolFactoryExpandsTheAgentPalette pins that "*" resolves to the palette of
// the agent kind, not to one shared list. A wrong expansion is silent: the agent
// simply has tools it should not, or lacks tools its prompt tells it to use.
func TestToolFactoryExpandsTheAgentPalette(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:     repo,
		Registry: testRegistry(channelWorkspace()),
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{WorkspaceID: "ws-1", ActorUserID: "U1", ToolSets: []string{kernel.ToolSetsAll}}

	t.Run("the channel case agent gets the mutating action tools", func(t *testing.T) {
		tools, err := factory(ctx, newProcess(kernel.AgentCaseChannel, sc))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		gt.Bool(t, slices.Contains(names, "core__create_action")).True()
		gt.Bool(t, slices.Contains(names, "core__list_actions")).True()
	})

	t.Run("the thread case agent's palette carries the action reads for its planner", func(t *testing.T) {
		// On a channel-mode workspace nothing withholds them; the thread-mode
		// gate is pinned by TestToolFactoryWithdrawsActionToolsInAThreadModeWorkspace.
		// The agent is a planner, so it is declared only the read half; the
		// writers reach its tasks (TestToolFactoryGrantsActionToolsToAThreadModeMentionTurn).
		tools, err := factory(ctx, newProcess(kernel.AgentCaseThread, sc))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		gt.Bool(t, slices.Contains(names, "core__list_actions")).True()
		gt.Bool(t, slices.Contains(names, "core__create_action")).False()
	})

	t.Run("the create agent gets neither action nor case-write tools", func(t *testing.T) {
		tools, err := factory(ctx, newProcess(kernel.AgentCaseThreadCreate, sc))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		gt.Bool(t, hasPrefixIn(names, "core__")).False()
		gt.Bool(t, hasPrefixIn(names, "case__")).False()
	})
}

// TestToolFactoryBuildsTheAssistPalette pins the assist agent's palette against
// what the pre-agentkit assist flow assembled: the mutating action tools plus
// Slack read AND post. The posting tool is the point of the whole command — an
// assist run that cannot write back into the case channel does nothing visible —
// and its channel comes from the case, not from the scope.
func TestToolFactoryBuildsTheAssistPalette(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	ctx = seedCase(t, ctx, repo, &model.Case{
		Title:          "assist target",
		SlackChannelID: "C-ASSIST",
	})

	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:     repo,
		Registry: testRegistry(channelWorkspace()),
		SlackBot: stubSlackBot{},
	})
	gt.NoError(t, err).Required()

	// No actor: assist is unattended, and RequiresActor excludes it precisely so
	// this resolves to a full palette rather than to nothing.
	sc := kernel.Scope{WorkspaceID: "ws-1", CaseID: 1, ToolSets: []string{kernel.ToolSetsAll}}
	tools, err := factory(ctx, newProcess(kernel.AgentAssist, sc))
	gt.NoError(t, err).Required()

	names := toolNames(tools)
	gt.Bool(t, slices.Contains(names, "core__create_action")).True()
	gt.Bool(t, slices.Contains(names, "core__list_actions")).True()
	gt.Bool(t, slices.Contains(names, "slack__get_messages")).True()
	gt.Bool(t, slices.Contains(names, "slack__post_message")).True()
	// Assist has never had the case writer, memo or knowledge tools.
	gt.Bool(t, hasPrefixIn(names, "case__")).False()
	gt.Bool(t, hasPrefixIn(names, "memo__")).False()
	gt.Bool(t, hasPrefixIn(names, "knowledge__")).False()
}

// TestToolFactoryWithholdsTheAssistPostToolWithoutACaseChannel pins that the
// posting tool is not built for a case with no Slack channel: it would post
// nowhere, and a tool that can only fail is worse than an absent one.
func TestToolFactoryWithholdsTheAssistPostToolWithoutACaseChannel(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	ctx = seedCase(t, ctx, repo, &model.Case{Title: "no channel"})

	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:     repo,
		Registry: testRegistry(channelWorkspace()),
		SlackBot: stubSlackBot{},
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{WorkspaceID: "ws-1", CaseID: 1, ToolSets: []string{kernel.ToolSetsAll}}
	tools, err := factory(ctx, newProcess(kernel.AgentAssist, sc))
	gt.NoError(t, err).Required()

	names := toolNames(tools)
	gt.Bool(t, slices.Contains(names, "slack__post_message")).False()
	// The read tool needs only the bot client, so it survives.
	gt.Bool(t, slices.Contains(names, "slack__get_messages")).True()
}

// TestToolFactoryHonoursAnExplicitSubset pins that a sub-agent gets exactly the
// toolsets its task was planned with, not the whole palette.
func TestToolFactoryHonoursAnExplicitSubset(t *testing.T) {
	ctx := context.Background()
	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:     memory.New(),
		Registry: testRegistry(channelWorkspace()),
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{WorkspaceID: "ws-1", ToolSets: []string{agent.ToolSetCoreRO}}
	tools, err := factory(ctx, newProcess(kernel.AgentTask, sc))
	gt.NoError(t, err).Required()

	gt.Array(t, toolNames(tools)).Equal([]string{"core__get_action", "core__list_actions"})
}

// TestToolFactoryWithdrawsActionToolsInAThreadModeWorkspace pins the rule that a
// thread-mode workspace withholds the Action tools from every run whose scope
// does not grant them (Scope.ThreadModeActions): only the mention turn's prompt
// describes Actions, so any other agent would be handed tools it was never told
// how to use.
//
// The WORKSPACE decides it, not the case's SlackThreadTS. Keying on the case
// would give action tools to a thread-mode case whose thread is not set yet —
// which the pre-agentkit hosts never did, because they read the workspace mode.
func TestToolFactoryWithdrawsActionToolsInAThreadModeWorkspace(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	// Deliberately WITHOUT SlackThreadTS: the workspace's mode is what must
	// withhold the tools.
	ctx = seedCase(t, ctx, repo, &model.Case{Title: "thread mode, no thread yet"})

	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:     repo,
		Registry: testRegistry(threadWorkspace()),
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{
		WorkspaceID: "ws-1", CaseID: 1, ActorUserID: "U1",
		ToolSets: []string{agent.ToolSetCore, agent.ToolSetCoreRO},
	}
	tools, err := factory(ctx, newProcess(kernel.AgentCaseChannel, sc))
	gt.NoError(t, err).Required()

	gt.Bool(t, hasPrefixIn(toolNames(tools), "core__")).False()
}

// TestToolFactoryGrantsActionToolsToAThreadModeMentionTurn pins the other half:
// a scope carrying ThreadModeActions — the thread-mode mention turn, and the
// sub-agents that inherit its metadata — gets the full action set on a
// thread-mode workspace.
func TestToolFactoryGrantsActionToolsToAThreadModeMentionTurn(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	ctx = seedCase(t, ctx, repo, &model.Case{
		Title:          "thread case",
		SlackChannelID: "C-THREAD",
		SlackThreadTS:  "1700000000.000100",
	})

	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:     repo,
		Registry: testRegistry(threadWorkspace()),
		ActionUC: stubActionMutator{},
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{
		WorkspaceID: "ws-1", CaseID: 1, ActorUserID: "U1",
		ToolSets:          []string{agent.ToolSetCore},
		ThreadModeActions: true,
	}

	t.Run("a sub-agent given the core toolset gets the action writers", func(t *testing.T) {
		tools, err := factory(ctx, newProcess(kernel.AgentTask, sc))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		for _, want := range []string{"core__list_actions", "core__create_action", "core__update_action_status", "core__add_action_step"} {
			gt.Bool(t, slices.Contains(names, want)).True()
		}
	})

	t.Run("the mention planner is declared only the action reads", func(t *testing.T) {
		all := sc
		all.ToolSets = []string{kernel.ToolSetsAll}
		tools, err := factory(ctx, newProcess(kernel.AgentCaseThread, all))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		gt.Bool(t, slices.Contains(names, "core__list_actions")).True()
		gt.Bool(t, slices.Contains(names, "core__create_action")).False()
	})

	t.Run("the same scope without the grant gets none", func(t *testing.T) {
		denied := sc
		denied.ThreadModeActions = false
		tools, err := factory(ctx, newProcess(kernel.AgentTask, denied))
		gt.NoError(t, err).Required()
		gt.Bool(t, hasPrefixIn(toolNames(tools), "core__")).False()
	})
}

// TestToolFactoryBuildsTheJobPalette pins the unattended-run palette against
// what the pre-agentkit buildJobTools assembled. The three withheld action tools
// are the point: a Job acts on its own judgement with nobody reviewing it, and a
// wrong archive is work the team can no longer see.
//
// It is checked on the simple-strategy Job, whose root Process does the work
// itself. A plan-execute Job's root is a planner and is declared only the read
// half (TestToolFactoryDeclaresAPlannerOnlyItsReadTools).
func TestToolFactoryBuildsTheJobPalette(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	ctx = seedCase(t, ctx, repo, &model.Case{
		Title:          "job target",
		SlackChannelID: "C-JOB",
	})

	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:        repo,
		Registry:    testRegistry(channelWorkspace()),
		SlackBot:    stubSlackBot{},
		SlackPoster: stubSlackPoster{},
		// The writers have to be wired for the write tools to exist at all.
		ActionUC: stubActionMutator{},
		CaseUC:   stubCaseMutator{},
	})
	gt.NoError(t, err).Required()

	// No actor: a Job is unattended, which RequiresActor excludes precisely so
	// this resolves to a palette rather than to nothing.
	sc := kernel.Scope{WorkspaceID: "ws-1", CaseID: 1, ToolSets: []string{kernel.ToolSetsAll}}
	tools, err := factory(ctx, newProcess(kernel.AgentJobSimple, sc))
	gt.NoError(t, err).Required()
	names := toolNames(tools)

	// Reads and the non-destructive writes.
	gt.Bool(t, slices.Contains(names, "core__list_actions")).True()
	gt.Bool(t, slices.Contains(names, "core__create_action")).True()
	gt.Bool(t, slices.Contains(names, "core__update_action_status")).True()
	// The three an unattended run must not have.
	gt.Bool(t, slices.Contains(names, "core__archive_action")).False()
	gt.Bool(t, slices.Contains(names, "core__unarchive_action")).False()
	gt.Bool(t, slices.Contains(names, "core__delete_action_step")).False()
	// Case edits, the channel-pinned poster, and Slack reads.
	gt.Bool(t, slices.Contains(names, "case__update_case")).True()
	gt.Bool(t, slices.Contains(names, "slack__post_to_case_channel")).True()
	gt.Bool(t, slices.Contains(names, "slack__get_messages")).True()
	// A Job has never had GitHub.
	gt.Bool(t, hasPrefixIn(names, "github__")).False()
}

type stubMemoMutator struct {
	memotool.MemoMutator
}

type stubCaseMultiUC struct {
	casemulti.CaseUsecase
}

type stubCaseMultiActionUC struct {
	casemulti.ActionUsecase
}

// writeTools is every tool that changes something outside the run, across the
// palettes a planner's agent kind may hold.
var writeTools = []string{
	"core__create_action",
	"core__update_action",
	"core__update_action_status",
	"core__archive_action",
	"case__update_case",
	"case__assign",
	"case__close_case",
	"case__create_case",
	"case__create_action",
	"slack__post_to_case_channel",
	"slack__post_message",
	"memo__apply_memo_changes",
	"knowledge__create_knowledge",
	"knowledge__update_knowledge",
}

func noWriteTool(t *testing.T, names []string) {
	t.Helper()
	for _, name := range writeTools {
		gt.Bool(t, slices.Contains(names, name)).False()
	}
}

// TestToolFactoryDeclaresAPlannerOnlyItsReadTools pins that a plan-execute
// agent's root Process is declared the read half of its palette. A write the
// planner made itself bypassed the task it should have been planned as: a
// planner called a Slack post with a placeholder text and the text was posted.
// The writes stay with the sub-agents its tasks spawn.
func TestToolFactoryDeclaresAPlannerOnlyItsReadTools(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	ctx = seedCase(t, ctx, repo, &model.Case{Title: "planner target", SlackChannelID: "C-PLAN"})

	ws := channelWorkspace()
	ws.MemoConfig = &config.MemoConfig{FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "note", Type: types.FieldTypeText},
	}}}

	deps := kernel.ToolDeps{
		Repo:              repo,
		Registry:          testRegistry(ws),
		SlackBot:          stubSlackBot{},
		SlackPoster:       stubSlackPoster{},
		ActionUC:          stubActionMutator{},
		CaseUC:            stubCaseMutator{},
		MemoUC:            stubMemoMutator{},
		CaseMultiUC:       stubCaseMultiUC{},
		CaseMultiActionUC: stubCaseMultiActionUC{},
		KnowledgeAccessor: stubKnowledgeAccessor{},
		KnowledgeMutator:  stubKnowledgeMutator{},
	}
	factory, err := kernel.NewToolFactory(deps)
	gt.NoError(t, err).Required()

	sc := kernel.Scope{WorkspaceID: "ws-1", CaseID: 1, ActorUserID: "U1", ToolSets: []string{kernel.ToolSetsAll}}

	t.Run("a plan-execute Job reads but does not write", func(t *testing.T) {
		tools, err := factory(ctx, newProcess(kernel.AgentJob, sc))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		noWriteTool(t, names)
		for _, name := range []string{
			"core__list_actions", "core__get_action",
			"slack__get_messages",
			"memo__list_memos", "memo__get_memo",
			"knowledge__search_knowledge",
		} {
			gt.Bool(t, slices.Contains(names, name)).True()
		}
	})

	t.Run("a thread case planner reads but does not write", func(t *testing.T) {
		tools, err := factory(ctx, newProcess(kernel.AgentCaseThread, sc))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		noWriteTool(t, names)
		gt.Bool(t, slices.Contains(names, "slack__get_messages")).True()
	})

	t.Run("a workspace planner reads cases but does not write them", func(t *testing.T) {
		tools, err := factory(ctx, newProcess(kernel.AgentWorkspace, sc))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		noWriteTool(t, names)
		for _, name := range []string{"case__list_cases", "case__get_case", "case__list_actions", "case__get_action"} {
			gt.Bool(t, slices.Contains(names, name)).True()
		}
	})

	// The agents that do the work themselves keep their writers.
	t.Run("a simple Job still writes", func(t *testing.T) {
		tools, err := factory(ctx, newProcess(kernel.AgentJobSimple, sc))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		gt.Bool(t, slices.Contains(names, "slack__post_to_case_channel")).True()
		gt.Bool(t, slices.Contains(names, "case__update_case")).True()
		gt.Bool(t, slices.Contains(names, "memo__apply_memo_changes")).True()
	})

	t.Run("the channel case agent still writes", func(t *testing.T) {
		tools, err := factory(ctx, newProcess(kernel.AgentCaseChannel, sc))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		gt.Bool(t, slices.Contains(names, "core__create_action")).True()
		gt.Bool(t, slices.Contains(names, "case__update_case")).True()
		gt.Bool(t, slices.Contains(names, "knowledge__create_knowledge")).True()
	})

	// The narrowing follows the agent kind, not the "*" expansion: a planner whose
	// scope names a writer explicitly still gets only the reads.
	t.Run("a planner naming a writer explicitly gets only the reads", func(t *testing.T) {
		explicit := sc
		explicit.ToolSets = []string{agent.ToolSetSlackPost, agent.ToolSetCoreJob}
		tools, err := factory(ctx, newProcess(kernel.AgentJob, explicit))
		gt.NoError(t, err).Required()
		names := toolNames(tools)
		noWriteTool(t, names)
		gt.Bool(t, slices.Contains(names, "core__list_actions")).True()
	})

	t.Run("a planner naming an unclassified toolset fails the claim", func(t *testing.T) {
		unknown := sc
		unknown.ToolSets = []string{"not_a_toolset"}
		tools, err := factory(ctx, newProcess(kernel.AgentJob, unknown))
		gt.Value(t, err).NotNil()
		gt.Array(t, tools).Length(0)
	})

	// A planner's task carries its toolsets explicitly, so the writer it was
	// granted reaches the sub-agent.
	t.Run("a task granted the poster gets it", func(t *testing.T) {
		task := sc
		task.ToolSets = []string{agent.ToolSetSlackPost}
		tools, err := factory(ctx, newProcess(kernel.AgentTask, task))
		gt.NoError(t, err).Required()
		gt.Bool(t, slices.Contains(toolNames(tools), "slack__post_to_case_channel")).True()
	})
}

// The case-draft agent is the one that has NOT chosen a workspace yet, so it is
// the one that gets the workspace-metadata tools. Every other host already knows
// which workspace it runs in.
func TestToolFactoryBuildsTheProposalPalette(t *testing.T) {
	ctx := context.Background()
	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:     memory.New(),
		Registry: testRegistry(channelWorkspace()),
		SlackBot: stubSlackBot{},
	})
	gt.NoError(t, err).Required()

	// No workspace and no case: choosing one is what the run is for. The actor is
	// required — a draft turn acts on a person's behalf.
	sc := kernel.Scope{ActorUserID: "U-HUMAN", ToolSets: []string{kernel.ToolSetsAll}}
	tools, err := factory(ctx, newProcess(kernel.AgentProposal, sc))
	gt.NoError(t, err).Required()
	names := toolNames(tools)

	gt.Bool(t, slices.Contains(names, "list_workspaces")).True()
	gt.Bool(t, slices.Contains(names, "get_workspace")).True()
	// And the read-only investigation tools it plans tasks with.
	gt.Bool(t, slices.Contains(names, "slack__get_messages")).True()
	// No writer tools: nothing is committed until a human submits the preview.
	gt.Bool(t, slices.Contains(names, "core__create_action")).False()
	gt.Bool(t, slices.Contains(names, "case__update_case")).False()
}

// A sub-agent asking for the workspace-metadata toolset by name gets it, which is
// what lets a planned task read a candidate workspace's schema.
func TestToolFactoryResolvesTheWSMetaToolSet(t *testing.T) {
	ctx := context.Background()
	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:     memory.New(),
		Registry: testRegistry(channelWorkspace()),
	})
	gt.NoError(t, err).Required()

	tools, err := factory(ctx, newProcess(kernel.AgentTask, kernel.Scope{
		ToolSets: []string{agent.ToolSetWSMeta},
	}))
	gt.NoError(t, err).Required()
	names := toolNames(tools)
	gt.Bool(t, slices.Contains(names, "list_workspaces")).True()
	gt.Bool(t, slices.Contains(names, "get_workspace")).True()
}

// TestToolFactoryRefusesAnAgentWithNoPalette pins that an agent kind with no
// declared palette fails loudly rather than falling back to a permissive one. The
// palettes differ in ways that matter — an unattended run is deliberately denied
// archive / unarchive / delete_action_step — so a fallback would hand some agent
// tools its kind was never meant to have, silently.
func TestToolFactoryRefusesAnAgentWithNoPalette(t *testing.T) {
	ctx := context.Background()
	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:     memory.New(),
		Registry: testRegistry(channelWorkspace()),
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{WorkspaceID: "ws-1", ToolSets: []string{kernel.ToolSetsAll}}
	tools, err := factory(ctx, newProcess(agentkit.AgentName("agent-added-without-a-palette"), sc))
	gt.Value(t, err).NotNil()
	gt.Array(t, tools).Length(0)
}

// TestToolFactoryWithholdsKnowledgeWritesForAPrivateCase pins both halves of the
// privacy gate: the flag recorded at spawn, and the case's current state. A
// durable Process can wait for a person for hours, so a case that turned private
// meanwhile must not still be writable through the older snapshot.
func TestToolFactoryWithholdsKnowledgeWritesForAPrivateCase(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	seedCase(t, ctx, repo, &model.Case{Title: "public case"})
	seedCase(t, ctx, repo, &model.Case{Title: "private case", IsPrivate: true})

	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:              repo,
		Registry:          testRegistry(channelWorkspace()),
		KnowledgeAccessor: stubKnowledgeAccessor{},
		KnowledgeMutator:  stubKnowledgeMutator{},
	})
	gt.NoError(t, err).Required()

	knowledgeWrites := func(sc kernel.Scope) bool {
		tools, err := factory(ctx, newProcess(kernel.AgentCaseChannel, sc))
		gt.NoError(t, err).Required()
		return slices.Contains(toolNames(tools), "knowledge__create_knowledge")
	}

	base := kernel.Scope{WorkspaceID: "ws-1", ActorUserID: "U1", ToolSets: []string{agent.ToolSetKnowledge}}

	t.Run("a public case may write", func(t *testing.T) {
		sc := base
		sc.CaseID = 1
		gt.Bool(t, knowledgeWrites(sc)).True()
	})

	t.Run("the spawn-time flag withholds writes", func(t *testing.T) {
		sc := base
		sc.CaseID = 1
		sc.PrivateCase = true
		gt.Bool(t, knowledgeWrites(sc)).False()
	})

	t.Run("a case that is private now withholds writes even without the flag", func(t *testing.T) {
		sc := base
		sc.CaseID = 2
		gt.Bool(t, knowledgeWrites(sc)).False()
	})
}

// TestToolFactoryWithholdsEverythingWithoutAnActor pins the backstop for an
// agent that acts on a person's behalf. It withholds the tools rather than
// failing the claim: a claim that fails is requeued forever without ever
// consuming the retry budget, so the Process would hold its Subject and block
// every later turn on that thread. With no tools the run reaches nothing and
// ends on its own.
func TestToolFactoryWithholdsEverythingWithoutAnActor(t *testing.T) {
	ctx := context.Background()
	// The knowledge read tools are offered to every agent regardless of what it
	// requested, so their absence is what proves the whole set was withheld
	// rather than merely unconfigured.
	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:              memory.New(),
		Registry:          testRegistry(channelWorkspace()),
		KnowledgeAccessor: stubKnowledgeAccessor{},
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{WorkspaceID: "ws-1", ToolSets: []string{kernel.ToolSetsAll}}

	t.Run("the workspace agent gets nothing", func(t *testing.T) {
		tools, err := factory(ctx, newProcess(kernel.AgentWorkspace, sc))
		gt.NoError(t, err).Required()
		gt.Array(t, tools).Length(0)
	})

	t.Run("with an actor it gets its palette back", func(t *testing.T) {
		withActor := sc
		withActor.ActorUserID = "U1"
		tools, err := factory(ctx, newProcess(kernel.AgentWorkspace, withActor))
		gt.NoError(t, err).Required()
		gt.Bool(t, len(tools) > 0).True()
	})

	// A sub-agent inherits its parent's metadata, so it is not required to carry
	// an actor of its own and must not be starved when it does not.
	t.Run("an agent that needs no actor is unaffected", func(t *testing.T) {
		tools, err := factory(ctx, newProcess(kernel.AgentTask, sc))
		gt.NoError(t, err).Required()
		gt.Bool(t, len(tools) > 0).True()
	})
}

// TestToolFactoryFailsOnAnUnknownWorkspace pins that a Process naming a
// workspace this deployment does not configure fails the claim. Handing the
// agent an empty tool set instead would let it run to a confident, tool-less
// conclusion and hide the misconfiguration.
func TestToolFactoryFailsOnAnUnknownWorkspace(t *testing.T) {
	ctx := context.Background()
	factory, err := kernel.NewToolFactory(kernel.ToolDeps{
		Repo:     memory.New(),
		Registry: testRegistry(),
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{WorkspaceID: "missing", ActorUserID: "U1", ToolSets: []string{kernel.ToolSetsAll}}
	tools, err := factory(ctx, newProcess(kernel.AgentCaseChannel, sc))
	gt.Value(t, err).NotNil()
	gt.Array(t, tools).Length(0)
}

// seedCase persists one case so the factory can load it, and returns the ctx
// unchanged so the caller reads naturally.
func seedCase(t *testing.T, ctx context.Context, repo interfaces.Repository, c *model.Case) context.Context {
	t.Helper()
	now := time.Now().UTC()
	c.ReporterID = "U1"
	c.CreatedAt = now
	c.UpdatedAt = now
	_, err := repo.Case().Create(ctx, "ws-1", c)
	gt.NoError(t, err).Required()
	return ctx
}

// --- ToolSetProbe ---------------------------------------------------------

// The palette a host advertises to its planner is a fixed list, while the tools
// behind it depend on what the deployment configured. The probe is what closes
// that gap: an id it drops is one whose tools the sub-agent would never receive.
func TestToolSetProbeDropsUnconfiguredToolSets(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	ctx = seedCase(t, ctx, repo, &model.Case{
		Title:          "job target",
		SlackChannelID: "C-JOB",
	})

	// Slack is wired, Notion / Jira / WebFetch are not — the shape of a
	// deployment that configured only Slack.
	probe, err := kernel.NewToolSetProbe(kernel.ToolDeps{
		Repo:        repo,
		Registry:    testRegistry(channelWorkspace()),
		SlackBot:    stubSlackBot{},
		SlackPoster: stubSlackPoster{},
		ActionUC:    stubActionMutator{},
		CaseUC:      stubCaseMutator{},
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{WorkspaceID: "ws-1", CaseID: 1, ToolSets: []string{kernel.ToolSetsAll}}
	got, err := probe.Available(ctx, sc, agent.KnownToolSetIDsJob)
	gt.NoError(t, err).Required()

	gt.Bool(t, slices.Contains(got, agent.ToolSetSlackPost)).True()
	gt.Bool(t, slices.Contains(got, agent.ToolSetSlackRO)).True()
	gt.Bool(t, slices.Contains(got, agent.ToolSetCoreJob)).True()
	gt.Bool(t, slices.Contains(got, agent.ToolSetCaseWrite)).True()
	gt.Bool(t, slices.Contains(got, agent.ToolSetNotion)).False()
	gt.Bool(t, slices.Contains(got, agent.ToolSetJira)).False()
	gt.Bool(t, slices.Contains(got, agent.ToolSetWebFetch)).False()
}

// Without a Slack poster the id behind slack__post_to_case_channel must not be
// offered at all. Advertising it is what let a planner assign a task the tool
// this deployment never built, and the run died on "unknown tool".
func TestToolSetProbeDropsSlackPostWithoutAPoster(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	ctx = seedCase(t, ctx, repo, &model.Case{
		Title:          "job target",
		SlackChannelID: "C-JOB",
	})

	probe, err := kernel.NewToolSetProbe(kernel.ToolDeps{
		Repo:     repo,
		Registry: testRegistry(channelWorkspace()),
		SlackBot: stubSlackBot{},
		ActionUC: stubActionMutator{},
		CaseUC:   stubCaseMutator{},
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{WorkspaceID: "ws-1", CaseID: 1, ToolSets: []string{kernel.ToolSetsAll}}
	got, err := probe.Available(ctx, sc, agent.KnownToolSetIDsJob)
	gt.NoError(t, err).Required()

	gt.Bool(t, slices.Contains(got, agent.ToolSetSlackPost)).False()
	// The reads are unaffected: they need the bot, which is wired.
	gt.Bool(t, slices.Contains(got, agent.ToolSetSlackRO)).True()
}

// A run pinned to no case cannot write to one, so the single-case writer id has
// nothing behind it and must not be offered.
func TestToolSetProbeDropsCaseWriteWithoutACase(t *testing.T) {
	probe, err := kernel.NewToolSetProbe(kernel.ToolDeps{
		Repo:     memory.New(),
		Registry: testRegistry(channelWorkspace()),
		SlackBot: stubSlackBot{},
		CaseUC:   stubCaseMutator{},
	})
	gt.NoError(t, err).Required()

	sc := kernel.Scope{WorkspaceID: "ws-1", ToolSets: []string{kernel.ToolSetsAll}}
	got, err := probe.Available(context.Background(), sc,
		[]string{agent.ToolSetCaseWrite, agent.ToolSetSlackRO})
	gt.NoError(t, err).Required()

	gt.Bool(t, slices.Contains(got, agent.ToolSetCaseWrite)).False()
	gt.Bool(t, slices.Contains(got, agent.ToolSetSlackRO)).True()
}

// A host bound without a probe keeps the palette it had, rather than losing its
// whole vocabulary.
func TestToolSetProbeNilReturnsThePaletteUnchanged(t *testing.T) {
	var probe *kernel.ToolSetProbe
	got, err := probe.Available(context.Background(), kernel.Scope{}, agent.KnownToolSetIDsJob)
	gt.NoError(t, err).Required()
	gt.Value(t, got).Equal(agent.KnownToolSetIDsJob)
}
