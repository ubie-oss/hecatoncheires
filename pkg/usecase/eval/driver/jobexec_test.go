package driver_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/driver"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/env"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/evaltype"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/scenario"
)

// actionCreatingJobLLM drives the (single-loop) job to call core__create_action
// once, then return a final summary. The created action is an observable,
// trace-independent side effect we can assert on via the repository.
func actionCreatingJobLLM(actionTitle, summary string) *mock.LLMClientMock {
	// Counted across sessions, not per session: the run is a durable Process whose
	// every transition opens its own gollem session, so a per-session counter would
	// restart at 1 each time and the model would ask for the same tool for as long
	// as the step budget allowed.
	var calls atomic.Int32
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					if calls.Add(1)%2 == 1 {
						return &gollem.Response{FunctionCalls: []*gollem.FunctionCall{{
							ID:        "c1",
							Name:      "core__create_action",
							Arguments: map[string]any{"title": actionTitle},
						}}}, nil
					}
					return &gollem.Response{Texts: []string{summary}}, nil
				},
				HistoryFunc:       func() (*gollem.History, error) { return &gollem.History{Version: gollem.HistoryVersion}, nil },
				AppendHistoryFunc: func(_ *gollem.History) error { return nil },
				CountTokenFunc:    func(_ context.Context, _ ...gollem.Input) (int, error) { return 0, nil },
			}, nil
		},
	}
}

func loadJobScenario(t *testing.T) *scenario.Scenario {
	t.Helper()
	sc, err := scenario.Load(filepath.Join("..", "scenario", "testdata", "job_simple.toml"))
	gt.NoError(t, err)
	return sc
}

func TestJobExecution_RunsJobAndCreatesAction(t *testing.T) {
	sc := loadJobScenario(t)

	e, err := env.Build(context.Background(), sc, env.Options{
		LLM:       actionCreatingJobLLM("Investigate portal 503 login", "Created an action to investigate the 503 login issue."),
		Models:    testModelPolicy(t),
		Completer: fakeCompleter{},
	})
	gt.NoError(t, err)
	gt.V(t, e.JobRunner).NotNil()

	d := driver.NewJobExecution()
	gt.V(t, d.Kind()).Equal("job")

	art, err := d.Run(context.Background(), e, sc, &recordingSim{})
	gt.NoError(t, err)

	ja, ok := art.(*evaltype.JobArtifact)
	gt.B(t, ok).True()
	gt.V(t, ja.JobID).Equal("triage_summary")
	gt.V(t, ja.Outcome.Stage).Equal("SUCCESS")
	gt.V(t, ja.Case).NotNil()
	gt.V(t, ja.Case.Title).Equal("Cannot log in to portal (503)")

	// Observable side effect: the job created an action against the target case.
	gt.A(t, ja.Actions).Length(1)
	gt.V(t, ja.Actions[0].Title).Equal("Investigate portal 503 login")
	gt.V(t, ja.Actions[0].CaseID).Equal(ja.Case.ID)

	// The judge-facing snapshot reflects the outcome and the created action.
	render := ja.Render()
	gt.String(t, render).Contains("Outcome: SUCCESS")
	gt.String(t, render).Contains("Investigate portal 503 login")
}

func TestJobExecution_SeedsEarlierMessagesForJobPrompts(t *testing.T) {
	ctx := t.Context()
	sc := loadJobScenario(t)
	sc.Workspace.SlackMonitorChannel = "C0123456789"
	sc.Cases[0].Thread = []scenario.ThreadMessage{
		{User: "U0123456789", Text: "The mobile portal still works."},
		{User: "U0123456790", Text: "The desktop portal returns HTTP 503."},
	}
	e, err := env.Build(ctx, sc, env.Options{
		LLM:       actionCreatingJobLLM("Investigate", "Action created."),
		Models:    testModelPolicy(t),
		Completer: fakeCompleter{},
	})
	gt.NoError(t, err).Required()
	t.Cleanup(e.Stop)

	messages, _, err := e.Repo.CaseMessage().List(ctx, e.Entry.Workspace.ID, e.SeededCases[0].ID, 10, "")
	gt.NoError(t, err).Required()
	gt.A(t, messages).Length(2).Required()
	gt.V(t, messages[0].Text()).Equal("The desktop portal returns HTTP 503.")
	gt.V(t, messages[1].Text()).Equal("The mobile portal still works.")
}

func TestJobExecution_UnknownJob(t *testing.T) {
	sc := loadJobScenario(t)
	sc.Job.ID = "no-such-job"

	e, err := env.Build(context.Background(), sc, env.Options{
		LLM:       actionCreatingJobLLM("x", "y"),
		Models:    testModelPolicy(t),
		Completer: fakeCompleter{},
	})
	gt.NoError(t, err)

	_, err = driver.NewJobExecution().Run(context.Background(), e, sc, &recordingSim{})
	gt.Error(t, err)
}

func TestJobExecution_RunsSelectedJobsInOrderOnOneCase(t *testing.T) {
	ctx := context.Background()
	sc := loadJobScenario(t)
	followup := *sc.Workspace.Jobs[0]
	followup.ID = "followup"
	sc.Workspace.Jobs = append(sc.Workspace.Jobs, &followup)
	sc.Job.ID = ""
	sc.Job.IDs = []string{"followup", "triage_summary"}
	sc.Meta.Now = "2026-04-11T09:30:00+09:00"

	e, err := env.Build(ctx, sc, env.Options{
		LLM:       actionCreatingJobLLM("Investigate", "Action created."),
		Models:    testModelPolicy(t),
		Completer: fakeCompleter{},
	})
	gt.NoError(t, err).Required()
	t.Cleanup(e.Stop)

	art, err := driver.NewJobExecution().Run(ctx, e, sc, &recordingSim{})
	gt.NoError(t, err).Required()

	ja := art.(*evaltype.JobArtifact)
	gt.V(t, ja.JobID).Equal("followup+triage_summary")
	gt.V(t, ja.Outcome.Stage).Equal("[followup] SUCCESS\n[triage_summary] SUCCESS")
	gt.A(t, ja.Actions).Length(2).Required()
	for _, action := range ja.Actions {
		gt.V(t, action.CaseID).Equal(ja.Case.ID)
	}
	gt.A(t, ja.ToolCalls).Length(2).Required()
	gt.V(t, ja.ToolCalls[0].Mode).Equal("followup")
	gt.V(t, ja.ToolCalls[1].Mode).Equal("triage_summary")
	gt.V(t, ja.ToolCalls[1].Seq).Equal(2)
	gt.V(t, e.Now()).Equal(time.Date(2026, 4, 11, 0, 30, 0, 0, time.UTC))
}
