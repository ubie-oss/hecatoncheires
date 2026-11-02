package runtrace

import (
	"context"
	"time"

	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

// Usage is what a run consumed, as counted by whoever ran it.
//
// A durable agent run needs this because its Recorder does not survive: the run
// finishes on whichever instance committed its terminal transition, which is not
// necessarily the one that opened the log. The runtime meters usage on the
// Process itself, so the caller reads it from there and hands it over.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	LLMCalls                 int64
	ToolCalls                int64
	// CostNanoUSD is what those tokens cost, priced at the rate of the model the
	// run generated through. It is carried rather than derived because the token
	// counts alone cannot be priced later: which model ran is the run's own fact,
	// and a configured price may change afterwards.
	CostNanoUSD int64
	// Model is the provider's own name for the model the run generated through —
	// the value an operator can match against a provider's billing. Empty when
	// the caller could not resolve it.
	Model string
}

// FinishRun ends a run record from a process that does not hold its Recorder.
//
// It is the durable counterpart of Recorder.Finish: it reloads the RUNNING log
// the run opened, folds in the supplied usage, and transitions it — execErr nil
// → SUCCESS, non-nil → FAILED. Reloading rather than carrying the Recorder is
// the whole point: an agent run spans many transitions and may end on another
// instance entirely.
//
// Every persistence failure is non-fatal (errutil.Handle), for the same reason
// Recorder.Finish treats them that way: the run record is observability, and
// losing it must never fail the turn that produced it.
func FinishRun(
	ctx context.Context,
	repo interfaces.Repository,
	key model.JobRunKey,
	runID string,
	usage Usage,
	execErr error,
	endedAt time.Time,
) {
	if repo == nil || runID == "" {
		return
	}

	log, err := repo.JobRunLog().Get(ctx, key, runID)
	if err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "load the run log to finish it",
			goerr.V("workspace_id", key.WorkspaceID),
			goerr.V("case_id", key.CaseID),
			goerr.V("job_id", key.JobID),
			goerr.V("run_id", runID)), "load the run log to finish it")
		return
	}

	// A suspended run can terminate through cancellation without being resumed.
	// Terminal records must not retain a live question or its response target.
	log.PendingInteraction = nil
	log.AgentProcessID = ""
	log.AgentAwaitKey = ""
	log.EndedAt = endedAt
	log.InputTokens += usage.InputTokens
	log.OutputTokens += usage.OutputTokens
	log.CacheCreationInputTokens += usage.CacheCreationInputTokens
	log.CacheReadInputTokens += usage.CacheReadInputTokens
	log.LLMCallCount += usage.LLMCalls
	log.ToolCallCount += usage.ToolCalls
	// The cost accumulates like the token counts, so an interactive run that
	// suspends and resumes reports what the whole exchange spent.
	log.CostNanoUSD += usage.CostNanoUSD
	// The model is set rather than accumulated: it is the same for every turn of
	// a run. An empty value leaves whatever the run recorded before, so a caller
	// that cannot resolve it does not erase what an earlier turn knew.
	if usage.Model != "" {
		log.Model = usage.Model
	}

	status := model.JobRunStatusSuccess
	if execErr != nil {
		status = model.JobRunStatusFailed
		log.Stage = model.JobRunStageFailed
		log.Error = Truncate(execErr.Error(), model.MaxInlineBytes)
	} else {
		log.Stage = model.JobRunStageSuccess
	}

	if err := repo.JobRunLog().Finish(ctx, log); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "finish the run log",
			goerr.V("run_id", runID)), "finish the run log")
	}
	// RecordRun materialises the JobRun summary the case agent page lists from,
	// so it has to run even when the log write failed — otherwise the run is
	// invisible rather than merely incomplete.
	if err := repo.JobRun().RecordRun(ctx, key, status, endedAt, log.RunID, log.TraceID, log.Error); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "record the run summary",
			goerr.V("run_id", runID)), "record the run summary")
	}
}
