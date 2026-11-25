package driver

import (
	"context"
	"strings"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/env"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/evaltype"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/scenario"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
)

// JobExecution drives a workspace Job against a seeded case and judges what the
// job produced: its run outcome (success/failure + summary), the case state
// after the run, the actions present, and the tool-call trajectory.
//
// The job goes through the real JobRunner onto the same agent runtime serve and
// tick dispatch to, so a scenario exercises the production strategy (simple /
// planexec), its per-transition checkpointing and budget, source resolution and
// tool wiring — not a second implementation of them.
type JobExecution struct{}

// NewJobExecution builds the driver.
func NewJobExecution() *JobExecution { return &JobExecution{} }

// Kind implements WorkflowDriver.
func (*JobExecution) Kind() string { return scenario.WorkflowJob }

// Run implements WorkflowDriver. The simulator is unused (jobs run unattended,
// without user questions).
func (*JobExecution) Run(ctx context.Context, e *env.Env, sc *scenario.Scenario, _ evaltype.Simulator) (evaltype.Artifact, error) {
	logger := logging.From(ctx)
	if lang, err := i18n.ParseLang(sc.Meta.Language); err == nil && sc.Meta.Language != "" {
		ctx = i18n.ContextWithLang(ctx, lang)
	}
	wsID := e.Entry.Workspace.ID
	if sc.Job == nil {
		return nil, goerr.New("job workflow requires [run_job] with id or ids")
	}

	target, err := resolveTargetCase(e.SeededCases, sc.Job.TargetCase)
	if err != nil {
		return nil, err
	}
	jobIDs := sc.Job.SelectedIDs()
	jobs := make([]*model.Job, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		j, err := resolveJob(e.Entry.Jobs, jobID)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	// Replay Jobs in scenario order against the same evolving case.
	art := &evaltype.JobArtifact{
		JobID:               strings.Join(jobIDs, "+"),
		StartingDescription: target.Description,
	}
	for i, seeded := range e.SeededCases {
		if seeded.ID == target.ID {
			for _, message := range sc.Cases[i].Thread {
				art.StartingThread = append(art.StartingThread, message.User+": "+message.Text)
			}
			break
		}
	}
	eventTime := e.Now()
	for _, jobModel := range jobs {
		ev := job.Event{
			Domain:        model.JobEventDomainCase,
			WorkspaceID:   wsID,
			CaseID:        target.ID,
			Timestamp:     eventTime,
			ActorUserID:   target.ReporterID,
			CaseLifecycle: model.CaseLifecycleCreated,
		}

		logger.Info("eval: running job", "scenario", sc.Meta.ID, "job", jobModel.ID, "case_id", target.ID, "strategy", string(jobModel.Strategy))
		runErr := e.JobRunner.Run(ctx, jobModel, ev)
		async.Wait()

		key := model.JobRunKey{WorkspaceID: wsID, CaseID: target.ID, JobID: jobModel.ID}
		logs, err := e.Repo.JobRunLog().List(ctx, key, 0)
		if err != nil {
			return nil, goerr.Wrap(err, "list job run logs")
		}
		if len(logs) == 0 {
			if runErr != nil {
				return nil, goerr.Wrap(runErr, "job run failed before producing a log")
			}
			return nil, goerr.New("job run produced no log records")
		}
		latest, err := e.AwaitJobRun(ctx, key)
		if err != nil {
			return nil, err
		}
		summary, toolCalls, err := readTimeline(ctx, e.Repo, key, latest.RunID)
		if err != nil {
			return nil, err
		}
		prefix := ""
		if len(jobIDs) > 1 {
			prefix = "[" + jobModel.ID + "] "
		}
		art.Outcome.Stage = joinNonEmpty(art.Outcome.Stage, prefix+string(latest.Stage))
		art.Outcome.Error = joinNonEmpty(art.Outcome.Error, prefixIf(prefix, latest.Error))
		art.Outcome.Summary = joinNonEmpty(art.Outcome.Summary, prefixIf(prefix, summary))
		for _, tc := range toolCalls {
			tc.Seq = len(art.ToolCalls) + 1
			if len(jobs) > 1 {
				tc.Mode = jobModel.ID
			}
			art.ToolCalls = append(art.ToolCalls, tc)
		}
	}

	c, err := e.Repo.Case().Get(ctx, wsID, target.ID)
	if err != nil {
		return nil, goerr.Wrap(err, "get case after job run")
	}
	art.Case = c

	actions, err := listCaseActions(ctx, e.Repo, wsID, target.ID)
	if err != nil {
		return nil, err
	}
	art.Actions = actions

	art.Posts = e.Slack.Posts()

	logger.Info("eval: job done", "scenario", sc.Meta.ID, "job", art.JobID, "stage", art.Outcome.Stage, "actions", len(art.Actions))
	return art, nil
}

func joinNonEmpty(a, b string) string {
	switch {
	case b == "":
		return a
	case a == "":
		return b
	}
	return a + "\n" + b
}

func prefixIf(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

func resolveTargetCase(seeded []*model.Case, title string) (*model.Case, error) {
	if len(seeded) == 0 {
		return nil, goerr.New("job workflow: no seeded case to run the job against")
	}
	if title == "" {
		return seeded[0], nil
	}
	for _, c := range seeded {
		if c.Title == title {
			return c, nil
		}
	}
	return nil, goerr.New("job workflow: target case not found among seeded cases", goerr.V("target_case", title))
}

func resolveJob(jobs []*model.Job, id string) (*model.Job, error) {
	for _, j := range jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return nil, goerr.New("job workflow: job id not found in workspace", goerr.V("job_id", id))
}

// readTimeline extracts the final LLM summary text and the ordered tool calls
// from the job run event timeline.
func readTimeline(ctx context.Context, repo interfaces.Repository, key model.JobRunKey, runID string) (summary string, toolCalls []evaltype.ToolCallRecord, err error) {
	events, err := repo.JobRunEvent().List(ctx, key, runID)
	if err != nil {
		return "", nil, goerr.Wrap(err, "list job run events")
	}
	seq := 0
	for _, ev := range events {
		switch ev.Kind {
		case model.JobRunEventKindLLMResponse:
			if ev.LLMResponse != nil {
				if joined := strings.TrimSpace(strings.Join(ev.LLMResponse.Texts, "\n")); joined != "" {
					summary = joined // keep the latest non-empty response
				}
			}
		case model.JobRunEventKindToolCall:
			if ev.ToolCall != nil {
				seq++
				toolCalls = append(toolCalls, evaltype.ToolCallRecord{
					Seq:  seq,
					Tool: ev.ToolCall.ToolName,
					Args: ev.ToolCall.ArgumentsJSON,
					Mode: "job",
				})
			}
		}
	}
	return summary, toolCalls, nil
}

func listCaseActions(ctx context.Context, repo interfaces.Repository, wsID string, caseID int64) ([]*model.Action, error) {
	all, err := repo.Action().List(ctx, wsID, interfaces.ActionListOptions{})
	if err != nil {
		return nil, goerr.Wrap(err, "list actions")
	}
	out := make([]*model.Action, 0)
	for _, a := range all {
		if a.CaseID == caseID {
			out = append(out, a)
		}
	}
	return out, nil
}
