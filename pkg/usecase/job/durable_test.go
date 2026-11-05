package job_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gollem-dev/agentkit"
	agentprocmemory "github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/budget"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/planexec"
	slackgo "github.com/slack-go/slack"
	"testing"
	"time"

	"github.com/m-mizutani/gt"

	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/agentarchive"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// A one-shot sweep must execute the runs it dispatched before it exits.
//
// `Run` returns as soon as a run is recorded, so a batch command that only
// dispatched would leave the work for a worker that may not be running — a
// scheduled sweep would silently stop executing anything. Drain is what makes the
// sweep own its runs: it drives the worker in the foreground until every run it
// started is terminal.
func TestDurableRuntime_DrainExecutesTheRunsTheSweepDispatched(t *testing.T) {
	ctx := context.Background()
	repo, c := setupCase(t, "ws")
	registry := model.NewWorkspaceRegistry()

	jobs := make([]*model.Job, 0, 2)
	for i := range 2 {
		jobs = append(jobs, &model.Job{
			ID:     fmt.Sprintf("swept-%d", i),
			Prompt: "summarise the case",
			Events: model.JobEvents{
				Scheduled: &model.ScheduledEventConfig{Every: time.Hour},
			},
		})
	}
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}, Jobs: jobs})

	llm := singleReplyLLM("the case looks fine", 120, 34)
	durable := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore()}
	runner := job.NewJobRunner(job.RunnerDeps{
		Repo: repo, Registry: registry, LLMClient: llm, Durable: durable,
	})
	// No worker: Drain is the only thing that can move these runs, which is exactly
	// the situation `hecatoncheires tick` is in.
	bindDurableJobRuntime(t, runner, durable, repo, registry, llm)
	durable.TrackSpawns()

	for _, j := range jobs {
		gt.NoError(t, runner.Run(ctx, j, job.Event{
			Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID,
			Timestamp: time.Now().UTC(),
		})).Required()
	}

	// Nothing has executed yet: the runs are recorded and waiting.
	for _, j := range jobs {
		key := model.JobRunKey{WorkspaceID: "ws", CaseID: c.ID, JobID: j.ID}
		logs, err := repo.JobRunLog().List(ctx, key, 10)
		gt.NoError(t, err).Required()
		gt.Array(t, logs).Length(1).Required()
		gt.Value(t, logs[0].Stage).Equal(model.JobRunStageRunning)
	}

	gt.NoError(t, durable.Drain(ctx)).Required()

	// Drain returned only once every run reached its outcome, so the results are
	// readable without waiting for anything.
	for _, j := range jobs {
		key := model.JobRunKey{WorkspaceID: "ws", CaseID: c.ID, JobID: j.ID}
		run, err := repo.JobRun().Get(ctx, key)
		gt.NoError(t, err).Required()
		gt.Value(t, run).NotNil().Required()
		gt.Value(t, run.LastStatus).Equal(model.JobRunStatusSuccess)

		logs, err := repo.JobRunLog().List(ctx, key, 10)
		gt.NoError(t, err).Required()
		gt.Array(t, logs).Length(1).Required()
		gt.Value(t, logs[0].Stage).Equal(model.JobRunStageSuccess)
	}
}

// TestDurableRuntime_SpawnCarriesTheJobsModelAndBudget pins how a per-Job model
// and budget reach the runtime: on the run's own metadata, decided at spawn.
//
// They travel with the run rather than being looked up later because the
// configuration can be replaced while the run is still going — a run must be
// judged against what it started under, and must generate through the model it
// was started with.
func TestDurableRuntime_SpawnCarriesTheJobsModelAndBudget(t *testing.T) {
	ctx := context.Background()
	repo, c := setupCase(t, "ws")
	registry := model.NewWorkspaceRegistry()

	withModel := &model.Job{
		ID:       "with_model",
		Prompt:   "summarise the case",
		LLMModel: "cheap",
		Budget:   pricing.FromUSD(0.5),
		Events:   model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
	}
	withoutModel := &model.Job{
		ID:     "without_model",
		Prompt: "summarise the case",
		Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
	}
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws"},
		Jobs:      []*model.Job{withModel, withoutModel},
	})

	llm := singleReplyLLM("the case looks fine", 120, 34)
	durable := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore()}
	runner := job.NewJobRunner(job.RunnerDeps{
		Repo: repo, Registry: registry, LLMClient: llm, Durable: durable,
	})
	// No worker: the run stays where Run left it, so its metadata can be read
	// with nothing racing to finish it.
	k := bindDurableJobRuntime(t, runner, durable, repo, registry, llm)

	scopeOf := func(j *model.Job) agentkernel.Scope {
		t.Helper()
		gt.NoError(t, runner.Run(ctx, j, job.Event{
			Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID,
			Timestamp: time.Now().UTC(),
		})).Required()

		busy, err := durable.Locator.Busy(ctx, agentkernel.JobRunSubject("ws", c.ID, j.ID))
		gt.NoError(t, err).Required()
		gt.Value(t, busy).NotNil().Required()

		proc, err := k.GetProcess(ctx, busy.ProcessID)
		gt.NoError(t, err).Required()
		return agentkernel.ScopeFrom(proc.Metadata)
	}

	named := scopeOf(withModel)
	gt.String(t, named.LLMModel).Equal("cheap")
	gt.Value(t, named.Budget).Equal(pricing.FromUSD(0.5))

	// A Job naming neither leaves both unset, which is what the runtime reads as
	// "the deployment's default".
	plain := scopeOf(withoutModel)
	gt.String(t, plain.LLMModel).Equal("")
	gt.Value(t, plain.Budget).Equal(pricing.NanoUSD(0))
}

// A durable Job run carries the workspace language on its own metadata and in the
// planner's language directive, and its completion marker follows it. A
// workspace that configures no language leaves both empty, so the run keeps the
// deployment default and a prompt with no language directive, as before.
func TestDurableRuntime_RunFollowsTheWorkspaceLanguage(t *testing.T) {
	i18n.Init(i18n.LangEN)
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		wsLang    i18n.Lang
		wantLang  string
		wantLabel string
		wantDone  string
	}{
		{
			name: "workspace ja", wsLang: i18n.LangJA, wantLang: "ja", wantLabel: "Japanese",
			wantDone: i18n.T(i18n.ContextWithLang(ctx, i18n.LangJA), i18n.MsgJobRunCompleted, "triage"),
		},
		{
			name: "workspace unset", wsLang: "", wantLang: "", wantLabel: "",
			wantDone: i18n.T(i18n.ContextWithLang(ctx, i18n.LangEN), i18n.MsgJobRunCompleted, "triage"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, c := setupCaseWithSlack(t, "ws", "C1", "")
			registry := model.NewWorkspaceRegistry()
			j := &model.Job{
				ID:       "triage",
				Prompt:   "summarise the case",
				Strategy: model.JobStrategyPlanexec,
				Events:   model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
			}
			registry.Register(&model.WorkspaceEntry{
				Workspace: model.Workspace{ID: "ws"},
				Jobs:      []*model.Job{j},
				Language:  tc.wsLang,
			})

			llm := singleReplyLLM("the case looks fine", 120, 34)
			fake := &fakeNotifier{rootTS: "root-123"}
			durable := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore()}
			runner := job.NewJobRunner(job.RunnerDeps{
				Repo: repo, Registry: registry, LLMClient: llm, Durable: durable,
				SlackNotifier: fake,
			})
			k := bindDurableJobRuntime(t, runner, durable, repo, registry, llm)
			durable.TrackSpawns()

			gt.NoError(t, runner.Run(ctx, j, job.Event{
				Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID,
				Timestamp: time.Now().UTC(),
			})).Required()

			busy, err := durable.Locator.Busy(ctx, agentkernel.JobRunSubject("ws", c.ID, j.ID))
			gt.NoError(t, err).Required()
			gt.Value(t, busy).NotNil().Required()
			proc, err := k.GetProcess(ctx, busy.ProcessID)
			gt.NoError(t, err).Required()
			gt.String(t, agentkernel.ScopeFrom(proc.Metadata).Lang).Equal(tc.wantLang)

			var spawned struct {
				Input struct {
					LanguageLabel string `json:"language_label"`
				} `json:"input"`
			}
			gt.NoError(t, json.Unmarshal(proc.State, &spawned)).Required()
			gt.String(t, spawned.Input.LanguageLabel).Equal(tc.wantLabel)

			gt.NoError(t, durable.Drain(ctx)).Required()
			calls := fake.snapshot()
			gt.Array(t, calls).Length(2).Required()
			gt.String(t, calls[1].text).Equal(tc.wantDone)
		})
	}

	// The completion marker follows the language the run was spawned with, even
	// if the configuration loaded when it finishes says otherwise.
	t.Run("configuration changed while the run was in flight", func(t *testing.T) {
		repo, c := setupCaseWithSlack(t, "ws", "C1", "")
		registry := model.NewWorkspaceRegistry()
		j := &model.Job{
			ID:     "triage",
			Prompt: "summarise the case",
			Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
		}
		entry := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}, Jobs: []*model.Job{j}, Language: i18n.LangJA}
		registry.Register(entry)

		llm := singleReplyLLM("the case looks fine", 120, 34)
		fake := &fakeNotifier{rootTS: "root-123"}
		durable := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore()}
		runner := job.NewJobRunner(job.RunnerDeps{
			Repo: repo, Registry: registry, LLMClient: llm, Durable: durable,
			SlackNotifier: fake,
		})
		bindDurableJobRuntime(t, runner, durable, repo, registry, llm)
		durable.TrackSpawns()

		gt.NoError(t, runner.Run(ctx, j, job.Event{
			Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID,
			Timestamp: time.Now().UTC(),
		})).Required()
		entry.Language = ""
		gt.NoError(t, durable.Drain(ctx)).Required()

		jaCtx := i18n.ContextWithLang(ctx, i18n.LangJA)
		calls := fake.snapshot()
		gt.Array(t, calls).Length(2).Required()
		gt.String(t, calls[0].text).Equal(i18n.T(jaCtx, i18n.MsgJobRunStarting, "triage"))
		gt.String(t, calls[1].text).Equal(i18n.T(jaCtx, i18n.MsgJobRunCompleted, "triage"))
	})
}

// The run's start time must reach the sub-agents, not only the planner's system
// prompt. Without it a task whose text says "last week" or "by today" has no
// instant to resolve against and the model falls back on whatever date its
// training suggests.
//
// It asserts the instant the RunnerDeps.Clock produced, so a wiring that dropped
// the value or reached for a second clock is caught. caseTaskContext's own
// rendering is covered by TestCaseTaskContext_CarriesTheCaseThreadNotTheSessionThread;
// what this pins is JobRunner.Run passing the run's own instant into it.
func TestDurableRuntime_SpawnCarriesTheRunsStartTimeToSubAgents(t *testing.T) {
	ctx := context.Background()
	repo, c := setupCase(t, "ws")
	registry := model.NewWorkspaceRegistry()

	// Planexec: the task context exists only on that path — the simple strategy
	// runs one ReAct loop and spawns no sub-agent to tell.
	j := &model.Job{
		ID:       "triage",
		Prompt:   "summarise the case",
		Strategy: model.JobStrategyPlanexec,
		Events:   model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
	}
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws"},
		Jobs:      []*model.Job{j},
	})

	startedAt := time.Date(2026, 9, 7, 3, 35, 19, 0, time.UTC)
	llm := singleReplyLLM("the case looks fine", 120, 34)
	durable := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore()}
	runner := job.NewJobRunner(job.RunnerDeps{
		Repo: repo, Registry: registry, LLMClient: llm, Durable: durable,
		Clock: func() time.Time { return startedAt },
	})
	// No worker: the run stays where Run left it, so the state it was spawned with
	// can be read with nothing racing to advance it.
	k := bindDurableJobRuntime(t, runner, durable, repo, registry, llm)

	gt.NoError(t, runner.Run(ctx, j, job.Event{
		Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID,
		Timestamp: startedAt,
	})).Required()

	busy, err := durable.Locator.Busy(ctx, agentkernel.JobRunSubject("ws", c.ID, j.ID))
	gt.NoError(t, err).Required()
	gt.Value(t, busy).NotNil().Required()

	proc, err := k.GetProcess(ctx, busy.ProcessID)
	gt.NoError(t, err).Required()

	// The task context is the only channel a sub-agent has: it receives neither
	// the host's system prompt nor its user prompt.
	var spawned struct {
		Input struct {
			TaskContext  string `json:"task_context"`
			SystemPrompt string `json:"system_prompt"`
		} `json:"input"`
	}
	gt.NoError(t, json.Unmarshal(proc.State, &spawned)).Required()
	gt.String(t, spawned.Input.TaskContext).Contains("- current_time: 2026-09-07T03:35:19Z (UTC)")
	// The planner is told the same instant through its own system prompt, so the
	// two cannot disagree about what "today" means inside one run.
	gt.String(t, spawned.Input.SystemPrompt).Contains("2026-09-07T03:35:19Z")
}

// TestDurableRuntime_RecordsWhatTheRunCost pins the record a finished run leaves:
// the tokens it consumed AND what they cost at the rate of the model it ran on,
// plus that model's own name.
//
// The cost is stored rather than derived later because it cannot be recomputed:
// the token counts alone do not say which model produced them, and a configured
// price may be corrected after the run is over.
func TestDurableRuntime_RecordsWhatTheRunCost(t *testing.T) {
	ctx := context.Background()
	repo, c := setupCase(t, "ws")
	registry := model.NewWorkspaceRegistry()

	j := &model.Job{
		ID:     "priced",
		Prompt: "summarise the case",
		Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
	}
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws"}, Jobs: []*model.Job{j},
	})

	// 1200 input and 340 output tokens on the test model ($1 / $5 per MTok).
	llm := singleReplyLLM("the case looks fine", 1200, 340)
	durable := &job.DurableRuntime{
		History: agentarchive.NewMemoryHistoryStore(),
		Models:  testModelPolicy(t),
	}
	runner := job.NewJobRunner(job.RunnerDeps{
		Repo: repo, Registry: registry, LLMClient: llm, Durable: durable,
	})
	bindDurableJobRuntime(t, runner, durable, repo, registry, llm)
	durable.TrackSpawns()

	gt.NoError(t, runner.Run(ctx, j, job.Event{
		Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID,
		Timestamp: time.Now().UTC(),
	})).Required()
	gt.NoError(t, durable.Drain(ctx)).Required()

	key := model.JobRunKey{WorkspaceID: "ws", CaseID: c.ID, JobID: j.ID}
	logs, err := repo.JobRunLog().List(ctx, key, 10)
	gt.NoError(t, err).Required()
	gt.Array(t, logs).Length(1).Required()
	got := logs[0]

	gt.Value(t, got.Stage).Equal(model.JobRunStageSuccess)
	gt.String(t, got.Model).Equal("test-model")
	// The recorded cost is the recorded tokens priced at that model's rate, so the
	// two halves of the record cannot disagree.
	want := pricing.Rate{Input: 1000, Output: 5000}.Cost(
		got.InputTokens, got.OutputTokens,
		got.CacheReadInputTokens, got.CacheCreationInputTokens)
	gt.Value(t, got.CostNanoUSD).Equal(int64(want))
	gt.Number(t, got.CostNanoUSD).Greater(0)
}

// This test uses only the pre-policy public API so it can also run against the
// base revision. An existing Job keeps its prompts, model, tools, accounting,
// and reflection lifecycle when it has no execution policy.
func TestDurableRuntime_UnconfiguredExecutionPreservesLegacyContract(t *testing.T) {
	for _, strategy := range []model.JobStrategy{model.JobStrategySimple, model.JobStrategyPlanexec} {
		t.Run(string(strategy), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			repo, c := setupCase(t, "ws")
			startedAt := time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC)
			j := &model.Job{
				ID: "review", Name: "Case review", Description: "Review the current case evidence",
				Prompt:   "Review case {{.Case.ID}} for {{.Event.Domain}} using {{.Job.LLMModel}}.",
				Strategy: strategy, LLMModel: "named", Budget: pricing.FromUSD(1), Reflection: true,
				Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
			}
			ws := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}, Jobs: []*model.Job{j}}
			registry := model.NewWorkspaceRegistry()
			registry.Register(ws)
			ev := job.Event{Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID, Timestamp: startedAt}
			wantSystem, err := job.BuildSystemPrompt(job.PromptInputs{Job: j, Workspace: ws, Case: c, Event: ev, Now: startedAt})
			gt.NoError(t, err).Required()
			wantUser := fmt.Sprintf("Review case %d for scheduled using named.", c.ID)
			replies := []string{"The case review is complete."}
			wantSpawns := int64(0)
			if strategy == model.JobStrategyPlanexec {
				replies = []string{
					`{"tasks":[{"id":"review","title":"Review evidence","description":"Inspect the current case evidence","acceptance_criteria":"Return the observed case state","tools":["core_job"],"budget_usd":0.1}]}`,
					"The evidence confirms the current case state.",
					`{"finalize":{"reason":"The case evidence was reviewed"}}`,
					"The case review is complete.",
				}
				wantSpawns = 1
			}
			type capturedCall struct {
				config gollem.SessionConfig
				inputs []gollem.Input
			}
			var mu sync.Mutex
			var calls []capturedCall
			named := &mock.LLMClientMock{NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
				cfg := gollem.NewSessionConfig(opts...)
				return &mock.SessionMock{
					GenerateFunc: func(_ context.Context, inputs []gollem.Input, opts ...gollem.GenerateOption) (*gollem.Response, error) {
						mu.Lock()
						defer mu.Unlock()
						i := len(calls)
						calls = append(calls, capturedCall{config: cfg, inputs: slices.Clone(inputs)})
						if i >= len(replies) {
							return nil, goerr.New("unexpected extra generation", goerr.V("call", i))
						}
						generate := gollem.NewGenerateConfig(opts...)
						gt.Bool(t, generate.ToolCallsDisabled()).False()
						gt.Value(t, generate.MaxTokens()).Nil()
						return &gollem.Response{Texts: []string{replies[i]}, InputToken: 120, OutputToken: 34,
							CacheReadInputToken: 40, CacheCreationInputToken: 20}, nil
					},
					HistoryFunc: func() (*gollem.History, error) {
						return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
					},
				}, nil
			}}
			fallback := &mock.LLMClientMock{NewSessionFunc: func(context.Context, ...gollem.SessionOption) (gollem.Session, error) {
				return nil, goerr.New("the configured named model must handle every generation")
			}}
			rate := pricing.Rate{Input: 1000, Output: 5000, CacheRead: 100, CacheWrite: 1250}
			models, err := agentkernel.NewModelPolicy(agentkernel.ModelPolicyInput{
				Defs: []agentkernel.ModelDef{
					{Ref: "fallback", Provider: agentkernel.ProviderClaude, Model: "fallback-model", Rate: pricing.Rate{Input: 10000, Output: 50000}},
					{Ref: "named", Provider: agentkernel.ProviderClaude, Model: "named-model", Rate: rate},
				}, DefaultRef: "fallback", DefaultBudget: pricing.FromUSD(2), Clients: map[string]gollem.LLMClient{"named": named},
			})
			gt.NoError(t, err).Required()
			durable := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore(), Models: models}
			reflector := &fakeReflector{}
			runner := job.NewJobRunner(job.RunnerDeps{Repo: repo, Registry: registry, LLMClient: fallback, Durable: durable,
				Reflector: reflector, Clock: func() time.Time { return startedAt }})
			reg := agentkit.NewRegistry()
			cfg := budget.Config{MaxSteps: 64, MaxInputTokens: 100000, MaxOutputTokens: 100000, NoticeRatio: 0.8}
			root := budget.Root{MaxSteps: 64, NoticeRatio: 0.8}
			task, err := agentkernel.RegisterTaskAgent(reg, cfg.Limiter(models.Resolve), durable.History)
			gt.NoError(t, err).Required()
			gt.NoError(t, durable.Register(reg, root.Limiter(models.Resolve), task)).Required()
			procRepo := agentprocmemory.New()
			durable.Locator, err = agentkernel.NewLocator(procRepo)
			gt.NoError(t, err).Required()
			k, err := agentkernel.Build(agentkernel.Deps{Repo: procRepo, History: durable.History, LLM: fallback,
				Trace: agentarchive.NewMemoryTraceRepository(), Tools: agentkernel.ToolDeps{Repo: repo, Registry: registry},
				Budgets: agentkernel.Budgets{Root: root, Task: cfg}, Models: models, Agents: reg})
			gt.NoError(t, err).Required()
			durable.Bind(k, nil)
			durable.AttachRunner(runner)
			durable.TrackSpawns()
			gt.NoError(t, runner.Run(ctx, j, ev)).Required()
			busy, err := durable.Locator.Busy(ctx, agentkernel.JobRunSubject("ws", c.ID, j.ID))
			gt.NoError(t, err).Required()
			gt.Value(t, busy).NotNil().Required()
			gt.NoError(t, durable.Drain(ctx, agentkit.WithPollInterval(time.Millisecond))).Required()
			async.Wait()

			proc, err := k.GetProcess(ctx, busy.ProcessID)
			gt.NoError(t, err).Required()
			gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)
			wantAgent := agentkernel.AgentJobSimple
			if strategy == model.JobStrategyPlanexec {
				wantAgent = agentkernel.AgentJob
			}
			gt.Value(t, proc.Agent).Equal(wantAgent)
			gt.Value(t, proc.Metrics.Spawns).Equal(wantSpawns)
			scope := agentkernel.ScopeFrom(proc.Metadata)
			gt.String(t, scope.LLMModel).Equal("named")
			gt.Value(t, scope.Budget).Equal(j.Budget)
			for _, key := range []string{"job_execution", "stage_role", "stage_owner"} {
				_, present := proc.Metadata[key]
				gt.Bool(t, present).False()
			}
			events, err := k.ListEvents(ctx, proc.ID)
			gt.NoError(t, err).Required()
			for _, event := range events {
				gt.Bool(t, strings.HasPrefix(string(event.Type), "hecatoncheires.usage.")).False()
				gt.Bool(t, strings.HasPrefix(string(event.Type), "hecatoncheires.cascade.")).False()
			}
			gt.Array(t, fallback.NewSessionCalls()).Length(0)
			mu.Lock()
			observed := slices.Clone(calls)
			mu.Unlock()
			gt.Array(t, observed).Length(len(replies)).Required()
			wantInputs := []gollem.Input{gollem.Text(wantUser)}
			if strategy == model.JobStrategyPlanexec {
				wantInputs = []gollem.Input{gollem.Text("[budget] remaining $1.00 of $1.00"), gollem.Text(wantUser)}
			}
			gt.Array(t, observed[0].inputs).Equal(wantInputs)
			readTools := []string{"core__get_action", "core__list_actions"}
			jobTools := []string{"core__add_action_step", "core__create_action", "core__get_action", "core__list_actions",
				"core__rename_action_step", "core__set_action_assignee", "core__set_action_step_done", "core__update_action", "core__update_action_status"}
			for i, call := range observed {
				gt.String(t, call.config.SystemPrompt()).NotContains("Advisor handoff")
				gt.String(t, call.config.SystemPrompt()).NotContains("Orchestrator")
				var names []string
				for _, tool := range call.config.Tools() {
					names = append(names, tool.Spec().Name)
				}
				slices.Sort(names)
				if strategy == model.JobStrategySimple {
					gt.String(t, call.config.SystemPrompt()).Equal(wantSystem)
					gt.Array(t, names).Equal(jobTools)
				} else {
					if i == 1 {
						gt.Array(t, names).Equal(jobTools)
						gt.String(t, call.config.SystemPrompt()).Contains("Review evidence")
						gt.String(t, call.config.SystemPrompt()).Contains("2026-08-12T09:30:00Z")
					} else {
						gt.Array(t, names).Equal(readTools)
						gt.String(t, call.config.SystemPrompt()).Contains(wantSystem)
						gt.String(t, call.config.SystemPrompt()).Equal(observed[0].config.SystemPrompt())
					}
				}
			}
			logs, err := repo.JobRunLog().List(ctx, model.JobRunKey{WorkspaceID: "ws", CaseID: c.ID, JobID: j.ID}, 10)
			gt.NoError(t, err).Required()
			gt.Array(t, logs).Length(1).Required()
			log := logs[0]
			gt.Value(t, log.Stage).Equal(model.JobRunStageSuccess)
			gt.String(t, log.Model).Equal("named-model")
			gt.Value(t, log.LLMCallCount).Equal(int64(len(replies)))
			gt.Value(t, log.InputTokens).Equal(120 * int64(len(replies)))
			gt.Value(t, log.OutputTokens).Equal(34 * int64(len(replies)))
			gt.Value(t, log.CacheReadInputTokens).Equal(40 * int64(len(replies)))
			gt.Value(t, log.CacheCreationInputTokens).Equal(20 * int64(len(replies)))
			gt.Value(t, log.CostNanoUSD).Equal(int64(rate.Cost(proc.Metrics.InputTokens, proc.Metrics.OutputTokens,
				proc.Metrics.CacheReadInputTokens, proc.Metrics.CacheCreationInputTokens)))
			gt.Value(t, log.CostNanoUSD).Equal(259000 * int64(len(replies)))
			gt.Array(t, reflector.calls).Length(1).Required()
			reflection := reflector.calls[0]
			gt.String(t, reflection.WorkspaceID).Equal("ws")
			gt.Value(t, reflection.CaseID).Equal(c.ID)
			gt.String(t, reflection.JobID).Equal(j.ID)
			gt.String(t, reflection.JobName).Equal(j.Name)
			gt.String(t, reflection.JobDescription).Equal(j.Description)
			history, err := durable.History.Load(ctx, proc.ID, proc.HistoryRef)
			gt.NoError(t, err).Required()
			gt.Value(t, reflection.History).Equal(history)
		})
	}
}

// Drain waits for the runs THIS process started, not for whatever else is in the
// store. A sweep that waited on another instance's runs would hang for as long as
// they take, and a sweep that tracked nothing would exit having executed nothing.
func TestDurableRuntime_DrainWithoutTrackingWaitsForNothing(t *testing.T) {
	ctx := context.Background()
	repo, c := setupCase(t, "ws")
	registry := model.NewWorkspaceRegistry()
	j := &model.Job{
		ID:     "untracked",
		Prompt: "summarise the case",
		Events: model.JobEvents{
			Scheduled: &model.ScheduledEventConfig{Every: time.Hour},
		},
	}
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}, Jobs: []*model.Job{j}})

	llm := singleReplyLLM("the case looks fine", 120, 34)
	durable := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore()}
	runner := job.NewJobRunner(job.RunnerDeps{
		Repo: repo, Registry: registry, LLMClient: llm, Durable: durable,
	})
	bindDurableJobRuntime(t, runner, durable, repo, registry, llm)
	// TrackSpawns deliberately NOT called: this stands in for `serve`, which never
	// drains and must not accumulate a list of every run it has ever started.

	gt.NoError(t, runner.Run(ctx, j, job.Event{
		Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID,
		Timestamp: time.Now().UTC(),
	})).Required()

	// Returns immediately rather than driving the run, and does not block.
	gt.NoError(t, durable.Drain(ctx)).Required()

	key := model.JobRunKey{WorkspaceID: "ws", CaseID: c.ID, JobID: j.ID}
	logs, err := repo.JobRunLog().List(ctx, key, 10)
	gt.NoError(t, err).Required()
	gt.Array(t, logs).Length(1).Required()
	gt.Value(t, logs[0].Stage).Equal(model.JobRunStageRunning)
}

// A Job's sub-agents are told the CASE's thread, not the thread the run's
// operational log consolidates into. The two differ for a channel-mode case,
// where the log thread is rooted fresh at the "starting" marker: telling a
// sub-agent that one would send it to read the run's own progress notes instead
// of the conversation the case is about.
func TestCaseTaskContext_CarriesTheCaseThreadNotTheSessionThread(t *testing.T) {
	key := model.JobRunKey{WorkspaceID: "ws-1", CaseID: 7, JobID: "triage"}
	c := &model.Case{
		ID:             7,
		SlackChannelID: "C-CASE",
		SlackThreadTS:  "1700000000.000100",
	}

	now := time.Date(2026, 9, 7, 3, 35, 19, 0, time.UTC)
	got := job.CaseTaskContextForTest(key, c, now)
	gt.Value(t, got.WorkspaceID).Equal("ws-1")
	gt.Value(t, got.CaseID).Equal(int64(7))
	gt.Value(t, got.SlackChannelID).Equal("C-CASE")
	gt.Value(t, got.SlackThreadTS).Equal("1700000000.000100")

	rendered, err := got.Render()
	gt.NoError(t, err).Required()
	gt.String(t, rendered).Contains("- slack_thread_ts: 1700000000.000100")
	// The run's start time reaches the sub-agents too, so they resolve a relative
	// date the same way the planner's own system prompt does.
	gt.String(t, rendered).Contains("- current_time: 2026-09-07T03:35:19Z (UTC)")
}

// A run whose case could not be loaded still names the workspace and case it is
// for; the Slack lines are simply absent rather than empty.
func TestCaseTaskContext_WithoutACaseCarriesOnlyTheKey(t *testing.T) {
	key := model.JobRunKey{WorkspaceID: "ws-1", CaseID: 7, JobID: "triage"}

	got := job.CaseTaskContextForTest(key, nil, time.Date(2026, 9, 7, 3, 35, 19, 0, time.UTC))
	gt.Value(t, got.WorkspaceID).Equal("ws-1")
	gt.Value(t, got.CaseID).Equal(int64(7))
	gt.Value(t, got.SlackChannelID).Equal("")
	gt.Value(t, got.SlackThreadTS).Equal("")

	rendered, err := got.Render()
	gt.NoError(t, err).Required()
	gt.Bool(t, strings.Contains(rendered, "slack_channel_id")).False()
	gt.Bool(t, strings.Contains(rendered, "slack_thread_ts")).False()
}

func TestDurableExecutionPolicySelectsFromBaseStrategy(t *testing.T) {
	for _, tt := range []struct {
		name     string
		strategy model.JobStrategy
		mode     model.JobExecutionMode
		cascade  bool
		target   model.JobExecutionTarget
		agent    agentkit.AgentName
		active   bool
	}{
		{"simple standard keeps strategy", model.JobStrategySimple, model.JobExecutionModeStandard, false, model.JobExecutionTargetJobSimple, agentkernel.AgentJobSimple, true},
		{"combined starts controller", model.JobStrategySimple, model.JobExecutionModeOrchestrator, true, model.JobExecutionTargetJobSimple, agentkernel.AgentJobCascade, true},
		{"excluded combined keeps strategy", model.JobStrategySimple, model.JobExecutionModeOrchestrator, true, model.JobExecutionTargetJob, agentkernel.AgentJobSimple, false},
		{"simple orchestrator promotes", model.JobStrategySimple, model.JobExecutionModeOrchestrator, false, model.JobExecutionTargetJobSimple, agentkernel.AgentJob, true},
		{"planexec cascade starts controller", model.JobStrategyPlanexec, model.JobExecutionModeStandard, true, model.JobExecutionTargetJob, agentkernel.AgentJobCascade, true},
		{"simple cascade starts controller", model.JobStrategySimple, model.JobExecutionModeStandard, true, model.JobExecutionTargetJobSimple, agentkernel.AgentJobCascade, true},
		{"excluded simple remains legacy", model.JobStrategySimple, model.JobExecutionModeOrchestrator, false, model.JobExecutionTargetJob, agentkernel.AgentJobSimple, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			repo, c := setupCase(t, "ws")
			j := &model.Job{ID: "configured", Prompt: "check the case", Strategy: tt.strategy, LLMModel: "test", Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}}, Execution: &model.JobExecution{Mode: tt.mode, Cascade: tt.cascade, ApplyTo: []model.JobExecutionTarget{tt.target}, Models: model.JobExecutionModels{Advisor: "test"}}}
			registry := model.NewWorkspaceRegistry()
			registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}, Jobs: []*model.Job{j}})
			llm := singleReplyLLM("done", 10, 5)
			d := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore(), Models: testModelPolicy(t)}
			runner := job.NewJobRunner(job.RunnerDeps{Repo: repo, Registry: registry, LLMClient: llm, Durable: d})
			k := bindDurableJobRuntime(t, runner, d, repo, registry, llm)
			gt.NoError(t, runner.Run(ctx, j, job.Event{Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID, Timestamp: time.Now()})).Required()
			busy, err := d.Locator.Busy(ctx, agentkernel.JobRunSubject("ws", c.ID, j.ID))
			gt.NoError(t, err).Required()
			gt.Value(t, busy).NotNil().Required()
			proc, err := k.GetProcess(ctx, busy.ProcessID)
			gt.NoError(t, err).Required()
			gt.Value(t, proc.Agent).Equal(tt.agent)
			sc := agentkernel.ScopeFrom(proc.Metadata)
			gt.Value(t, sc.JobExecution != nil).Equal(tt.active)
			if tt.active {
				gt.String(t, sc.JobExecution.Models.Main).Equal("test")
				gt.String(t, sc.JobExecution.Models.Sub).Equal("test")
				j.Execution.Models.Main = "changed"
				gt.String(t, sc.JobExecution.Models.Main).Equal("test")
			}
			if tt.mode == model.JobExecutionModeOrchestrator && tt.active && !tt.cascade {
				var st struct {
					Input struct {
						SystemPrompt string `json:"system_prompt"`
						AllowDirect  bool   `json:"allow_direct"`
					} `json:"input"`
				}
				gt.NoError(t, json.Unmarshal(proc.State, &st)).Required()
				gt.String(t, st.Input.SystemPrompt).Contains("Orchestrated execution")
				gt.Bool(t, st.Input.AllowDirect).False()
			}
		})
	}
}

func TestDurableCascadeCompletesLogicalRunAndReflectionOnce(t *testing.T) {
	ctx := context.Background()
	repo, c := setupCaseWithSlack(t, "ws", "C-CASE", "thread")
	j := &model.Job{ID: "cascade", Prompt: "check the case", Reflection: true, Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}}, Execution: &model.JobExecution{Mode: model.JobExecutionModeStandard, Cascade: true, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJobSimple}, Models: model.JobExecutionModels{Main: "test", Advisor: "test"}}}
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}, Jobs: []*model.Job{j}})
	var calls atomic.Int32
	llm := &mock.LLMClientMock{NewSessionFunc: func(context.Context, ...gollem.SessionOption) (gollem.Session, error) {
		return &mock.SessionMock{
			GenerateFunc: func(context.Context, []gollem.Input, ...gollem.GenerateOption) (*gollem.Response, error) {
				if calls.Add(1) == 1 {
					return &gollem.Response{InputToken: 10, OutputToken: 5, FunctionCalls: []*gollem.FunctionCall{{ID: "handoff", Name: "job__request_advisor", Arguments: map[string]any{"reason": "conflict"}}}}, nil
				}
				return &gollem.Response{InputToken: 10, OutputToken: 5, Texts: []string{"advisor completed"}}, nil
			},
			HistoryFunc: func() (*gollem.History, error) {
				return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
			},
		}, nil
	}}
	reflector := &fakeReflector{}
	notifier := &fakeNotifier{}
	j.Execution.Models.Advisor = "advisor"
	models, err := agentkernel.NewModelPolicy(agentkernel.ModelPolicyInput{
		Defs: []agentkernel.ModelDef{
			{Ref: "test", Provider: agentkernel.ProviderClaude, Model: "main-model", Rate: pricing.Rate{Input: 1000, Output: 5000}},
			{Ref: "advisor", Provider: agentkernel.ProviderClaude, Model: "advisor-model", Rate: pricing.Rate{Input: 10000, Output: 50000}},
		},
		DefaultRef: "test", DefaultBudget: pricing.FromUSD(100),
		Clients: map[string]gollem.LLMClient{"advisor": singleReplyLLM("advisor completed", 10, 5)},
	})
	gt.NoError(t, err).Required()
	d := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore(), Models: models}
	runner := job.NewJobRunner(job.RunnerDeps{Repo: repo, Registry: registry, LLMClient: llm, Durable: d, Reflector: reflector, SlackNotifier: notifier})
	k := bindDurableJobRuntime(t, runner, d, repo, registry, llm)
	d.TrackSpawns()
	gt.NoError(t, runner.Run(ctx, j, job.Event{Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID, Timestamp: time.Now()})).Required()
	busy, err := d.Locator.Busy(ctx, agentkernel.JobRunSubject("ws", c.ID, j.ID))
	gt.NoError(t, err).Required()
	gt.Value(t, busy).NotNil().Required()
	gt.NoError(t, d.Drain(ctx)).Required()
	logs, err := repo.JobRunLog().List(ctx, model.JobRunKey{WorkspaceID: "ws", CaseID: c.ID, JobID: j.ID}, 10)
	gt.NoError(t, err).Required()
	gt.Array(t, logs).Length(1).Required()
	gt.Value(t, logs[0].Stage).Equal(model.JobRunStageSuccess)
	gt.Value(t, calls.Load()).Equal(int32(1))
	gt.Value(t, logs[0].LLMCallCount).Equal(int64(2))
	gt.Array(t, reflector.calls).Length(1)
	gt.Array(t, notifier.snapshot()).Length(2)
	gt.Value(t, logs[0].CostNanoUSD).Equal(int64(385000))
	usage := executionUsageRecords(t, k, busy.ProcessID)
	gt.Array(t, usage).Length(2)
	for _, call := range usage {
		gt.String(t, call.Phase).NotEqual("")
		gt.String(t, call.Phase).NotEqual("unknown")
	}
}

func TestExecutionPolicyRefusesLegacyFallbackWithoutDurable(t *testing.T) {
	j := &model.Job{ID: "configured", Prompt: "review the case", Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}}}
	j.Execution = &model.JobExecution{Mode: model.JobExecutionModeStandard, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJobSimple}}
	executor := &recordingExecutor{}
	runner, _, c := newRunner(t, "ws", []*model.Job{j}, executor)
	err := runner.RunManual(context.Background(), "ws", c.ID, j.ID, "operator")
	gt.Value(t, err).NotNil().Required()
	gt.Value(t, executor.calls.Load()).Equal(int32(0))
}

// executionScript supplies provider responses without accessing a real service.
type executionScript struct {
	responses []*gollem.Response
	calls     atomic.Int32
}

func (s *executionScript) client() gollem.LLMClient {
	return &mock.LLMClientMock{NewSessionFunc: func(context.Context, ...gollem.SessionOption) (gollem.Session, error) {
		return &mock.SessionMock{
			GenerateFunc: func(context.Context, []gollem.Input, ...gollem.GenerateOption) (*gollem.Response, error) {
				i := int(s.calls.Add(1)) - 1
				if i >= len(s.responses) {
					return nil, goerr.New("unexpected execution test generation")
				}
				return s.responses[i], nil
			},
			HistoryFunc: func() (*gollem.History, error) {
				return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
			},
		}, nil
	}}
}

type executionPoster struct {
	calls  atomic.Int32
	before func()
}

func (p *executionPoster) PostMessage(context.Context, string, []slackgo.Block, string) (string, error) {
	if p.before != nil {
		p.before()
	}
	p.calls.Add(1)
	return "posted", nil
}
func (p *executionPoster) PostThreadMessage(ctx context.Context, channel, thread string, blocks []slackgo.Block, text string) (string, error) {
	return p.PostMessage(ctx, channel, blocks, text)
}

func executionTask(toolset string) *gollem.Response {
	return cascadeText(fmt.Sprintf(`{"tasks":[{"id":"work","title":"Review case","description":"Inspect the case and follow the assigned scope","acceptance_criteria":"Return precise evidence and outcomes","tools":[%q],"budget_usd":0.1}]}`, toolset))
}

func TestDurableCombinedCascadeAccountsForBothSubtrees(t *testing.T) {
	for _, tc := range []struct {
		name                                              string
		writeBeforeHandoff, unknownUsage, exhaustedBudget bool
	}{
		{name: "handoff retains cheap workers"},
		{name: "descendant write prevents fresh advisor", writeBeforeHandoff: true},
		{name: "unknown usage prevents fresh advisor", unknownUsage: true},
		{name: "exhausted shared budget prevents advisor", exhaustedBudget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			repo, c := setupCaseWithSlack(t, "ws", "C-GENERIC", "thread")
			j := &model.Job{ID: "combined", Prompt: "Review case evidence and report the result", Budget: pricing.FromUSD(1), Reflection: true, Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}}, Execution: &model.JobExecution{Mode: model.JobExecutionModeOrchestrator, Cascade: true, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJobSimple}, Models: model.JobExecutionModels{Main: "main", Sub: "sub", Advisor: "advisor"}}}
			registry := model.NewWorkspaceRegistry()
			registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}, Jobs: []*model.Job{j}})
			set := "core_ro"
			if tc.writeBeforeHandoff {
				set = "slack_post"
			}
			main := &executionScript{responses: []*gollem.Response{executionTask(set), cascadeCalls(job.CascadeEscalationToolNameForTest)}}
			if tc.exhaustedBudget {
				main.responses = []*gollem.Response{cascadeCalls(job.CascadeEscalationToolNameForTest)}
				main.responses[0].InputToken = 1000000
				main.responses[0].OutputToken = 500000
			}
			if tc.unknownUsage {
				main.responses = []*gollem.Response{cascadeCalls(job.CascadeEscalationToolNameForTest)}
				main.responses[0].InputToken = 0
				main.responses[0].OutputToken = 0
			}
			write := &gollem.Response{InputToken: 10, OutputToken: 5, FunctionCalls: []*gollem.FunctionCall{{ID: "post", Name: "slack__post_to_case_channel", Arguments: map[string]any{"text": "The generic case review is complete."}}}}
			sub := &executionScript{responses: []*gollem.Response{cascadeText("Evidence includes a conflicting revision."), write, cascadeText("The exact requested message was posted.")}}
			if tc.writeBeforeHandoff {
				sub.responses = []*gollem.Response{write, cascadeText("The exact requested message was posted.")}
			}
			advisor := &executionScript{responses: []*gollem.Response{executionTask("slack_post"), cascadeText(`{"finalize":{"reason":"verified"}}`), cascadeText("Review completed.")}}
			models, err := agentkernel.NewModelPolicy(agentkernel.ModelPolicyInput{
				Defs: []agentkernel.ModelDef{
					{Ref: "main", Provider: agentkernel.ProviderClaude, Model: "main-model", Rate: pricing.Rate{Input: 1000, Output: 5000}},
					{Ref: "sub", Provider: agentkernel.ProviderClaude, Model: "sub-model", Rate: pricing.Rate{Input: 100, Output: 500}},
					{Ref: "advisor", Provider: agentkernel.ProviderClaude, Model: "advisor-model", Rate: pricing.Rate{Input: 10000, Output: 50000}},
				}, DefaultRef: "main", DefaultBudget: pricing.FromUSD(1), Clients: map[string]gollem.LLMClient{"sub": sub.client(), "advisor": advisor.client()},
			})
			gt.NoError(t, err).Required()
			d := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore(), Models: models}
			notifier := &fakeNotifier{}
			reflector := &fakeReflector{}
			runner := job.NewJobRunner(job.RunnerDeps{Repo: repo, Registry: registry, LLMClient: main.client(), Durable: d, SlackNotifier: notifier, Reflector: reflector})
			reg := agentkit.NewRegistry()
			cfg := budget.Config{MaxSteps: 128, MaxInputTokens: 100000, MaxOutputTokens: 100000, NoticeRatio: 0.8}
			rootLimit := budget.Root{MaxSteps: 128, NoticeRatio: 0.8}
			task, err := agentkernel.RegisterTaskAgent(reg, cfg.Limiter(models.Resolve), d.History)
			gt.NoError(t, err).Required()
			gt.NoError(t, d.Register(reg, rootLimit.Limiter(models.Resolve), task)).Required()
			procRepo := agentprocmemory.New()
			d.Locator, err = agentkernel.NewLocator(procRepo)
			gt.NoError(t, err).Required()
			poster := &executionPoster{}
			tools := agentkernel.ToolDeps{Repo: repo, Registry: registry, SlackPoster: poster}
			probe, err := agentkernel.NewToolSetProbe(tools)
			gt.NoError(t, err).Required()
			k, err := agentkernel.Build(agentkernel.Deps{Repo: procRepo, History: d.History, LLM: main.client(), Trace: agentarchive.NewMemoryTraceRepository(), Tools: tools, Budgets: agentkernel.Budgets{Root: rootLimit, Task: cfg}, Models: models, Agents: reg})
			gt.NoError(t, err).Required()
			d.Bind(k, probe)
			d.AttachRunner(runner)
			d.TrackSpawns()
			gt.NoError(t, runner.Run(ctx, j, job.Event{Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID, Timestamp: time.Now()})).Required()
			busy, err := d.Locator.Busy(ctx, agentkernel.JobRunSubject("ws", c.ID, j.ID))
			gt.NoError(t, err).Required()
			gt.Value(t, busy).NotNil().Required()
			rootID := busy.ProcessID
			poster.before = func() {
				written, err := models.CascadeWriteStarted(ctx, rootID)
				gt.NoError(t, err)
				gt.Value(t, written).Equal(tc.writeBeforeHandoff)
			}
			gt.NoError(t, d.Drain(ctx, agentkit.WithPollInterval(time.Millisecond))).Required()
			logs, err := repo.JobRunLog().List(ctx, model.JobRunKey{WorkspaceID: "ws", CaseID: c.ID, JobID: j.ID}, 10)
			gt.NoError(t, err).Required()
			gt.Array(t, logs).Length(1).Required()
			gt.Array(t, notifier.snapshot()).Length(2)
			rootProc, err := k.GetProcess(ctx, rootID)
			gt.NoError(t, err).Required()
			var state job.CascadeStateForTest
			gt.NoError(t, json.Unmarshal(rootProc.State, &state)).Required()
			mainProc, err := k.GetProcess(ctx, state.MainID)
			gt.NoError(t, err).Required()
			gt.Value(t, mainProc.Agent).Equal(agentkernel.AgentJob)
			gt.Value(t, agentkernel.ScopeFrom(mainProc.Metadata).Budget).Equal(pricing.FromUSD(0.5))
			if tc.writeBeforeHandoff || tc.unknownUsage || tc.exhaustedBudget {
				gt.Value(t, logs[0].Stage).Equal(model.JobRunStageFailed)
				gt.Value(t, advisor.calls.Load()).Equal(int32(0))
				gt.Value(t, state.AdvisorID).Equal(agentkit.ProcessID(""))
				gt.Array(t, reflector.calls).Length(0)
				if tc.writeBeforeHandoff {
					gt.Value(t, poster.calls.Load()).Equal(int32(1))
				} else {
					gt.Value(t, poster.calls.Load()).Equal(int32(0))
					gt.Value(t, sub.calls.Load()).Equal(int32(0))
				}
				return
			}
			gt.Value(t, logs[0].Stage).Equal(model.JobRunStageSuccess)
			gt.Value(t, logs[0].LLMCallCount).Equal(int64(8))
			gt.Value(t, logs[0].CostNanoUSD).Equal(int64(1130500))
			gt.Value(t, poster.calls.Load()).Equal(int32(1))
			gt.Array(t, reflector.calls).Length(1)
			gt.Value(t, state.MainCost).Equal(pricing.NanoUSD(73500))
			advisorProc, err := k.GetProcess(ctx, state.AdvisorID)
			gt.NoError(t, err).Required()
			gt.Value(t, advisorProc.Agent).Equal(agentkernel.AgentJob)
			gt.Value(t, advisorProc.InheritedHistory).Nil()
			gt.Value(t, agentkernel.ScopeFrom(advisorProc.Metadata).Budget).Equal(pricing.FromUSD(1) - state.MainCost)
			var advisorInput struct {
				Input planexec.Input `json:"input"`
			}
			gt.NoError(t, json.Unmarshal(advisorProc.State, &advisorInput)).Required()
			gt.Value(t, advisorInput.Input.HandoffTool).Nil()
			gt.String(t, advisorInput.Input.SystemPrompt).NotContains("Advisor handoff")
			usage := executionUsageRecords(t, k, rootID)
			gt.Array(t, usage).Length(8)
			var total pricing.NanoUSD
			owners := map[agentkernel.StageRole]bool{}
			for _, call := range usage {
				gt.String(t, call.RootID).Equal(string(rootID))
				gt.String(t, call.RunID).Equal(logs[0].RunID)
				gt.Bool(t, call.UsageKnown).True()
				total += call.CostNanoUSD
				proc, err := k.GetProcess(ctx, agentkit.ProcessID(call.ProcessID))
				gt.NoError(t, err).Required()
				scope := agentkernel.ScopeFrom(proc.Metadata)
				if proc.Agent == agentkernel.AgentTask {
					gt.String(t, call.ModelRef).Equal("sub")
					gt.Value(t, scope.StageRole).Equal(agentkernel.StageRoleSub)
					gt.Value(t, scope.Budget).Equal(pricing.FromUSD(0.1))
					owners[scope.StageOwner] = true
				}
			}
			gt.Value(t, total).Equal(pricing.NanoUSD(logs[0].CostNanoUSD))
			gt.Bool(t, owners[agentkernel.StageRoleMain]).True()
			gt.Bool(t, owners[agentkernel.StageRoleAdvisor]).True()
		})
	}
}

// executionUsage is a read-only projection of the durable SDK billing events.
type executionUsage struct {
	ProcessID   string          `json:"process_id"`
	RootID      string          `json:"root_id"`
	RunID       string          `json:"run_id"`
	Phase       string          `json:"phase"`
	ModelRef    string          `json:"model_ref"`
	CostNanoUSD pricing.NanoUSD `json:"cost_nano_usd"`
	UsageKnown  bool            `json:"usage_known"`
}

func executionUsageRecords(t *testing.T, k *agentkit.Kernel, rootID agentkit.ProcessID) []executionUsage {
	t.Helper()
	events, err := k.ListEvents(context.Background(), rootID)
	gt.NoError(t, err).Required()
	var records []executionUsage
	for _, event := range events {
		if event.Type != "hecatoncheires.usage.completed" {
			continue
		}
		var record executionUsage
		gt.NoError(t, json.Unmarshal(event.Payload, &record)).Required()
		records = append(records, record)
	}
	return records
}

func TestDurableExecutionCancellationRestoresUsageAfterWorkerStops(t *testing.T) {
	ctx := context.Background()
	j := &model.Job{
		ID: "interactive-cancel", Prompt: "Identify the affected environment",
		Strategy: model.JobStrategyPlanexec, Interactive: true,
		Events:    model.JobEvents{Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}}},
		Execution: &model.JobExecution{Mode: model.JobExecutionModeStandard, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJob}, Models: model.JobExecutionModels{Main: "test", Sub: "test"}},
	}
	repo, c := setupCaseWithSlack(t, "ws", "C-CASE", "thread")
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}, Jobs: []*model.Job{j}})
	llm := scriptedReplyLLM(
		`{"tasks":[{"id":"read","title":"Review case","description":"Read the case","acceptance_criteria":"Identify the environment","tools":["core_job"],"budget_usd":0.01}]}`,
		"The case does not identify the environment.",
		`{"question":{"reason":"The environment is required","items":[{"id":"environment","text":"Which environment?","type":"select","options":["staging","production"]}]}}`,
	)
	models := testModelPolicy(t)
	d := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore(), Models: models}
	runner := job.NewJobRunner(job.RunnerDeps{Repo: repo, Registry: registry, LLMClient: llm, Durable: d, InteractionPoster: &recordingQuestionPoster{}})
	k := bindDurableJobRuntime(t, runner, d, repo, registry, llm)
	key := model.JobRunKey{WorkspaceID: "ws", CaseID: c.ID, JobID: j.ID}
	gt.NoError(t, runner.Run(ctx, j, job.Event{Domain: model.JobEventDomainCase, WorkspaceID: "ws", CaseID: c.ID, Timestamp: time.Now(), CaseLifecycle: model.CaseLifecycleCreated})).Required()
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	served := make(chan error, 1)
	async.DispatchCancelable(serveCtx, func(ctx context.Context) error {
		served <- agentkernel.Serve(ctx, k, agentkit.WithPollInterval(time.Millisecond))
		return nil
	})
	suspended := awaitJobRunLogStage(t, repo, key, model.JobRunStageAwaitingInput)
	stop()
	serveErr := <-served
	gt.Bool(t, serveErr == nil || errors.Is(serveErr, context.Canceled)).True()
	async.Wait()
	pid := agentkit.ProcessID(suspended.AgentProcessID)
	proc, err := k.GetProcess(ctx, pid)
	gt.NoError(t, err).Required()
	gt.Value(t, proc.Status).Equal(agentkit.ProcessWaiting)
	// There are no active claims: the callback must reload persisted billing
	// evidence instead of depending on another request's process-local cache.
	_, known := models.UsageCost(agentkernel.ScopeFrom(proc.Metadata))
	gt.Bool(t, known).False()
	gt.NoError(t, k.Cancel(ctx, pid, "Cancelled by the operator")).Required()
	logs, err := repo.JobRunLog().List(ctx, key, 10)
	gt.NoError(t, err).Required()
	gt.Array(t, logs).Length(1).Required()
	gt.Value(t, logs[0].Stage).Equal(model.JobRunStageFailed)
	gt.Value(t, logs[0].LLMCallCount).Equal(int64(3))
	gt.Value(t, logs[0].CostNanoUSD).Equal(int64(60000))
	gt.Array(t, executionUsageRecords(t, k, pid)).Length(3)
	_, known = models.UsageCost(agentkernel.ScopeFrom(proc.Metadata))
	gt.Bool(t, known).False()
}

func TestDurableExecutionCancellationBeforeAnyClaimKeepsKnownZeroUsage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    model.JobExecutionMode
		cascade bool
	}{
		{name: "simple", mode: model.JobExecutionModeStandard},
		{name: "orchestrator", mode: model.JobExecutionModeOrchestrator},
		{name: "cascade", mode: model.JobExecutionModeOrchestrator, cascade: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo, c := setupCase(t, "ws")
			j := &model.Job{ID: "cancel-pending", Prompt: "Review case evidence", Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}}, Execution: &model.JobExecution{Mode: tc.mode, Cascade: tc.cascade, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJobSimple}, Models: model.JobExecutionModels{Main: "test", Sub: "test", Advisor: "test"}}}
			registry := model.NewWorkspaceRegistry()
			registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}, Jobs: []*model.Job{j}})
			script := &executionScript{}
			llm := script.client()
			d := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore(), Models: testModelPolicy(t)}
			runner := job.NewJobRunner(job.RunnerDeps{Repo: repo, Registry: registry, LLMClient: llm, Durable: d})
			k := bindDurableJobRuntime(t, runner, d, repo, registry, llm)
			gt.NoError(t, runner.Run(ctx, j, job.Event{Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID, Timestamp: time.Now()})).Required()
			busy, err := d.Locator.Busy(ctx, agentkernel.JobRunSubject("ws", c.ID, j.ID))
			gt.NoError(t, err).Required()
			gt.Value(t, busy).NotNil().Required()
			gt.NoError(t, k.Cancel(ctx, busy.ProcessID, "Cancelled before execution")).Required()
			logs, err := repo.JobRunLog().List(ctx, model.JobRunKey{WorkspaceID: "ws", CaseID: c.ID, JobID: j.ID}, 10)
			gt.NoError(t, err).Required()
			gt.Array(t, logs).Length(1).Required()
			gt.Value(t, logs[0].Stage).Equal(model.JobRunStageFailed)
			gt.Value(t, logs[0].CostNanoUSD).Equal(int64(0))
			gt.String(t, logs[0].Error).NotContains("accounting_unknown")
			gt.Value(t, script.calls.Load()).Equal(int32(0))
		})
	}
}
