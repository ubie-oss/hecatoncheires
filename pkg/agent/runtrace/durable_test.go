package runtrace_test

import (
	"context"
	"testing"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/runtrace"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
)

func durableKey() model.JobRunKey {
	return model.JobRunKey{WorkspaceID: "ws-1", CaseID: 7, JobID: "job-mention-1"}
}

// openDurableRun opens a run log the way a host does before spawning, WITHOUT
// keeping the Recorder — which is the situation FinishRun exists for.
func openDurableRun(t *testing.T, repo *memory.Memory, started time.Time) {
	t.Helper()
	_, err := runtrace.Open(context.Background(), openParams(repo, started))
	gt.NoError(t, err).Required()
}

// TestFinishRunClosesALogItDidNotOpen is the property a durable run needs: the
// instance that commits the terminal transition is not necessarily the one that
// opened the log, so the run has to be finishable from the record alone.
func TestFinishRunClosesALogItDidNotOpen(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	ended := started.Add(30 * time.Second)

	openDurableRun(t, repo, started)

	runtrace.FinishRun(ctx, repo, durableKey(), "run-abc", runtrace.Usage{
		InputTokens:              1200,
		OutputTokens:             340,
		CacheCreationInputTokens: 100,
		CacheReadInputTokens:     900,
		LLMCalls:                 3,
		ToolCalls:                5,
		CostNanoUSD:              4_250_000,
		Model:                    "claude-opus-5",
	}, nil, ended)

	log, err := repo.JobRunLog().Get(ctx, durableKey(), "run-abc")
	gt.NoError(t, err).Required()
	gt.Value(t, log.Stage).Equal(model.JobRunStageSuccess)
	gt.Bool(t, log.EndedAt.Equal(ended)).True()
	gt.Value(t, log.InputTokens).Equal(int64(1200))
	gt.Value(t, log.OutputTokens).Equal(int64(340))
	gt.Value(t, log.CacheCreationInputTokens).Equal(int64(100))
	gt.Value(t, log.CacheReadInputTokens).Equal(int64(900))
	gt.Value(t, log.LLMCallCount).Equal(int64(3))
	gt.Value(t, log.ToolCallCount).Equal(int64(5))
	gt.Value(t, log.CostNanoUSD).Equal(int64(4_250_000))
	gt.String(t, log.Model).Equal("claude-opus-5")
	gt.String(t, log.Error).Equal("")

	// The summary doc is what the case agent page lists from, so a finished run
	// that never materialised it would be invisible rather than incomplete.
	run, err := repo.JobRun().Get(ctx, durableKey())
	gt.NoError(t, err).Required()
	gt.Value(t, run.LastStatus).Equal(model.JobRunStatusSuccess)
	gt.Value(t, run.LastRunID).Equal("run-abc")
}

func TestFinishRunRecordsAFailure(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	ended := started.Add(time.Minute)

	openDurableRun(t, repo, started)

	runtrace.FinishRun(ctx, repo, durableKey(), "run-abc", runtrace.Usage{LLMCalls: 1},
		goerr.New("step budget exhausted (64/64)"), ended)

	log, err := repo.JobRunLog().Get(ctx, durableKey(), "run-abc")
	gt.NoError(t, err).Required()
	gt.Value(t, log.Stage).Equal(model.JobRunStageFailed)
	gt.String(t, log.Error).Contains("step budget exhausted")

	run, err := repo.JobRun().Get(ctx, durableKey())
	gt.NoError(t, err).Required()
	gt.Value(t, run.LastStatus).Equal(model.JobRunStatusFailed)
}

// TestFinishRunToleratesAMissingLog pins that the run record is observability:
// a log that cannot be read must not turn into an error the caller has to
// handle, because the caller is a completion handler whose real job is posting
// the agent's answer.
func TestFinishRunToleratesAMissingLog(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()

	runtrace.FinishRun(ctx, repo, durableKey(), "never-opened", runtrace.Usage{}, nil, time.Now().UTC())
	runtrace.FinishRun(ctx, repo, durableKey(), "", runtrace.Usage{}, nil, time.Now().UTC())
	runtrace.FinishRun(ctx, nil, durableKey(), "run-abc", runtrace.Usage{}, nil, time.Now().UTC())
}

// TestFinishRunAccumulatesCostAcrossTurns pins how an interactive run reports
// what it spent: the suspending turn and the resumed turn each fold their own
// figure in, so the record shows the cost of the whole exchange rather than of
// the last turn only.
//
// The model, by contrast, is set rather than added — it is the same model for
// every turn — and a turn that cannot name it must not erase what an earlier turn
// recorded.
func TestFinishRunAccumulatesCostAcrossTurns(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	openDurableRun(t, repo, started)
	runtrace.FinishRun(ctx, repo, durableKey(), "run-abc", runtrace.Usage{
		InputTokens: 1000, CostNanoUSD: 1_000_000, Model: "gemini-3.7-flash",
	}, nil, started.Add(time.Minute))

	// The resumed turn: same run id, its own spend, and no model to report.
	runtrace.FinishRun(ctx, repo, durableKey(), "run-abc", runtrace.Usage{
		InputTokens: 500, CostNanoUSD: 250_000,
	}, nil, started.Add(2*time.Minute))

	log, err := repo.JobRunLog().Get(ctx, durableKey(), "run-abc")
	gt.NoError(t, err).Required()
	gt.Value(t, log.InputTokens).Equal(int64(1500))
	gt.Value(t, log.CostNanoUSD).Equal(int64(1_250_000))
	gt.String(t, log.Model).Equal("gemini-3.7-flash")
}

func TestFinishRunClearsSuspendedInteractionOnCancellation(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	openDurableRun(t, repo, started)
	log, err := repo.JobRunLog().Get(ctx, durableKey(), "run-abc")
	gt.NoError(t, err).Required()
	log.Stage = model.JobRunStageAwaitingInput
	log.AgentProcessID = "process"
	log.AgentAwaitKey = "question"
	log.PendingInteraction = &model.PendingInteraction{
		PostedChannelID: "C-CASE", PostedMessageTS: "question-ts", Reason: "A choice is required",
		Items: []model.PendingInteractionItem{{ID: "environment", Text: "Which environment?", Type: "free_text"}},
	}
	gt.NoError(t, repo.JobRunLog().Finish(ctx, log)).Required()
	runtrace.FinishRun(ctx, repo, durableKey(), "run-abc", runtrace.Usage{LLMCalls: 3, CostNanoUSD: 60000}, goerr.New("operator cancelled"), started.Add(time.Minute))
	finished, err := repo.JobRunLog().Get(ctx, durableKey(), "run-abc")
	gt.NoError(t, err).Required()
	gt.Value(t, finished.Stage).Equal(model.JobRunStageFailed)
	gt.Value(t, finished.PendingInteraction).Nil()
	gt.String(t, finished.AgentProcessID).Equal("")
	gt.String(t, finished.AgentAwaitKey).Equal("")
	gt.Value(t, finished.LLMCallCount).Equal(int64(3))
	gt.Value(t, finished.CostNanoUSD).Equal(int64(60000))
}
