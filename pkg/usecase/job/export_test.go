package job

import (
	"context"
	"log/slog"

	"github.com/gollem-dev/agentkit"
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/planexec"
	"time"

	goslack "github.com/slack-go/slack"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/interaction"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/runtrace"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

// JobInteractorForTest exposes the JobInteractor type for tests.
type JobInteractorForTest = JobInteractor

// NewJobInteractorForTest constructs a JobInteractor for tests.
func NewJobInteractorForTest(
	repo interfaces.Repository,
	poster jobQuestionPoster,
	key model.JobRunKey,
	runID, channelID, threadTS, requesterUserID string,
	runningLog *model.JobRunLog,
	handler *runtrace.Handler,
	now func() time.Time,
) *JobInteractor {
	return newJobInteractor(repo, poster, key, runID, channelID, threadTS, requesterUserID, runningLog, handler, now)
}

// ParseJobQuestionAnswersForTest exposes parseJobQuestionAnswers.
func ParseJobQuestionAnswersForTest(pending *model.PendingInteraction, state *goslack.BlockActionStates) []interaction.Answer {
	return parseJobQuestionAnswers(pending, state)
}

// DecodeJobQuestionRefForTest decodes a Submit-button value and returns the
// resume context fields.
func DecodeJobQuestionRefForTest(value string) (workspaceID string, caseID int64, jobID, runID string, err error) {
	ref, err := decodeJobQuestionRef(value)
	if err != nil {
		return "", 0, "", "", err
	}
	return ref.WorkspaceID, ref.CaseID, ref.JobID, ref.RunID, nil
}

// EncodeJobQuestionRefForTest builds the Submit-button value for a run, the
// way the posted form does.
func EncodeJobQuestionRefForTest(key model.JobRunKey, runID string) (string, error) {
	return jobQuestionRef{WorkspaceID: key.WorkspaceID, CaseID: key.CaseID, JobID: key.JobID, RunID: runID}.encode()
}

// JobQuestionPosterForTest exposes the narrow poster interface so tests can
// supply a fake.
type JobQuestionPosterForTest = jobQuestionPoster

// JobRunTraceHandlerForTest exposes the shared runtrace.Handler under the
// name the runner tests use to assert on the executor's TraceHandler.
type JobRunTraceHandlerForTest = runtrace.Handler

// TruncateRunesForTest exposes truncateRunes for tests in other packages.
var TruncateRunesForTest = truncateRunes

// SlotHoldForTest exposes the acquired-slot handle type. The concurrency gate
// lives entirely inside this package (JobRunner.Run is its only caller), so
// the type and its methods stay unexported in production code.
type SlotHoldForTest = slotHold

// AcquireSlotForTest exposes ConcurrencyLimiter.acquire.
func AcquireSlotForTest(ctx context.Context, l *ConcurrencyLimiter, key model.JobRunKey) (*SlotHoldForTest, error) {
	h, _, err := l.acquire(ctx, key)
	return h, err
}

// AcquireSlotObservedForTest is AcquireSlotForTest plus the admission
// observation the runner reports on its summary line.
func AcquireSlotObservedForTest(ctx context.Context, l *ConcurrencyLimiter, key model.JobRunKey) (hold *SlotHoldForTest, occupied, limit int, err error) {
	h, obs, err := l.acquire(ctx, key)
	return h, obs.Occupied, obs.Limit, err
}

// ReleaseSlotForTest exposes slotHold.release and its hold duration.
func ReleaseSlotForTest(ctx context.Context, h *SlotHoldForTest) time.Duration {
	return h.release(ctx)
}

// SlotHoldIndexForTest reports which execution slot a hold occupies. Tests
// assert on it to prove distinct runs land on distinct slots; production code
// has no reason to know the index.
func SlotHoldIndexForTest(h *SlotHoldForTest) int {
	if h == nil {
		return -1
	}
	return h.index
}

// WithQuietForTest exposes withQuiet for tests in other packages.
var WithQuietForTest = withQuiet

// IsQuietForTest exposes isQuiet for tests in other packages.
var IsQuietForTest = isQuiet

// ValidateEventForTest exposes Event.validate so the per-domain invariants can
// be table-tested directly instead of through eight Publish scenarios.
var ValidateEventForTest = Event.validate

// TickStatsForTest exposes the per-sweep outcome collector. It is unexported in
// production because only the scanner may create one.
type TickStatsForTest = tickStats

// NewTickStatsForTest constructs a collector anchored at startedAt.
func NewTickStatsForTest(startedAt time.Time) *TickStatsForTest {
	return newTickStats(startedAt)
}

// WithTickStatsForTest attaches a collector to ctx, as the scanner does.
func WithTickStatsForTest(ctx context.Context, s *TickStatsForTest) context.Context {
	return withTickStats(ctx, s)
}

// TickStatsFromForTest reads the collector back out of a context.
func TickStatsFromForTest(ctx context.Context) *TickStatsForTest {
	return tickStatsFrom(ctx)
}

// TickStatsAddDueForTest counts one published (job, case) pair.
func TickStatsAddDueForTest(s *TickStatsForTest) { s.addDue() }

// BeginRunForTest / EndRunForTest bracket a dispatched run, as Publish does.
func (s *tickStats) BeginRunForTest() { s.beginRun() }
func (s *tickStats) EndRunForTest()   { s.endRun() }

// TickStatsWaitRunsForTest waits for the dispatched runs, reporting whether all
// of them were accounted for.
func TickStatsWaitRunsForTest(s *TickStatsForTest, timeout time.Duration) bool {
	return s.waitRuns(timeout)
}

// TickStatsRecordOutcomeForTest reports one finished attempt, as JobRunner does
// via its run summary. runID is what marks the attempt as having started.
func TickStatsRecordOutcomeForTest(s *TickStatsForTest, domain, outcome, runID string, slotLimit int, slotHoldMs int64) {
	s.recordRun(&runSummary{
		domain:     domain,
		outcome:    runOutcome(outcome),
		runID:      runID,
		slotGated:  slotLimit > 0,
		slotLimit:  slotLimit,
		slotHoldMs: slotHoldMs,
	})
}

// TickStatsLogAttrsForTest renders the summary attributes.
func TickStatsLogAttrsForTest(s *TickStatsForTest, now time.Time, settled bool) []slog.Attr {
	return s.logAttrs(now, settled)
}

// Run outcome labels, exposed so tests assert against the same constants the
// production code emits instead of re-typing the strings.
const (
	OutcomeCompletedForTest        = string(outcomeCompleted)
	OutcomeFailedForTest           = string(outcomeFailed)
	OutcomeSuspendedForTest        = string(outcomeSuspended)
	OutcomeSkippedLeaseForTest     = string(outcomeSkippedLease)
	OutcomeSkippedSuspendedForTest = string(outcomeSkippedSuspended)
	OutcomeSkippedSlotsFullForTest = string(outcomeSkippedSlotsFull)
	OutcomeSkippedRunningForTest   = string(outcomeSkippedRunning)
)

// CaseTaskContextForTest exposes caseTaskContext, which decides which Slack
// thread a Job's sub-agents are told about. The distinction it encodes — the
// CASE thread, not the run's operational log thread — is easy to get wrong and
// invisible from the outside, so it is pinned by a test.
var CaseTaskContextForTest = caseTaskContext

// CascadeInputForTest exposes the controller's immutable launch data.
type CascadeInputForTest = cascadeInput

// CascadeMainInputForTest exposes the single-loop launch data.
type CascadeMainInputForTest = cascadeMainInput

// CascadeStateForTest exposes controller checkpoints for persistence assertions.
type CascadeStateForTest = cascadeState

// CascadeMainStateForTest exposes the pre-write checkpoint.
type CascadeMainStateForTest = cascadeMainState

// CascadeEscalationToolNameForTest names the host-owned handoff control.
const CascadeEscalationToolNameForTest = cascadeEscalationToolName

// ResolvedExecutionForTest exposes the immutable policy resolution.
var ResolvedExecutionForTest = resolvedExecution

// RegisterCascadeForTest registers the controller without business completion hooks.
func RegisterCascadeForTest(reg *agentkit.Registry, d *DurableRuntime, limiter agentkit.Limiter) (agentkit.Agent[cascadeInput], error) {
	main, err := agentkit.Register(reg, agentkernel.AgentJobCascadeMain, 1, &cascadeMainStrategy{limiter: limiter}, agentkit.WithHistoryStore[cascadeMainOutput](d.History))
	if err != nil {
		return agentkit.Agent[cascadeInput]{}, err
	}
	d.cascadeMain = main
	d.simple, err = react.Register(reg, agentkernel.AgentJobSimple, 1, limiter, agentkit.WithHistoryStore[react.Output](d.History))
	if err != nil {
		return agentkit.Agent[cascadeInput]{}, err
	}
	d.planexec, err = planexec.Register(reg, agentkernel.AgentJob, 1, d.simple, nil, limiter, planexec.Config[planexec.TextResult]{TextOnly: true}, agentkit.WithHistoryStore[planexec.Output[planexec.TextResult]](d.History))
	if err != nil {
		return agentkit.Agent[cascadeInput]{}, err
	}
	return agentkit.Register(reg, agentkernel.AgentJobCascade, 1, &cascadeStrategy{runtime: d, limiter: limiter})
}

// RenderExecutionPromptForTest renders the host execution instruction templates.
func RenderExecutionPromptForTest(system string, orchestrator, handoff bool) (string, error) {
	tmpl := cascadeMainPrompt
	if orchestrator {
		tmpl = orchestratorPrompt
	}
	return renderExecutionPrompt(tmpl, executionPromptInput{SystemPrompt: system, AllowHandoff: handoff})
}
