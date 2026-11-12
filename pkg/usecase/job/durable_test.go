package job_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/m-mizutani/gt"

	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/agentarchive"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
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

// A Job on a thread-mode workspace that enables [case] actions is granted the
// action reads (and only the reads) through its scope. A thread-mode workspace
// without the option, and a channel-mode one — whose Jobs already get the
// Job-safe action set — carry no such grant.
func TestDurableRuntime_SpawnGrantsActionReadsToAThreadModeWorkspaceWithActions(t *testing.T) {
	cases := []struct {
		name      string
		mode      model.CaseMode
		actions   bool
		wantReads bool
	}{
		{name: "thread mode with [case] actions", mode: model.CaseModeThread, actions: true, wantReads: true},
		{name: "thread mode without [case] actions", mode: model.CaseModeThread, actions: false, wantReads: false},
		{name: "channel mode", mode: model.CaseModeChannel, actions: false, wantReads: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo, c := setupCaseWithSlack(t, "ws", "C-CASE", "1700000000.000100")
			j := &model.Job{
				ID:     "reader",
				Prompt: "summarise the case",
				Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
			}
			registry := model.NewWorkspaceRegistry()
			registry.Register(&model.WorkspaceEntry{
				Workspace:   model.Workspace{ID: "ws"},
				CaseMode:    tc.mode,
				CaseActions: tc.actions,
				Jobs:        []*model.Job{j},
			})

			llm := singleReplyLLM("the case looks fine", 120, 34)
			durable := &job.DurableRuntime{History: agentarchive.NewMemoryHistoryStore()}
			runner := job.NewJobRunner(job.RunnerDeps{
				Repo: repo, Registry: registry, LLMClient: llm, Durable: durable,
			})
			k := bindDurableJobRuntime(t, runner, durable, repo, registry, llm)

			gt.NoError(t, runner.Run(ctx, j, job.Event{
				Domain: model.JobEventDomainScheduled, WorkspaceID: "ws", CaseID: c.ID,
				Timestamp: time.Now().UTC(),
			})).Required()

			busy, err := durable.Locator.Busy(ctx, agentkernel.JobRunSubject("ws", c.ID, j.ID))
			gt.NoError(t, err).Required()
			gt.Value(t, busy).NotNil().Required()
			proc, err := k.GetProcess(ctx, busy.ProcessID)
			gt.NoError(t, err).Required()

			sc := agentkernel.ScopeFrom(proc.Metadata)
			gt.Value(t, sc.ThreadModeActionReads).Equal(tc.wantReads)
			// The Job is never given the mention turn's full action grant.
			gt.Bool(t, sc.ThreadModeActions).False()
		})
	}
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
