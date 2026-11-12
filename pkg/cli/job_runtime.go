// Job runtime wiring shared between `hecatoncheires serve` (which hosts
// the Case lifecycle publisher) and `hecatoncheires scheduled` (which
// fires only the time-driven sweep). Both ultimately drive the same
// JobUseCase / JobRunner.
package cli

import (
	"context"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/trace"
	"github.com/m-mizutani/goerr/v2"
	"github.com/urfave/cli/v3"

	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casewriter"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/core"
	knowledgetool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/knowledge"
	memotool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/memo"
	notiontool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/notion"
	slacktool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slackpost"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/webfetch"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	modelconfig "github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/service/notion"
	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	jobagent "github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/job"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
)

// jobReflectionLoopMax bounds the reflection agent's internal tool-calling
// loop. Set here at the wiring layer (not inside the reflector) so the budget
// stays configurable from the caller per project convention.
const jobReflectionLoopMax = 20

// Timing of the deployment-wide Job concurrency slots. Fixed here at the
// wiring layer (only the limit itself is operator-configurable) so no default
// hides inside pkg/usecase/job.
const (
	// jobSlotTTL is the delay between a holder's last heartbeat and its slot
	// becoming reusable — i.e. how long a crashed instance blocks a slot. Kept
	// short because at a limit of 1 that delay is lost throughput.
	jobSlotTTL = 30 * time.Second
	// jobSlotRenewInterval is TTL/3, so two consecutive renew failures still
	// leave a third attempt before a live run's slot expires.
	jobSlotRenewInterval = 10 * time.Second
	// jobSlotMaxHold stops a leaked hold from renewing itself forever. It is
	// not a run timeout: only the renewal stops, the run continues.
	jobSlotMaxHold = 2 * time.Hour
)

// tickRuntime bundles the dependencies the tick CLI / HTTP endpoint
// need to fire a sweep.
type tickRuntime struct {
	repo     interfaces.Repository
	registry *model.WorkspaceRegistry
	scanner  *job.ScheduledScanner
	// durable is the agent runtime the dispatched runs execute on. The sweep owns
	// its worker: it spawns the runs and then drains them in the foreground, so a
	// scheduled sweep does not depend on a serve instance being up to execute what
	// it dispatched.
	durable *job.DurableRuntime
	// cleanup releases what the runtime opened (the agent process repository, the
	// Cloud Storage client). Always non-nil.
	cleanup func()
}

// buildTickRuntime wires the minimal dependency graph for a scheduled-Job
// sweep. This includes the Job runner (so dispatched Jobs can actually
// execute), but excludes the full HTTP / Slack worker stack the serve
// command needs.
func buildTickRuntime(
	ctx context.Context,
	appCfg *config.AppConfig,
	repoCfg *config.Repository,
	llmCfg *config.LLM,
	embCfg *config.Embedding,
	jobCfg *config.JobConcurrency,
	agentCfg *config.Agent,
	storageCfg *config.Storage,
	integrationCfg tickIntegrationConfigs,
	c *cli.Command,
) (*tickRuntime, error) {
	_, registry, err := appCfg.Configure(c)
	if err != nil {
		return nil, goerr.Wrap(err, "load workspace configs")
	}
	repo, err := repoCfg.Configure(ctx)
	if err != nil {
		return nil, goerr.Wrap(err, "init repository")
	}

	modelSetup, err := buildLLMSetup(ctx, c, appCfg, llmCfg, registry, agentCfg.BudgetOr)
	if err != nil {
		return nil, goerr.Wrap(err, "resolve the LLM models")
	}
	llmClient := modelSetup.Default

	integrations, err := configureTickIntegrations(ctx, integrationCfg)
	if err != nil {
		return nil, err
	}
	// Slack and the LLM are required together, in both directions.
	//
	// Slack-without-LLM is what usecase.New enforces with a panic; surfacing it
	// here turns a stack trace into a sentence, which is what a process started by
	// a scheduler should produce.
	//
	// LLM-without-Slack is refused because a sweep with an LLM dispatches agent
	// runs, and Slack is the only way an unattended run reports anything: it would
	// mutate cases and tell nobody. It also leaves slack_post and slack_ro
	// resolving to nothing while the Job palette still advertises them.
	switch {
	case integrations.slack != nil && llmClient == nil:
		return nil, goerr.New("a Slack bot token is configured but no model is; " +
			"a sweep with Slack wired must also be given --llm-model")
	case integrations.slack == nil && llmClient != nil:
		return nil, goerr.New("a model is configured but no Slack bot token is; " +
			"a sweep that dispatches agent runs must be given --slack-bot-token, " +
			"or the runs would mutate cases with no way to report it")
	}

	ucOpts := integrations.ucOpts
	// The usecase set needs the LLM and embedding clients too, not just the agent
	// runtime.
	//
	// The webfetch client is built only when BOTH the HTTP settings and an LLM
	// client are present, because the LLM screen is this codebase's only
	// prompt-injection defense and webfetch fails closed without it. The knowledge
	// tools' similarity search runs on the embedder. Omitting either left a toolset
	// the Job palette advertises resolving to nothing.
	//
	// Embedding is required whenever an LLM is configured, and refused loudly
	// rather than defaulted — the same contract serve enforces, so a sweep cannot
	// silently run knowledge-blind Jobs a serve instance would have refused to
	// start for.
	if llmClient != nil {
		ucOpts = append(ucOpts, usecase.WithLLMClient(llmClient))
		if !embCfg.IsEnabled() {
			return nil, goerr.New("--embedding-gemini-project-id is required when --llm-model is set")
		}
		embedClient, eErr := embCfg.NewClient(ctx)
		if eErr != nil {
			return nil, goerr.Wrap(eErr, "init embedding client for the sweep")
		}
		ucOpts = append(ucOpts, usecase.WithEmbedClient(embedClient))
		logging.Default().Info("Embedding client enabled for the sweep", logAttrsToArgs(embCfg.LogAttrs())...)
	}
	uc := usecase.New(repo, registry, ucOpts...)

	// The sweep runs its Jobs on the same agent runtime serve does, so a scheduled
	// run gets the same step / token budget and the same one-transition-at-a-time
	// checkpointing. It differs only in who drives the worker: serve runs one
	// continuously, a sweep drains its own runs and exits.
	durable, cleanup, err := buildTickAgentRuntime(ctx, tickAgentDeps{
		repo: repo, registry: registry, llm: llmClient, models: modelSetup.Policy, uc: uc,
		agentCfg: agentCfg, storageCfg: storageCfg, repoCfg: repoCfg,
		slackSvc: integrations.slack, jiraTools: integrations.jiraTools,
		slotLimit: jobCfg.Limit(),
	})
	if err != nil {
		return nil, err
	}

	jobUC, jobRunner, err := buildJobRuntime(jobRuntimeDeps{
		Repo:      repo,
		Registry:  registry,
		LLMClient: llmClient,
		UC:        uc,
		WebFetch:  uc.WebFetchClient(),
		// The tick CLI dispatches Job runs itself, so it must enforce the same
		// deployment-wide limit as serve — a sweep that skipped the gate would
		// defeat it entirely.
		SlotLimit:   jobCfg.Limit(),
		SlotLimiter: durable.SlotLimiter,
		// Mirror the tool wiring done in serve.go so every Job host resolves the
		// same tool set. SlackService additionally backs the runner's own session
		// log and the interaction poster, so a sweep reports its runs exactly the
		// way serve does.
		SlackService:   integrations.slack,
		SlackSearch:    uc.SlackSearchService(),
		SlackRetriever: uc.SlackMessageRetriever(),
		SlackLimits:    uc.SlackToolLimits(),
		NotionTool:     uc.NotionToolClient(),
		JiraTools:      integrations.jiraTools,
		Durable:        durable.Runtime,
	})
	if err != nil {
		cleanup()
		return nil, goerr.Wrap(err, "build job runtime")
	}
	durable.Runtime.AttachRunner(jobRunner)
	uc.Case.SetEventPublisher(jobUC)

	scanner := job.NewScheduledScanner(job.ScannerDeps{
		Repo:      repo,
		Registry:  registry,
		Publisher: jobUC,
	})

	return &tickRuntime{
		repo:     repo,
		registry: registry,
		scanner:  scanner,
		durable:  durable.Runtime,
		cleanup:  cleanup,
	}, nil
}

// tickIntegrationConfigs is the external-integration configuration a sweep
// parses. Every field is required to be non-nil: a sweep EXECUTES the runs it
// dispatches, so each of these decides whether a tool the Job palette advertises
// actually exists. An unconfigured integration is expressed by its own config
// being empty, never by omitting it here.
// GitHub is deliberately absent: a sweep runs only Job agents, and
// agent.KnownToolSetIDsJob withholds the github toolset from an unattended run
// on purpose. Configuring a GitHub client here would build one no Job can reach.
type tickIntegrationConfigs struct {
	Slack *config.Slack
	// SlackTool bounds what a Slack read tool's result may inject into the model
	// context. It is required for the same reason the others are: a sweep that
	// left it out would run the Slack reads unbounded while serve bounded them.
	SlackTool *config.SlackTool
	Jira      *config.Jira
	WebFetch  *config.WebFetch
	// NotionToken backs both the Source service and the notion__* agent tools,
	// exactly as it does in serve. Empty disables both.
	NotionToken string
	// BaseURL is the web UI origin the Slack messages a run produces link back
	// to. A Job that files an Action posts a notification carrying that Action's
	// URL, so without this the link is dropped from a scheduled run's message but
	// present on the identical message from serve.
	BaseURL string
}

// Validate enforces the non-nil contract above.
func (c tickIntegrationConfigs) Validate() error {
	if c.Slack == nil {
		return goerr.New("slack configuration is required")
	}
	if c.SlackTool == nil {
		return goerr.New("slack agent tool configuration is required")
	}
	if err := c.SlackTool.Validate(); err != nil {
		return err
	}
	if c.Jira == nil {
		return goerr.New("jira configuration is required")
	}
	if c.WebFetch == nil {
		return goerr.New("webfetch configuration is required")
	}
	return nil
}

// tickIntegrations is what configureTickIntegrations built. The two concrete
// values are the ones no usecase accessor exposes; everything else reaches the
// tool wiring through the usecase options.
type tickIntegrations struct {
	slack     slacksvc.Service
	jiraTools []gollem.Tool
	ucOpts    []usecase.Option
}

// configureTickIntegrations builds the external clients a sweep's Job runs need
// and the usecase options that install them. It mirrors serve.go's integration
// blocks, minus everything only an HTTP process uses (Slack OAuth, the signing
// secret, the org-level detection that guards channel creation — a sweep creates
// no channels).
//
// It exists because the sweep does not hand its runs to a serve instance: it
// drives the agent worker itself, so the Job agent's tools are built from THIS
// process's clients. Leaving one unconfigured does not degrade gracefully — the
// Job palette still advertises the toolset id to the planner, which then assigns
// the model a tool that resolves to nothing and the call fails with "unknown
// tool". Each unconfigured integration is logged at startup for that reason.
func configureTickIntegrations(ctx context.Context, cfg tickIntegrationConfigs) (tickIntegrations, error) {
	if err := cfg.Validate(); err != nil {
		return tickIntegrations{}, goerr.Wrap(err, "invalid sweep integration configuration")
	}
	out := tickIntegrations{ucOpts: []usecase.Option{
		usecase.WithBaseURL(cfg.BaseURL),
		// Installed unconditionally, like serve: the bounds must hold wherever
		// the Slack read tools exist, and which token wired them is irrelevant.
		usecase.WithSlackToolLimits(cfg.SlackTool.Limits()),
	}}
	if cfg.BaseURL == "" {
		logging.Default().Warn("Base URL not configured; Slack messages a scheduled run posts will carry no link back to the web UI")
	}

	if cfg.Slack.BotToken() != "" {
		svc, err := slacksvc.New(cfg.Slack.BotToken())
		if err != nil {
			return tickIntegrations{}, goerr.Wrap(err, "init slack service for the sweep")
		}
		out.slack = svc
		out.ucOpts = append(out.ucOpts,
			usecase.WithSlackService(svc),
			usecase.WithNotificationSlotDuration(cfg.Slack.NotificationSlotDuration()),
		)

		// The User OAuth token is what lets the read tools reach public channels
		// the bot has not joined, and is the only token search.messages accepts.
		// Without it the sweep keeps the bot-token reads, exactly as serve does.
		if cfg.Slack.UserOAuthToken() != "" {
			searchSvc, sErr := slacktool.NewSearchClient(cfg.Slack.UserOAuthToken())
			if sErr != nil {
				return tickIntegrations{}, goerr.Wrap(sErr, "init slack search service for the sweep")
			}
			retrieverSvc, rErr := slacktool.NewMessageRetriever(cfg.Slack.UserOAuthToken())
			if rErr != nil {
				return tickIntegrations{}, goerr.Wrap(rErr, "init slack message retriever for the sweep")
			}
			out.ucOpts = append(out.ucOpts,
				usecase.WithSlackSearchService(searchSvc),
				usecase.WithSlackMessageRetriever(retrieverSvc),
			)
		}
		logging.Default().Info("Slack service enabled for the sweep", logAttrsToArgs(cfg.Slack.LogAttrs())...)
	} else {
		logging.Default().Warn("Slack bot token not configured; scheduled Job runs will have no Slack tools and cannot report their results")
	}

	if cfg.NotionToken != "" {
		notionSvc, err := notion.New(cfg.NotionToken)
		if err != nil {
			return tickIntegrations{}, goerr.Wrap(err, "init notion service for the sweep")
		}
		notionToolClient, err := notiontool.NewClient(cfg.NotionToken)
		if err != nil {
			return tickIntegrations{}, goerr.Wrap(err, "init notion tool client for the sweep")
		}
		out.ucOpts = append(out.ucOpts,
			usecase.WithNotion(notionSvc),
			usecase.WithNotionToolClient(notionToolClient),
		)
		logging.Default().Info("Notion service enabled for the sweep")
	} else {
		logging.Default().Warn("Notion API token not configured; scheduled Job runs will have no notion__* tools")
	}

	jiraTools, err := cfg.Jira.Configure(ctx)
	if err != nil {
		return tickIntegrations{}, goerr.Wrap(err, "init jira tools for the sweep")
	}
	if jiraTools != nil {
		out.jiraTools = jiraTools
		out.ucOpts = append(out.ucOpts, usecase.WithJiraTools(jiraTools))
		logging.Default().Info("Jira service enabled for the sweep", logAttrsToArgs(cfg.Jira.LogAttrs())...)
	} else {
		logging.Default().Warn("Jira not configured; scheduled Job runs will have no jira_* tools")
	}

	// The webfetch tool screens fetched content for prompt injection through the
	// LLM, so it is only ever built alongside one — the same condition serve
	// applies.
	if cfg.WebFetch.IsEnabled() {
		out.ucOpts = append(out.ucOpts, usecase.WithWebFetch(cfg.WebFetch.Settings()))
		logging.Default().Info("WebFetch tool enabled for the sweep", logAttrsToArgs(cfg.WebFetch.LogAttrs())...)
	} else {
		logging.Default().Warn("WebFetch not enabled; scheduled Job runs will have no webfetch tool")
	}

	return out, nil
}

// tickAgentDeps is what building the sweep's agent runtime needs.
type tickAgentDeps struct {
	repo     interfaces.Repository
	registry *model.WorkspaceRegistry
	llm      gollem.LLMClient
	// models is which model each run generates through and what it may spend.
	// The sweep executes the runs itself, so it needs the same policy serve has.
	models     agentkernel.ModelPolicy
	uc         *usecase.UseCases
	agentCfg   *config.Agent
	storageCfg *config.Storage
	repoCfg    *config.Repository
	// slackSvc is the bot-token client. nil when Slack is unconfigured; every
	// Slack-backed tool then binds nothing.
	slackSvc slacksvc.Service
	// jiraTools carries the already-expanded Jira read tools. Unlike the other
	// integrations there is no usecase accessor to read them back from, so they
	// travel as a plain slice — see ToolDeps.JiraTools.
	jiraTools []gollem.Tool
	slotLimit int
}

// tickAgentRuntime is the sweep's agent runtime plus the slot gate the Job runner
// must share with it.
type tickAgentRuntime struct {
	Runtime     *job.DurableRuntime
	SlotLimiter *job.ConcurrencyLimiter
}

// buildTickAgentRuntime builds the Kernel a sweep's Job runs execute on, in the
// order agentkit requires: register, build, bind.
//
// Without an LLM client there is nothing for an agent to run, so it returns a
// runtime the Job runner will find unusable (`handles` reports false) and the sweep
// dispatches nothing — the same shape serve has.
func buildTickAgentRuntime(ctx context.Context, d tickAgentDeps) (*tickAgentRuntime, func(), error) {
	noop := func() {}
	if d.llm == nil {
		return &tickAgentRuntime{Runtime: &job.DurableRuntime{}}, noop, nil
	}

	archive, err := d.storageCfg.Configure(ctx)
	if err != nil {
		return nil, noop, goerr.Wrap(err, "configure the agent session archive")
	}
	// Archive.Close is a plain cleanup func, not an io.Closer, so it is called
	// through a variable rather than as a method on the struct.
	closeArchive := archive.Close

	procRepo, procCleanup, err := d.repoCfg.ConfigureAgentProcess(ctx)
	if err != nil {
		closeArchive()
		return nil, noop, goerr.Wrap(err, "configure the agent process repository")
	}
	cleanup := func() {
		procCleanup()
		closeArchive()
	}

	budgets, err := d.agentCfg.Budgets()
	if err != nil {
		cleanup()
		return nil, noop, err
	}

	locator, err := agentkernel.NewLocator(procRepo)
	if err != nil {
		cleanup()
		return nil, noop, goerr.Wrap(err, "build the agent process locator")
	}

	var slots *job.ConcurrencyLimiter
	if d.slotLimit > 0 {
		slots, err = buildJobSlotLimiter(d.repo, d.slotLimit)
		if err != nil {
			cleanup()
			return nil, noop, err
		}
	}

	reg := agentkit.NewRegistry()
	taskAgent, err := agentkernel.RegisterTaskAgent(reg, budgets.Task.Limiter(d.models.Resolve),
		archive.ProcessHistory)
	if err != nil {
		cleanup()
		return nil, noop, goerr.Wrap(err, "register the task sub-agent")
	}
	durable := &job.DurableRuntime{
		History: archive.ProcessHistory, Locator: locator, Models: d.models,
	}
	if err := durable.Register(reg, budgets.Root.Limiter(d.models.Resolve), taskAgent); err != nil {
		cleanup()
		return nil, noop, goerr.Wrap(err, "register the job agents")
	}

	// The same assembly serve uses, so a scheduled run resolves the same tool set.
	// Every client behind it comes from the sweep's own configuration (see
	// configureTickIntegrations), because the sweep executes the runs itself.
	toolDeps := d.uc.AgentToolDeps()
	k, err := agentkernel.Build(agentkernel.Deps{
		Repo:    procRepo,
		History: archive.ProcessHistory,
		LLM:     d.llm,
		Trace:   archive.Trace,
		Budgets: budgets,
		Models:  d.models,
		Agents:  reg,
		Slots:   slots,
		Tools:   toolDeps,
	})
	if err != nil {
		cleanup()
		return nil, noop, goerr.Wrap(err, "build the agent runtime")
	}
	probe, err := agentkernel.NewToolSetProbe(toolDeps)
	if err != nil {
		cleanup()
		return nil, noop, goerr.Wrap(err, "build the agent toolset probe")
	}
	durable.Bind(k, probe)
	// The sweep waits for exactly the runs it dispatched, so it must remember them.
	durable.TrackSpawns()

	logging.Default().Info("Agent runtime configured for the sweep",
		logAttrsToArgs(d.agentCfg.LogAttrs())...)
	return &tickAgentRuntime{Runtime: durable, SlotLimiter: slots}, cleanup, nil
}

// jobRuntimeDeps groups everything the JobUseCase / JobRunner need at
// construction time.
type jobRuntimeDeps struct {
	Repo         interfaces.Repository
	Registry     *model.WorkspaceRegistry
	LLMClient    gollem.LLMClient
	UC           *usecase.UseCases
	SlackService slacksvc.Service // may be nil; slack_post tool then no-ops
	WebFetch     *webfetch.Client // may be nil; webfetch tool then not bound

	// Read-only tools the Job agent uses to read its case thread and do
	// corroboration. Each is nil-safe: the corresponding constructor binds no
	// tool when its dependency is nil, so an unconfigured deployment simply
	// runs without that tool (and the prompt's "do nothing if you can't read"
	// guard takes over).
	SlackSearch    slacktool.SearchService    // slack__search_messages
	SlackRetriever slacktool.MessageRetriever // slack__get_messages via User token
	NotionTool     notiontool.Client          // notion__search / notion__get_page / notion__get_database

	// SlackLimits bounds how much of a Slack read tool's result reaches the
	// model context (see config.SlackTool). The zero value disables both bounds.
	SlackLimits slacktool.Limits

	// JiraTools carries the already-expanded Jira read tools (see
	// pkg/agent/tool/jira). Unlike NotionTool this is not a client type:
	// gollem exposes no exported helper to turn a gollem.ToolSet into
	// []gollem.Tool, so config.Jira.Configure expands it once at startup
	// and hands the result through as a plain tool slice. nil/empty means
	// Jira is not configured.
	JiraTools []gollem.Tool

	// SlotLimit caps how many scheduled Job runs execute concurrently across
	// the whole deployment (see config.JobConcurrency). 0 means no limit and
	// builds no limiter at all.
	SlotLimit int

	// SlotLimiter, when non-nil, is the limiter to use instead of building one.
	// `serve` must pass the same instance it gave the Kernel as its slot gate:
	// with a durable Job the claim is what occupies a slot for the length of the
	// run, so a second limiter here would count a different set of holds and the
	// deployment-wide limit would bound neither side correctly.
	SlotLimiter *job.ConcurrencyLimiter

	// HistoryRepo / TraceRepo are required when wiring the planexec
	// executor (it needs persistent storage to replay sub-agent
	// reasoning). Nil falls back to in-memory implementations so the
	// scheduled-tick CLI command (which does not configure storage)
	// still gets a fully wired runtime.
	HistoryRepo gollem.HistoryRepository
	TraceRepo   trace.Repository

	// Durable, when non-nil, is the agent runtime the simple-strategy Jobs run
	// on. The caller creates it, hands it here, then registers and binds it
	// around the Kernel build — the order agentkit requires. Nil keeps every
	// strategy on the in-process executors.
	Durable *job.DurableRuntime
}

// registryHasInteractiveJob reports whether any enabled Job in any workspace
// is interactive. Used at serve startup to enforce that interactive Jobs —
// which suspend and resume across requests / instances — have a persistent
// (shared) agent history backend.
func registryHasInteractiveJob(registry *model.WorkspaceRegistry) bool {
	if registry == nil {
		return false
	}
	for _, ws := range registry.List() {
		if ws == nil {
			continue
		}
		for _, j := range ws.Jobs {
			if j != nil && !j.Disabled && j.Interactive {
				return true
			}
		}
	}
	return false
}

// buildJobRuntime constructs the JobRunner + JobUseCase pair, with a
// ToolBuilder that binds every read-only and writer tool the spec calls
// for to each invocation.
//
// It returns an error when the concurrency limiter cannot be built: a
// deployment that asked for a limit must fail to start rather than silently
// dispatch Job runs unbounded.
func buildJobRuntime(deps jobRuntimeDeps) (*job.UseCase, *job.JobRunner, error) {
	adapters := jobToolAdapters{
		action:            usecase.NewActionToolAdapter(deps.UC.Action),
		step:              usecase.NewActionStepToolAdapter(deps.UC.ActionStep),
		caseUC:            usecase.NewCaseToolAdapter(deps.UC.Case),
		caseRef:           deps.UC.Case,
		memo:              usecase.NewMemoToolAdapter(deps.UC.Memo),
		knowledgeAccessor: usecase.NewKnowledgeToolAccessor(deps.UC.Knowledge, deps.UC.Tag),
		knowledgeMutator:  usecase.NewKnowledgeToolMutator(deps.UC.Knowledge, deps.UC.Tag),
	}

	toolBuilder := job.ToolBuilderFunc(func(_ context.Context, c *model.Case, ws *model.WorkspaceEntry) []gollem.Tool {
		return buildJobTools(deps, adapters, c, ws)
	})

	// Every strategy runs on the durable runtime whenever one exists — `serve` keeps
	// a worker running, and the `tick` CLI drains its own runs before it exits — so
	// no executor is registered for it.
	//
	// A deployment with no LLM configured builds no Kernel at all, and there the
	// in-process executor is still what makes a run RECORDED: it fails on the absent
	// model, but `Run` reaches the finish stage and writes the run log and the
	// FAILED outcome an operator reads. Without it such a run would abort in the
	// prepare stage, leaving nothing in the run history to explain itself.
	executors := inProcessExecutors(deps.Durable)

	// Reflection agent: knowledge/tag tools only, sharing the same knowledge use
	// cases as the Job tools. Disabled (nil reflector) if knowledge is not
	// configured. It reads a finished run's transcript from the agent history store,
	// by the ref recorded on the Process.
	var reflector jobagent.Reflector
	if deps.LLMClient != nil {
		if refl, rErr := jobagent.NewLLMReflector(jobagent.ReflectorDeps{
			LLMClient:         deps.LLMClient,
			KnowledgeAccessor: adapters.knowledgeAccessor,
			KnowledgeMutator:  adapters.knowledgeMutator,
			LoopMax:           jobReflectionLoopMax,
		}); rErr == nil {
			reflector = refl
		}
	}

	// Wire the operational session-log notifier only when a Slack service is
	// present. Leaving it nil (e.g. the scheduled-tick CLI) disables the
	// starting / progress / completion markers without affecting the run.
	var slackNotifier job.SlackNotifier
	if deps.SlackService != nil {
		slackNotifier = slackNotifierAdapter{svc: deps.SlackService}
	}

	deps2 := job.RunnerDeps{
		Repo:          deps.Repo,
		Registry:      deps.Registry,
		LLMClient:     deps.LLMClient,
		Executors:     executors,
		ToolBuilder:   toolBuilder,
		SlackNotifier: slackNotifier,
		Reflector:     reflector,
		Durable:       deps.Durable,
		// The same authorizer every other Slack entry point uses, so an answer
		// to an interactive Job's question is held to the workspace's policy.
		WorkspaceAccess: deps.UC.WorkspaceAccess,
	}
	// The interactive-Job question form is Block Kit posted/updated directly
	// via the Slack service (the narrow SlackNotifier cannot carry blocks).
	// Wired only when Slack is present; without it an interactive Job that
	// emits a question fails loudly at the Interactor (it has no surface).
	if deps.SlackService != nil {
		deps2.InteractionPoster = deps.SlackService
	}
	// Deployment-wide concurrency gate on scheduled runs. `serve` builds it before
	// the Kernel (it is also the Kernel's slot gate) and hands the same instance
	// here; `tick` has no Kernel, so it builds one now.
	if deps.SlotLimiter != nil {
		deps2.SlotLimiter = deps.SlotLimiter
	} else if deps.SlotLimit > 0 {
		limiter, err := buildJobSlotLimiter(deps.Repo, deps.SlotLimit)
		if err != nil {
			return nil, nil, err
		}
		deps2.SlotLimiter = limiter
	}
	runner := job.NewJobRunner(deps2)
	jobUC := job.NewUseCase(deps.Registry, runner)
	return jobUC, runner, nil
}

// inProcessExecutors returns the in-process executors a deployment still needs.
//
// With an agent runtime there are none: every strategy is Spawned onto it, in both
// entry points (`serve` keeps a worker running, `tick` drains its own runs).
//
// Without one — a deployment with no LLM configured builds no Kernel at all — the
// single-loop executor is what keeps a run RECORDED. It fails on the absent model,
// but `Run` reaches its finish stage and writes the run log and the FAILED outcome
// an operator reads in the run history. Returning nothing here instead would abort
// such a run in the prepare stage, leaving no row to explain itself.
func inProcessExecutors(durable *job.DurableRuntime) map[model.JobStrategy]jobagent.JobExecutor {
	if durable != nil {
		return map[model.JobStrategy]jobagent.JobExecutor{}
	}
	return map[model.JobStrategy]jobagent.JobExecutor{
		model.JobStrategySimple: jobagent.NewSingleLoopJobExecutor(),
	}
}

// buildJobSlotLimiter builds the deployment-wide concurrency gate for scheduled
// Job runs. limit must be positive; 0 means ungated and the caller skips this.
//
// It is a standalone constructor because `serve` needs the limiter BEFORE the
// Kernel exists — the Kernel takes it as its claim-time slot gate, which is what
// bounds concurrent execution now that a durable run outlives the call that
// started it. Building a second one for the Runner would leave each counting only
// its own holds.
func buildJobSlotLimiter(repo interfaces.Repository, limit int) (*job.ConcurrencyLimiter, error) {
	limiter, err := job.NewConcurrencyLimiter(job.ConcurrencyLimiterDeps{
		Repo:          repo.JobSlot(),
		Limit:         limit,
		TTL:           jobSlotTTL,
		RenewInterval: jobSlotRenewInterval,
		MaxHold:       jobSlotMaxHold,
	})
	if err != nil {
		return nil, goerr.Wrap(err, "build job concurrency limiter",
			goerr.V("job_max_concurrency", limit))
	}
	// The slot timings are compiled-in, so this is the only place they are
	// visible; without them a slot_hold_ms cannot be read against the TTL that
	// would have expired it.
	logging.Default().Info("job concurrency limiter enabled",
		"job_max_concurrency", limit,
		"slot_ttl", jobSlotTTL.String(),
		"slot_renew_interval", jobSlotRenewInterval.String(),
		"slot_max_hold", jobSlotMaxHold.String())
	return limiter, nil
}

// jobToolAdapters groups the usecase-to-tool adapters once so buildJobTools
// can be called per Job invocation without rebuilding them each time.
type jobToolAdapters struct {
	action            core.ActionMutator
	step              core.ActionStepMutator
	caseUC            casewriter.CaseMutator
	caseRef           core.CaseRefReader
	memo              memotool.MemoMutator
	knowledgeAccessor knowledgetool.KnowledgeAccessor
	knowledgeMutator  knowledgetool.KnowledgeMutator
}

// buildJobTools assembles the tool slice for a single Job invocation. Action
// tools (read-only list/get plus the writer set) are bound only for
// channel-mode workspaces: a thread-mode workspace's Actions are managed from
// the Web UI, and its Job agent must not be able to read or mutate them. Case-editing
// (casewriter, incl. thread-mode board status), Slack post, web fetch and memo
// tools are bound in both modes.
func buildJobTools(deps jobRuntimeDeps, adapters jobToolAdapters, c *model.Case, ws *model.WorkspaceEntry) []gollem.Tool {
	var statusSet *model.ActionStatusSet
	var caseStatusSet *model.ActionStatusSet
	var fieldSchema *modelconfig.FieldSchema
	if ws != nil {
		statusSet = ws.ActionStatusSet
		caseStatusSet = ws.CaseStatusSet
		fieldSchema = ws.FieldSchema
	}
	caseID := int64(0)
	channelID := ""
	threadTS := ""
	if c != nil {
		caseID = c.ID
		channelID = c.SlackChannelID
		// Thread-mode cases post Job output into the case thread rather
		// than the monitored channel's root.
		threadTS = c.SlackThreadTS
	}
	wsID := ""
	if ws != nil {
		wsID = ws.Workspace.ID
	}

	coreDeps := core.Deps{
		Repo:         deps.Repo,
		WorkspaceID:  wsID,
		CaseID:       caseID,
		StatusSet:    statusSet,
		ActionUC:     adapters.action,
		ActionStepUC: adapters.step,
		CaseRefUC:    adapters.caseRef,
	}

	out := make([]gollem.Tool, 0, 16)
	// Action tools follow the workspace mode and [case] actions; see
	// job.ActionTools, which the eval harness shares.
	out = append(out, job.ActionTools(ws, coreDeps)...)
	out = append(out, casewriter.New(casewriter.Deps{
		CaseUC:      adapters.caseUC,
		WorkspaceID: wsID,
		CaseID:      caseID,
		Schema:      fieldSchema,
		StatusSet:   caseStatusSet,
	})...)
	if deps.SlackService != nil && channelID != "" {
		out = append(out, slackpost.New(slackpost.Deps{
			Poster:          usecase.NewSlackPoster(deps.SlackService),
			ChannelID:       channelID,
			DefaultThreadTS: threadTS,
		})...)
	}
	// Slack read-only tools (slack__get_messages / slack__search_messages). Not
	// Action tools, so wired in both channel- and thread-mode. NewReadOnly does
	// NOT include the post tool (posting stays on slackpost above); ChannelID is
	// intentionally omitted here. get_messages binds on a non-nil Bot and reads
	// via the User-token Retriever when present, else via the Bot if it is a
	// channel member; search_messages binds only on a non-nil Search.
	out = append(out, slacktool.NewReadOnly(slacktool.Deps{
		Bot:       deps.SlackService,
		Search:    deps.SlackSearch,
		Retriever: deps.SlackRetriever,
		Limits:    deps.SlackLimits,
	})...)
	// Notion read-only tools (notion__search / notion__get_page /
	// notion__get_database). New returns no tool when the client is nil, so this
	// is safe in deployments without Notion.
	out = append(out, notiontool.New(notiontool.Deps{Client: deps.NotionTool})...)
	out = append(out, webfetch.New(deps.WebFetch)...)
	// Jira read tools (jira_list_projects / jira_search_issues / jira_get_issues).
	// Already expanded at startup (see JiraTools doc comment); appended
	// unconditionally, same as the other integration tool sets above — an
	// empty/nil slice is a safe no-op.
	out = append(out, deps.JiraTools...)
	// Case-scoped memo tools, wired only when the workspace enabled memos.
	if ws != nil && ws.MemoConfig.Enabled() {
		out = append(out, memotool.New(memotool.Deps{
			Repo:        deps.Repo,
			WorkspaceID: wsID,
			CaseID:      caseID,
			MemoUC:      adapters.memo,
			Schema:      ws.MemoConfig.FieldSchema,
		})...)
	}
	// Workspace-wide knowledge tools (not Actions, so available in both modes).
	// Read is always offered; write is withheld while the Job runs against a
	// PRIVATE case (its contents must not leak into shared knowledge).
	if adapters.knowledgeAccessor != nil {
		kdeps := knowledgetool.Deps{WorkspaceID: wsID, Accessor: adapters.knowledgeAccessor}
		if adapters.knowledgeMutator != nil && c != nil && !c.IsPrivate {
			kdeps.Mutator = adapters.knowledgeMutator
			out = append(out, knowledgetool.New(kdeps)...)
		} else {
			out = append(out, knowledgetool.NewReadOnly(kdeps)...)
		}
	}
	return out
}
