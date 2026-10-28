// Package env builds the in-memory environment a scenario runs against: a
// memory repository (seeded with the scenario's prior cases), in-memory history
// and trace repositories, the system-under-test wired via usecase.New, a
// recording fake Slack service, and per-tool clients that are either simulated
// (ToolSimulator) or live (recorded). It exposes the AgentUseCase entrypoints
// the driver drives, plus the fake Slack, the tool-call recorder, and the trace
// repository for diagnostic dumps.
package env

import (
	"context"
	"time"

	"github.com/gollem-dev/agentkit"
	agentprocmemory "github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/budget"
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/actionwriter"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casewriter"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/core"
	githubtool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/github"
	knowledgetool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/knowledge"
	memotool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/memo"
	notiontool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/notion"
	slacktool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/webfetch"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	modelconfig "github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/agentarchive"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/evaltype"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/scenario"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/toolsim"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

// seedReporterID is the synthesized reporter for injected prior cases.
const seedReporterID = "U-EVALSEED"

// Options carries the externally-provided dependencies needed to build an env.
type Options struct {
	// LLM is the system-under-test agent's LLM client (required).
	LLM gollem.LLMClient
	// Models is which model each run generates through and what it may spend
	// (required). A scenario Job naming a model reaches it the same way a
	// deployed one does, so a scenario exercises the real resolution rather than
	// a harness-only default.
	Models agentkernel.ModelPolicy
	// Completer drives the simulated tools (required when any tool is sim).
	Completer evaltype.Completer
	// Live* are real tool clients, used for tools marked live=true. Nil unless
	// the corresponding live tool is requested.
	LiveSlackSearch slacktool.SearchService
	LiveNotion      notiontool.Client
	GitHub          *githubtool.Client
	// WebFetch holds the live-only webfetch HTTP settings; the eval LLM is
	// injected as the screening client when the tool is built.
	WebFetch *webfetch.ClientConfig
	// JiraTools carries the already-expanded Jira read tools (see
	// pkg/agent/tool/jira). Live-only, like GitHub.
	JiraTools []gollem.Tool
	// SlackToolLimits bounds what a Slack read tool's result may inject into
	// the model context, exactly as it does in serve. It applies to the
	// simulated Slack tools too: a scenario whose simulated result would be
	// truncated in production must be truncated here, or the harness measures
	// behaviour production never sees. The zero value disables both bounds.
	SlackToolLimits slacktool.Limits
}

// Env is a prepared single-scenario environment.
type Env struct {
	AgentUC        *usecase.AgentUseCase
	JobRunner      *job.JobRunner
	Repo           interfaces.Repository
	Registry       *model.WorkspaceRegistry
	Entry          *model.WorkspaceEntry
	Slack          *fakeSlack
	Recorder       *toolsim.Recorder
	Trace          *agentarchive.MemoryTraceRepository
	SeededCases    []*model.Case
	MonitorChannel string
	Language       string

	// stopAgents shuts the agent worker down. Every agent is a durable Process
	// driven by that worker, so a scenario that never starts it produces a run
	// that is recorded and then never executed.
	stopAgents func()
}

// Stop shuts the agent worker down. Callers must call it when the scenario ends;
// leaving it running leaks a goroutine per scenario.
//
// It is deliberately not named Close: the project reserves that name for
// io.Closer, which must go through safe.Close, and this ends a worker rather than
// releasing a handle.
func (e *Env) Stop() {
	if e != nil && e.stopAgents != nil {
		e.stopAgents()
		e.stopAgents = nil
	}
}

// awaitTurnTimeout bounds how long a driver waits for one agent turn. A live LLM
// turn with several investigation rounds is slow; a hung one must still end the
// scenario rather than the whole eval run.
const awaitTurnTimeout = 10 * time.Minute

// AwaitTurn blocks until the turn started on this thread has finished — the
// agent either recorded how it ended on the Session, or committed the case.
//
// A driver needs it because a turn is a durable Process: the entry point returns
// once the run is recorded, and everything the driver then inspects (the pending
// question, the created case, the thread replies) is written afterwards by the
// run's completion handler.
func (e *Env) AwaitTurn(ctx context.Context, channelID, threadTS string, before model.SessionEndReason) error {
	deadline := time.Now().Add(awaitTurnTimeout)
	for {
		ssn, err := e.Repo.Session().GetByThread(ctx, channelID, threadTS)
		if err != nil {
			return goerr.Wrap(err, "await turn: load session",
				goerr.V("channel_id", channelID), goerr.V("thread_ts", threadTS))
		}
		if ssn != nil && ssn.LastAction != "" && ssn.LastAction != before {
			return nil
		}
		// A create turn that committed its case is finished even if the outcome
		// stamp lost a race with this read.
		if c, err := e.Repo.Case().GetBySlackThread(ctx, e.Entry.Workspace.ID, channelID, threadTS); err == nil && c != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return goerr.New("await turn: the agent turn did not finish",
				goerr.V("channel_id", channelID), goerr.V("thread_ts", threadTS))
		}
		select {
		case <-ctx.Done():
			return goerr.Wrap(ctx.Err(), "await turn")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// AwaitJobRun blocks until the newest run recorded for this Job has left the
// RUNNING stage, and returns that run's log.
//
// A driver needs it for the same reason AwaitTurn exists: a Job now runs as a
// durable Process, so JobRunner.Run returns once the run is recorded and spawned.
// Everything the driver inspects — the outcome, the per-call timeline, the case
// and actions the run produced — is written afterwards, by the run's completion
// handler.
func (e *Env) AwaitJobRun(ctx context.Context, key model.JobRunKey) (*model.JobRunLog, error) {
	deadline := time.Now().Add(awaitTurnTimeout)
	for {
		logs, err := e.Repo.JobRunLog().List(ctx, key, 0)
		if err != nil {
			return nil, goerr.Wrap(err, "await job run: list run logs",
				goerr.V("job_id", key.JobID), goerr.V("case_id", key.CaseID))
		}
		if len(logs) > 0 && logs[0].Stage != model.JobRunStageRunning {
			return logs[0], nil
		}
		if time.Now().After(deadline) {
			return nil, goerr.New("await job run: the job run did not finish",
				goerr.V("job_id", key.JobID), goerr.V("case_id", key.CaseID))
		}
		select {
		case <-ctx.Done():
			return nil, goerr.Wrap(ctx.Err(), "await job run")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// The scenario budgets are deliberately generous: a scenario is meant to fail on
// the agent's judgement, not on a ceiling the harness imposed.
//
// They are two different ceilings on purpose. A finished sub-agent's whole
// Metrics is folded into its parent (see `.claude/rules/architecture.md` §
// Budget), so the ROOT ceiling has to cover the planner's own transitions PLUS
// every sub-agent a turn spawns — up to 5 per round, over several rounds. Giving
// both tiers the same number is what made live scenarios die of
// "step budget exhausted (122/64)" with the planner barely started: one busy
// sub-agent was enough to spend the whole root allowance.
//
// evalTaskBudget bounds one investigation; evalRootBudget bounds the turn, and is
// sized for several rounds of them.
var evalTaskBudget = budget.Config{
	MaxSteps: 48, MaxInputTokens: 1_000_000, MaxOutputTokens: 1_000_000, NoticeRatio: 0.9,
}

// evalRootBudget carries no spend ceiling of its own: what a root run may spend
// is money, and the figure comes from the ModelPolicy the harness was given (the
// [agent] section, or the harness default).
var evalRootBudget = budget.Root{MaxSteps: 480, NoticeRatio: 0.9}

// agentRuntime is the started runtime plus the handles a scenario needs from it:
// the Job runtime the JobRunner dispatches onto, and the stop function.
type agentRuntime struct {
	durable *job.DurableRuntime
	stop    func()
}

// startAgentRuntime builds the agentkit runtime a scenario's agents run on and
// starts its worker.
//
// It registers the SAME agents production does, the Job agents included, in the
// order agentkit requires (register → build → bind). That is the point of the
// harness: a scenario exercises the checkpointing, the per-Process budget, the
// duplicate-side-effect bound and the claim middleware a deployed run gets, not a
// second implementation of them.
//
// Everything is in-process and per-scenario: the Process store, the history and
// the trace all live and die with the Env, which is what keeps two scenarios from
// seeing each other's runs.
func startAgentRuntime(repo interfaces.Repository, registry *model.WorkspaceRegistry,
	uc *usecase.UseCases, llm gollem.LLMClient, models agentkernel.ModelPolicy,
) (*agentRuntime, error) {
	procRepo := agentprocmemory.New()
	history := agentarchive.NewMemoryHistoryStore()

	locator, err := agentkernel.NewLocator(procRepo)
	if err != nil {
		return nil, goerr.Wrap(err, "env: build the agent process locator")
	}

	reg := agentkit.NewRegistry()
	taskAgent, err := agentkernel.RegisterTaskAgent(reg, evalTaskBudget.Limiter(models.Resolve), history)
	if err != nil {
		return nil, goerr.Wrap(err, "env: register the task sub-agent")
	}
	if err := uc.Agent.RegisterAgents(reg, evalRootBudget.Limiter(models.Resolve), models,
		history, procRepo, taskAgent); err != nil {
		return nil, goerr.Wrap(err, "env: register the agents")
	}
	durable := &job.DurableRuntime{History: history, Locator: locator, Models: models}
	if err := durable.Register(reg, evalRootBudget.Limiter(models.Resolve), taskAgent); err != nil {
		return nil, goerr.Wrap(err, "env: register the job agents")
	}

	// The same assembly serve uses, so a scenario exercises the tool palette a
	// deployment actually gets rather than one this harness composed by hand.
	toolDeps := uc.AgentToolDeps()
	k, err := agentkernel.Build(agentkernel.Deps{
		Repo:    procRepo,
		History: history,
		LLM:     llm,
		Trace:   agentarchive.NewMemoryTraceRepository(),
		Budgets: agentkernel.Budgets{Root: evalRootBudget, Task: evalTaskBudget},
		Models:  models,
		Agents:  reg,
		Tools:   toolDeps,
	})
	if err != nil {
		return nil, goerr.Wrap(err, "env: build the agent runtime")
	}
	probe, err := agentkernel.NewToolSetProbe(toolDeps)
	if err != nil {
		return nil, goerr.Wrap(err, "env: build the agent toolset probe")
	}
	uc.Agent.BindAgentKernel(k, probe)
	durable.Bind(k, probe)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// Deliberately NOT async.Dispatch / DispatchCancelable, which every other
	// background goroutine in this codebase uses. Those register on the package
	// WaitGroup that async.Wait() drains, and this worker runs for the whole life
	// of the Env — so a driver calling async.Wait() to flush a handler's async
	// tail would block on the worker instead and the scenario would never finish.
	// The panic recovery and error reporting those helpers provide is done inline
	// here instead, so the reason for the deviation is the WaitGroup and nothing
	// else.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				errutil.Handle(ctx, goerr.New("panic in the eval agent worker",
					goerr.V("panic", r)), "eval agent worker panicked")
				done <- nil
			}
		}()
		done <- agentkernel.Serve(ctx, k, agentkit.WithPollInterval(5*time.Millisecond))
	}()
	return &agentRuntime{
		durable: durable,
		stop: func() {
			cancel()
			<-done
		},
	}, nil
}

// Build assembles the environment for one scenario.
func Build(ctx context.Context, sc *scenario.Scenario, opts Options) (*Env, error) {
	if opts.LLM == nil {
		return nil, goerr.New("env: LLM client is required")
	}
	if opts.Models.IsZero() {
		return nil, goerr.New("env: model policy is required")
	}
	if sc.Workspace == nil {
		return nil, goerr.New("env: scenario has no workspace config")
	}

	repo := memory.New()
	registry, entry := buildRegistry(sc.Workspace)

	historyRepo := agentarchive.NewMemoryHistoryRepository()
	traceRepo := agentarchive.NewMemoryTraceRepository()
	recorder := toolsim.NewRecorder()

	slackSearch, err := resolveSlackSearch(sc, opts, recorder)
	if err != nil {
		return nil, err
	}
	notionClient, err := resolveNotion(sc, opts, recorder)
	if err != nil {
		return nil, err
	}

	fake := newFakeSlack()
	ucOpts := []usecase.Option{
		usecase.WithLLMClient(opts.LLM),
		usecase.WithSlackService(fake),
		usecase.WithSlackSearchService(slackSearch),
		usecase.WithSlackMessageRetriever(toolsim.SlackRetriever(recorder)),
		usecase.WithSlackToolLimits(opts.SlackToolLimits),
		usecase.WithNotionToolClient(notionClient),
		usecase.WithHistoryRepository(historyRepo),
		usecase.WithTraceRepository(traceRepo),
	}
	if opts.GitHub != nil {
		ucOpts = append(ucOpts, usecase.WithGitHubService(opts.GitHub))
	}
	if opts.WebFetch != nil {
		ucOpts = append(ucOpts, usecase.WithWebFetch(*opts.WebFetch))
	}
	if len(opts.JiraTools) > 0 {
		ucOpts = append(ucOpts, usecase.WithJiraTools(opts.JiraTools))
	}

	uc := usecase.New(repo, registry, ucOpts...)
	if uc.Agent == nil {
		return nil, goerr.New("env: agent use case was not constructed (LLM/history/trace wiring incomplete)")
	}

	seeded, err := seedCases(ctx, repo, entry.Workspace.ID, sc)
	if err != nil {
		return nil, err
	}
	if err := seedSources(ctx, repo, entry.Workspace.ID, sc); err != nil {
		return nil, err
	}

	// The runtime is built first: the Job agents must be registered before the
	// Kernel exists, and the JobRunner needs the runtime to dispatch onto.
	runtime, err := startAgentRuntime(repo, registry, uc, opts.LLM, opts.Models)
	if err != nil {
		return nil, err
	}
	jobRunner := buildJobRunner(repo, registry, uc, opts.LLM, runtime.durable)
	runtime.durable.AttachRunner(jobRunner)

	return &Env{
		stopAgents:     runtime.stop,
		AgentUC:        uc.Agent,
		JobRunner:      jobRunner,
		Repo:           repo,
		Registry:       registry,
		Entry:          entry,
		Slack:          fake,
		Recorder:       recorder,
		Trace:          traceRepo,
		SeededCases:    seeded,
		MonitorChannel: sc.Workspace.SlackMonitorChannel,
		Language:       sc.Meta.Language,
	}, nil
}

func resolveSlackSearch(sc *scenario.Scenario, opts Options, rec *toolsim.Recorder) (slacktool.SearchService, error) {
	t := sc.Tools[toolsim.ToolSlackSearch]
	if t.Live {
		if opts.LiveSlackSearch == nil {
			return nil, goerr.New("env: slack_search marked live but no live client provided")
		}
		return toolsim.RecordingSlackSearch(opts.LiveSlackSearch, rec), nil
	}
	return toolsim.SlackSearch(opts.Completer, t.Background, rec), nil
}

func resolveNotion(sc *scenario.Scenario, opts Options, rec *toolsim.Recorder) (notiontool.Client, error) {
	t := sc.Tools[toolsim.ToolNotionSearch]
	if t.Live {
		if opts.LiveNotion == nil {
			return nil, goerr.New("env: notion_search marked live but no live client provided")
		}
		return toolsim.RecordingNotion(opts.LiveNotion, rec), nil
	}
	return toolsim.NotionSearch(opts.Completer, t.Background, rec), nil
}

// buildRegistry constructs a single-entry registry from the workspace config,
// mirroring config.AppConfig.Configure's mapping.
func buildRegistry(wc *config.WorkspaceConfig) (*model.WorkspaceRegistry, *model.WorkspaceEntry) {
	entry := &model.WorkspaceEntry{
		Workspace: model.Workspace{
			ID:          wc.ID,
			Name:        wc.Name,
			Description: wc.Description,
			Emoji:       wc.Emoji,
			Color:       wc.Color,
		},
		FieldSchema:           wc.FieldSchema,
		ActionStatusSet:       wc.ActionStatusSet,
		SlackChannelPrefix:    wc.SlackChannelPrefix,
		SlackTeamID:           wc.SlackTeamID,
		SlackInviteUsers:      wc.SlackInviteUsers,
		SlackInviteGroups:     wc.SlackInviteGroups,
		SlackWelcomeMessages:  wc.SlackWelcomeMessages,
		CompilePrompt:         wc.CompilePrompt,
		AssistPrompt:          wc.AssistPrompt,
		AssistLanguage:        wc.AssistLanguage,
		Language:              wc.Language,
		CaseCreatePrompt:      wc.CaseCreatePrompt,
		Jobs:                  wc.Jobs,
		CaseMode:              wc.CaseMode,
		SlackMonitorChannelID: wc.SlackMonitorChannel,
		CaseStatusSet:         wc.CaseStatusSet,

		AllowExistingSlackChannel: wc.AllowExistingSlackChannel,
	}
	registry := model.NewWorkspaceRegistry()
	registry.Register(entry)
	return registry, entry
}

// seedCases injects the scenario's prior cases into the memory repository. Field
// values are not populated in v1 (the scenario `cases.fields` are reserved for
// future search use); core identity fields are set so the repository's
// create-time validation passes.
func seedCases(ctx context.Context, repo interfaces.Repository, wsID string, sc *scenario.Scenario) ([]*model.Case, error) {
	now := time.Now().UTC()
	out := make([]*model.Case, 0, len(sc.Cases))
	for i := range sc.Cases {
		cs := sc.Cases[i]
		c := &model.Case{
			Title:          cs.Title,
			Description:    cs.Description,
			Status:         types.CaseStatusOpen,
			ReporterID:     seedReporterID,
			SlackChannelID: sc.Workspace.SlackMonitorChannel,
			BoardStatus:    cs.BoardStatus,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		created, err := repo.Case().Create(ctx, wsID, c)
		if err != nil {
			return nil, goerr.Wrap(err, "seed case", goerr.V("title", cs.Title))
		}
		out = append(out, created)
	}
	return out, nil
}

// buildJobRunner wires a JobRunner mirroring the production job runtime
// (pkg/cli/job_runtime.go) with the in-memory env: read-only core tools plus
// action writer tools, dispatching every strategy onto the agent runtime exactly
// as serve and tick do. Sources reach the job through the system prompt
// (resolveSources), not tools, so the seeded sources are surfaced here.
// (casewriter / slackpost tools are omitted in v1 — they require host adapters
// not exported from the usecase layer.)
//
// No in-process executor is registered, for the same reason production registers
// none when a runtime exists: every strategy the runtime handles must go through
// it, or a scenario would pass against a second implementation.
func buildJobRunner(
	repo interfaces.Repository,
	registry *model.WorkspaceRegistry,
	uc *usecase.UseCases,
	llm gollem.LLMClient,
	durable *job.DurableRuntime,
) *job.JobRunner {
	actionAdapter := usecase.NewActionToolAdapter(uc.Action)
	stepAdapter := usecase.NewActionStepToolAdapter(uc.ActionStep)
	caseAdapter := usecase.NewCaseToolAdapter(uc.Case)
	memoAdapter := usecase.NewMemoToolAdapter(uc.Memo)
	knowledgeAccessor := usecase.NewKnowledgeToolAccessor(uc.Knowledge, uc.Tag)
	knowledgeMutator := usecase.NewKnowledgeToolMutator(uc.Knowledge, uc.Tag)

	toolBuilder := job.ToolBuilderFunc(func(_ context.Context, c *model.Case, ws *model.WorkspaceEntry) []gollem.Tool {
		var statusSet *model.ActionStatusSet
		var caseStatusSet *model.ActionStatusSet
		var fieldSchema *modelconfig.FieldSchema
		wsID := ""
		if ws != nil {
			statusSet = ws.ActionStatusSet
			caseStatusSet = ws.CaseStatusSet
			fieldSchema = ws.FieldSchema
			wsID = ws.Workspace.ID
		}
		caseID := int64(0)
		if c != nil {
			caseID = c.ID
		}
		coreDeps := core.Deps{
			Repo:         repo,
			WorkspaceID:  wsID,
			CaseID:       caseID,
			StatusSet:    statusSet,
			ActionUC:     actionAdapter,
			ActionStepUC: stepAdapter,
			CaseRefUC:    uc.Case,
		}
		out := make([]gollem.Tool, 0, 16)
		// Action tools exist only in channel-mode workspaces; thread-mode cases
		// have no Actions and the usecase boundary rejects action writes there.
		// Mirror the production Job runtime (pkg/cli/job_runtime.go), which gates
		// the core/actionwriter tools on workspace mode.
		if ws == nil || !ws.IsThreadMode() {
			out = append(out, core.NewReadOnly(coreDeps)...)
			out = append(out, actionwriter.New(coreDeps)...)
		}
		out = append(out, casewriter.New(casewriter.Deps{
			CaseUC:      caseAdapter,
			WorkspaceID: wsID,
			CaseID:      caseID,
			Schema:      fieldSchema,
			StatusSet:   caseStatusSet,
		})...)
		if ws != nil && ws.MemoConfig.Enabled() {
			out = append(out, memotool.New(memotool.Deps{
				Repo:        repo,
				WorkspaceID: wsID,
				CaseID:      caseID,
				MemoUC:      memoAdapter,
				Schema:      ws.MemoConfig.FieldSchema,
			})...)
		}
		if knowledgeAccessor != nil {
			kdeps := knowledgetool.Deps{WorkspaceID: wsID, Accessor: knowledgeAccessor}
			if knowledgeMutator != nil && c != nil && !c.IsPrivate {
				kdeps.Mutator = knowledgeMutator
				out = append(out, knowledgetool.New(kdeps)...)
			} else {
				out = append(out, knowledgetool.NewReadOnly(kdeps)...)
			}
		}
		return out
	})

	return job.NewJobRunner(job.RunnerDeps{
		Repo:            repo,
		Registry:        registry,
		LLMClient:       llm,
		ToolBuilder:     toolBuilder,
		Durable:         durable,
		WorkspaceAccess: uc.WorkspaceAccess,
	})
}

// seedSources injects the scenario's workspace data sources into the memory
// repository so source-aware tools / workflows read them from the same repo.
func seedSources(ctx context.Context, repo interfaces.Repository, wsID string, sc *scenario.Scenario) error {
	now := time.Now().UTC()
	for i := range sc.Sources {
		src := toModelSource(sc.Sources[i], now)
		if _, err := repo.Source().Create(ctx, wsID, src); err != nil {
			return goerr.Wrap(err, "seed source", goerr.V("name", src.Name), goerr.V("type", string(src.SourceType)))
		}
	}
	return nil
}

// toModelSource maps a scenario source (already validated) to a model.Source
// with a fresh id and the matching typed config.
func toModelSource(s scenario.Source, now time.Time) *model.Source {
	src := &model.Source{
		ID:          model.NewSourceID(),
		Name:        s.Name,
		SourceType:  model.SourceType(s.Type),
		Description: s.Description,
		Enabled:     s.IsEnabled(),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	switch model.SourceType(s.Type) {
	case model.SourceTypeNotionDB:
		src.NotionDBConfig = &model.NotionDBConfig{
			DatabaseID:    s.NotionDB.DatabaseID,
			DatabaseTitle: s.NotionDB.DatabaseTitle,
			DatabaseURL:   s.NotionDB.DatabaseURL,
		}
	case model.SourceTypeNotionPage:
		src.NotionPageConfig = &model.NotionPageConfig{
			PageID:    s.NotionPage.PageID,
			PageTitle: s.NotionPage.PageTitle,
			PageURL:   s.NotionPage.PageURL,
			Recursive: s.NotionPage.Recursive,
			MaxDepth:  s.NotionPage.MaxDepth,
		}
	case model.SourceTypeSlack:
		channels := make([]model.SlackChannel, 0, len(s.Slack.Channels))
		for _, ch := range s.Slack.Channels {
			channels = append(channels, model.SlackChannel{ID: ch.ID, Name: ch.Name})
		}
		src.SlackConfig = &model.SlackConfig{Channels: channels}
	case model.SourceTypeGitHub:
		repos := make([]model.GitHubRepository, 0, len(s.GitHub.Repositories))
		for _, r := range s.GitHub.Repositories {
			repos = append(repos, model.GitHubRepository{Owner: r.Owner, Repo: r.Repo})
		}
		src.GitHubConfig = &model.GitHubConfig{Repositories: repos}
	}
	return src
}
