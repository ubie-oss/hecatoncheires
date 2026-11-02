package job

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/interaction"
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/runtrace"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent"
	agentjob "github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/job"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/planexec"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
)

// Strategy state versions stamped on every Process the Job agents create. Bump
// one only alongside a DecodeState that still reads the older shape — a running
// deployment always has in-flight Processes on the old one.
const (
	jobSimpleAgentVersion   = 1
	jobPlanexecAgentVersion = 1
)

// DurableRuntime is the agent runtime a Job run is Spawned onto, plus the pieces
// a finished run needs to close itself out.
//
// It is a struct rather than a bag of fields on RunnerDeps because all of it is
// required together: a Kernel with no history store cannot reflect, and an agent
// handle with no Kernel cannot spawn.
type DurableRuntime struct {
	// Kernel is the runtime the agents run on. Filled by Bind after the Kernel is
	// built, because registration has to happen first.
	Kernel *agentkit.Kernel
	// History is where each Process's conversation is stored. The reflection pass
	// reads a finished run's transcript from here, by the ref recorded on the
	// Process — a durable run's history is not in the by-run-id store the
	// in-process executor used.
	History agentkit.HistoryStore
	// Locator answers whether a run of this Job is already live. It is what lets a
	// re-trigger be refused BEFORE the run record and the Slack marker exist; a nil
	// Locator degrades to finding out at Spawn, which is late but still correct.
	Locator agentkernel.Locator
	// Models prices a finished run at the rate of the model it generated through,
	// and names that model for the run record. The zero value prices nothing,
	// which is what a deployment with no LLM configured has — such a deployment
	// spawns no durable run at all.
	Models agentkernel.ModelPolicy

	simple      agentkit.Agent[react.Input]
	planexec    agentkit.Agent[planexec.Input]
	cascade     agentkit.Agent[cascadeInput]
	cascadeMain agentkit.Agent[cascadeMainInput]
	runner      *JobRunner
	// probe answers which toolset ids resolve to a tool for a run's scope, so the
	// planner is offered only what its sub-agents will actually receive. Filled by
	// Bind; nil leaves the palette unfiltered.
	probe *agentkernel.ToolSetProbe

	// trackedMu guards tracked. It is written by every spawn and read by Drain.
	trackedMu sync.Mutex
	// tracked accumulates the runs this process started, and is nil unless
	// TrackSpawns turned it on. See TrackSpawns for why serve leaves it off.
	tracked []agentkit.ProcessID
}

// TrackSpawns makes the runtime remember the runs it starts so Drain can wait for
// exactly those.
//
// It is opt-in because only a batch command needs it. `serve` runs indefinitely and
// never waits for a run, so keeping the list would grow it without bound; a
// one-shot sweep, in contrast, must not exit while the runs it dispatched are still
// going, and must not wait for runs some other instance started.
func (d *DurableRuntime) TrackSpawns() {
	if d == nil {
		return
	}
	d.trackedMu.Lock()
	defer d.trackedMu.Unlock()
	if d.tracked == nil {
		d.tracked = []agentkit.ProcessID{}
	}
}

// track records a spawned run when tracking is on.
func (d *DurableRuntime) track(pid agentkit.ProcessID) {
	d.trackedMu.Lock()
	defer d.trackedMu.Unlock()
	if d.tracked == nil {
		return
	}
	d.tracked = append(d.tracked, pid)
}

// tracking reports whether Drain has anything to wait for.
func (d *DurableRuntime) trackedPIDs() []agentkit.ProcessID {
	d.trackedMu.Lock()
	defer d.trackedMu.Unlock()
	return append([]agentkit.ProcessID(nil), d.tracked...)
}

// Drain runs the agent worker in the foreground until every run this process
// started has reached a terminal state.
//
// A one-shot sweep owns this loop: it dispatched the runs, and nothing else is
// guaranteed to be up to execute them. Waiting for exactly its own runs is what
// keeps it from also waiting on a `serve` instance's work, which shares the store.
//
// serveOpts is for tests; production passes none.
func (d *DurableRuntime) Drain(ctx context.Context, serveOpts ...agentkit.ServeOption) error {
	if d == nil || d.Kernel == nil {
		return nil
	}
	pending := make(map[agentkit.ProcessID]bool)
	for _, pid := range d.trackedPIDs() {
		pending[pid] = true
	}
	if len(pending) == 0 {
		return nil
	}
	logger := logging.From(ctx)
	logger.Info("draining job runs", "count", len(pending))

	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	served := make(chan error, 1)
	async.DispatchCancelable(serveCtx, func(c context.Context) error {
		served <- agentkernel.Serve(c, d.Kernel, serveOpts...)
		// Reported through `served`, not returned: the normal exit here is the
		// cancellation Drain performs itself, and returning it would have the async
		// helper report every sweep as a failure.
		return nil
	})

	unfinished := 0
	for len(pending) > 0 {
		for pid := range pending {
			proc, err := d.Kernel.GetProcess(ctx, pid)
			if err != nil {
				// A run whose row cannot be read will never be observed to finish, so
				// stop waiting on it rather than spinning forever.
				errutil.Handle(ctx, goerr.Wrap(err, "read a job run while draining",
					goerr.V("process", pid)), "read a job run while draining")
				delete(pending, pid)
				unfinished++
				continue
			}
			if proc.Status.Terminal() {
				delete(pending, pid)
			}
		}
		if len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			stop()
			if err := <-served; err != nil && !errors.Is(err, context.Canceled) {
				errutil.Handle(ctx, err, "agent worker failed while the sweep was interrupted")
			}
			return goerr.Wrap(ctx.Err(), "the sweep was interrupted while runs were still going",
				goerr.V("unfinished", len(pending)))
		case <-time.After(drainPollInterval):
		}
	}

	stop()
	// Serve returns the context error it stopped on; that is the expected exit
	// here, not a failure of the sweep.
	if err := <-served; err != nil && !errors.Is(err, context.Canceled) {
		return goerr.Wrap(err, "run the agent worker for the sweep")
	}
	logger.Info("job runs finished", "unfinished", unfinished)
	return nil
}

// drainPollInterval is how often Drain re-reads the runs it is waiting on. It
// bounds only the delay between the last run finishing and the command exiting, so
// a coarse value costs nothing.
const drainPollInterval = 200 * time.Millisecond

// alreadyLive reports whether a Process for this (workspace, case, job) is still
// open, so the caller can drop the trigger before producing any outward evidence
// of a run.
//
// It exists because the lease no longer covers the run: Run returns as soon as the
// Process is recorded, so the next tick re-acquires the lease while the first run
// is still going. Without this check that tick creates a run log, posts a
// "starting" marker, and only THEN learns the subject is busy — which the caller
// would record as a failed run.
//
// A read failure is reported as not-live rather than as an error: the Spawn that
// follows is itself the authoritative check, and refusing a legitimate run because
// a lookup blipped is the worse outcome.
func (d *DurableRuntime) alreadyLive(ctx context.Context, key model.JobRunKey) bool {
	if d == nil || d.Locator == nil {
		return false
	}
	busy, err := d.Locator.Busy(ctx,
		agentkernel.JobRunSubject(key.WorkspaceID, key.CaseID, key.JobID))
	if err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "look up the live run of a job",
			goerr.V("job_id", key.JobID), goerr.V("case_id", key.CaseID)),
			"look up the live run of a job")
		return false
	}
	return busy != nil
}

// Register registers the Job agents. Call it before building the Kernel, then
// Bind and AttachRunner afterwards.
//
// The three steps are separate because their dependencies form a cycle if they
// are not: building the Kernel needs the registry registration fills, and the
// JobRunner needs this runtime handed to it at construction. Registration itself
// needs neither, so it goes first and the other two are wired in after.
// taskAgent is the ReAct sub-agent the planexec-strategy Job spawns per planned
// task. It is registered elsewhere and shared, because agentkit keys a Process on
// the agent NAME and a second registration under the same name is an error.
func (d *DurableRuntime) Register(reg *agentkit.Registry, limiter agentkit.Limiter,
	taskAgent agentkit.Agent[react.Input],
) error {
	if d == nil {
		return goerr.New("durable runtime is nil")
	}
	if d.History == nil {
		return goerr.New("history store is required")
	}
	simple, err := react.Register(reg, agentkernel.AgentJobSimple, jobSimpleAgentVersion, limiter,
		agentkit.WithHistoryStore[react.Output](d.History),
		agentkit.WithOnFinish(d.onSimpleFinish),
	)
	if err != nil {
		return goerr.Wrap(err, "register the simple-strategy job agent")
	}
	d.simple = simple

	// A Job's deliverable is a prose summary posted to the case, so the terminal
	// output is text; there is no structured object for a host to apply.
	//
	// No progress: a Job run reports through its operational log in the case
	// thread, not through a milestone message a person is watching.
	handle, err := planexec.Register(reg, agentkernel.AgentJob, jobPlanexecAgentVersion,
		taskAgent, nil, limiter,
		planexec.Config[planexec.TextResult]{
			TextOnly: true, Asker: jobAsker{runtime: d}, Remaining: d.Models.RemainingFunc(),
		},
		agentkit.WithHistoryStore[planexec.Output[planexec.TextResult]](d.History),
		agentkit.WithOnFinish(d.onPlanexecFinish),
	)
	if err != nil {
		return goerr.Wrap(err, "register the planexec-strategy job agent")
	}
	d.planexec = handle
	return d.registerCascade(reg, limiter)
}

// Bind hands over the Kernel the registered agents run on. Until it is called
// agentFor reports no agent, so a Job started before the runtime is ready takes
// the in-process path rather than failing.
// probe filters the planner palette down to the toolset ids that resolve to a
// tool for the run; nil leaves it unfiltered.
func (d *DurableRuntime) Bind(k *agentkit.Kernel, probe *agentkernel.ToolSetProbe) {
	if d != nil {
		d.Kernel = k
		d.probe = probe
	}
}

// AttachRunner hands over the runner a finished run closes itself out through.
// It is separate from Register because the runner is built with this runtime as
// one of its dependencies; nothing can spawn before it exists, so a completion
// handler cannot fire in between.
func (d *DurableRuntime) AttachRunner(r *JobRunner) {
	if d != nil {
		d.runner = r
	}
}

// handles reports whether the durable runtime drives this run. A run it does not
// drive stays on the in-process executor.
//
// An interactive run is driven here too: it parks on an agentkit question await
// and continues when Resume responds to it, which is why the run log records the
// suspended Process and its await key. The simple strategy cannot be interactive
// (Job.Validate enforces it) — a single ReAct loop has no planner to ask from.
func (d *DurableRuntime) handles(strategy model.JobStrategy, interactive bool) bool {
	if d == nil || d.Kernel == nil {
		return false
	}
	switch strategy {
	case model.JobStrategySimple:
		return !interactive && d.simple.Name() != ""
	case model.JobStrategyPlanexec:
		return d.planexec.Name() != ""
	default:
		return false
	}
}

// spawnParams is what starting a durable Job run needs beyond the prompts.
type spawnParams struct {
	// channelID and sessionThreadTS locate the operational-log thread this run
	// already opened with its "starting" marker. They are carried rather than
	// re-derived because that thread belongs to THIS run — for a channel-mode Case
	// the marker rooted a fresh thread, which nothing on the Case records — so a
	// completion handler that reloaded the Case could not find it and would post
	// nowhere. Empty when the marker could not be posted, which is not a reason to
	// refuse the run.
	channelID       string
	sessionThreadTS string

	// taskContext is the subject block every sub-agent of a planexec run is told.
	// It carries the CASE's own channel and thread, which is not the same as the
	// pair above: sessionThreadTS is the operational log's thread, freshly rooted
	// for a channel-mode case, whereas a sub-agent asked to read the case
	// conversation needs the case thread.
	taskContext agent.TaskContext

	job          *model.Job
	event        Event
	key          model.JobRunKey
	runID        string
	systemPrompt string
	userPrompt   string
	// interactive lets a planexec Job ask the user mid-run. The simple strategy
	// ignores it: a single ReAct loop has no planner to ask from.
	interactive bool
	// inheritFrom continues a finished run's conversation in a run of its own. It
	// is how an answered question resumes: a new Process, its own budget, the
	// prior turn's context.
	inheritFrom agentkit.ProcessID
}

// spawn starts one durable Job run and returns as soon as it is recorded.
//
// The subject is the (workspace, case, job) triple, which is what now serialises
// two runs of the same Job on the same Case. The lease the caller holds is
// released when Run returns, so the subject — not the lease — is the durable
// guard from here on.
func (d *DurableRuntime) spawn(ctx context.Context, strategy model.JobStrategy, p spawnParams) (agentkit.ProcessID, error) {
	baseSystemPrompt := p.systemPrompt
	execution := resolvedExecution(p.job, strategy)
	if execution != nil && d.Models.IsZero() {
		return "", goerr.New("job execution policy requires configured models")
	}
	if execution != nil {
		if execution.Models.Main == "" {
			execution.Models.Main = d.Models.DefaultRef()
		}
		if execution.Models.Sub == "" {
			execution.Models.Sub = execution.Models.Main
		}
	}
	if execution != nil && execution.Mode == model.JobExecutionModeOrchestrator {
		strategy = model.JobStrategyPlanexec
		prompt, err := renderExecutionPrompt(orchestratorPrompt, executionPromptInput{SystemPrompt: p.systemPrompt, AllowHandoff: false})
		if err != nil {
			return "", err
		}
		p.systemPrompt = prompt
	}
	name := agentkernel.AgentJobSimple
	if strategy == model.JobStrategyPlanexec {
		name = agentkernel.AgentJob
	}
	if execution != nil && execution.Cascade {
		name = agentkernel.AgentJobCascade
	}
	scope := agentkernel.Scope{
		WorkspaceID: p.key.WorkspaceID,
		CaseID:      p.key.CaseID,
		ToolSets:    []string{agentkernel.ToolSetsAll},
		JobID:       p.key.JobID,
		JobRunID:    p.runID,
		EventType:   string(p.event.Domain),
		// Only the scheduled domain is rate-limited. One tick can make hundreds
		// of (job, case) pairs due at once and a skipped scheduled run costs
		// nothing, whereas a lifecycle event, a manual Run or an interactive
		// resume is a single user-visible action with no such retry.
		SlotGated: p.event.Domain == model.JobEventDomainScheduled,
		// The Job's own model and budget, carried on the run rather than looked
		// up later: the configuration can be replaced while this run is still
		// going, and a run must be judged against what it started under. Empty
		// and zero mean "the deployment's default", which is what a Job that
		// names neither gets.
		LLMModel: p.job.LLMModel,
		Budget:   p.job.Budget,
		// Empty unless the workspace configures a language (Run sets it): an
		// unconfigured Job keeps the deployment default, as it always has.
		Lang:         string(i18n.LangFromContext(ctx)),
		JobExecution: execution,
	}
	if execution != nil {
		scope.StageRole = agentkernel.StageRoleMain
	}
	// Both or neither: the scope rejects a half-set pair, and a marker that could
	// not be posted leaves the run with no thread to report into rather than
	// stopping it.
	if p.channelID != "" && p.sessionThreadTS != "" {
		scope.ChannelID = p.channelID
		scope.ThreadTS = p.sessionThreadTS
	}
	if err := agentkernel.ValidateSpawn(name, scope); err != nil {
		return "", goerr.Wrap(err, "validate the job run scope", goerr.V("job_id", p.key.JobID))
	}

	opts := []agentkit.SpawnOption{
		agentkit.WithSubject(agentkernel.JobRunSubject(p.key.WorkspaceID, p.key.CaseID, p.key.JobID)),
		agentkit.WithMetadata(scope.Metadata()),
		// The run id is what a later resume looks the run's Process up by, so it
		// doubles as the idempotency key. That also makes a re-attempt of the same
		// run resolve to the run it already started.
		agentkit.WithIdempotencyKey(jobRunProcessKey(p.key, p.runID)),
	}
	if p.inheritFrom != "" {
		opts = append(opts, agentkit.WithInheritedHistory(p.inheritFrom))
	}

	var pid agentkit.ProcessID
	var err error
	var plannerInput planexec.Input
	if strategy == model.JobStrategyPlanexec {
		taskContext, tcErr := p.taskContext.Render()
		if tcErr != nil {
			return "", goerr.Wrap(tcErr, "render the sub-agent task context",
				goerr.V("job_id", p.key.JobID), goerr.V("run_id", p.runID))
		}
		// Offer the planner only the toolsets this run's tools actually resolve
		// to. The palette is a fixed list while the tools behind it depend on what
		// the deployment configured and on the case: advertising slack_post to a
		// deployment with no Slack poster is what produced a planner assigning a
		// task a tool its sub-agent never received.
		knownToolIDs, ktErr := d.probe.Available(ctx, scope, agent.KnownToolSetIDsJob)
		if ktErr != nil {
			return "", goerr.Wrap(ktErr, "resolve the job tool palette",
				goerr.V("job_id", p.key.JobID), goerr.V("run_id", p.runID))
		}
		// The language directive is given only when a language was resolved, so a
		// Job in a workspace that configures none keeps its prompt unchanged.
		var languageLabel string
		if scope.Lang != "" {
			languageLabel = i18n.LanguageLabel(ctx)
		}
		plannerInput = planexec.Input{
			SystemPrompt:  p.systemPrompt,
			UserInput:     p.userPrompt,
			KnownToolIDs:  knownToolIDs,
			LanguageLabel: languageLabel,
			// The Job's own system prompt already names the case's channel and
			// thread; its sub-agents get a prompt built from the planner's task text
			// alone, so without this they hold the Slack tools with no id to call
			// them with.
			TaskContext: taskContext,
			// A Job's sub-agents perform the deliverable action (posting the result,
			// filing an action) rather than only observing, so the write tools its
			// palette carries are actually usable.
			AllowSubAgentWrites: true,
			// A trivially-answerable Job skips the investigation machinery and
			// replies in a single tool-enabled pass.
			AllowDirect: execution == nil || execution.Mode != model.JobExecutionModeOrchestrator,
			// Only an interactive Job may stop and ask; an unattended one has nobody
			// to answer. It waits in-band rather than ending the turn, because a Job
			// run is ONE record spanning the whole exchange — the same run id resumes
			// once the user answers.
			AllowQuestion:     p.interactive,
			SuspendOnQuestion: p.interactive,
		}
	}
	if execution != nil && execution.Cascade {
		readOnlyNames, readErr := d.probe.ReadOnlyNames(ctx, scope, agent.KnownToolSetIDsJob)
		if readErr != nil {
			return "", goerr.Wrap(readErr, "resolve cascade read tools")
		}
		mainPlannerInput := plannerInput
		if execution.Mode == model.JobExecutionModeOrchestrator {
			prompt, renderErr := renderExecutionPrompt(orchestratorPrompt, executionPromptInput{SystemPrompt: baseSystemPrompt, AllowHandoff: true})
			if renderErr != nil {
				return "", renderErr
			}
			mainPlannerInput.SystemPrompt = prompt
		}
		pid, err = d.cascade.Spawn(ctx, d.Kernel, cascadeInput{
			Main:     cascadeMainInput{Input: react.Input{SystemPrompt: p.systemPrompt, Prompt: p.userPrompt}, ReadOnlyNames: readOnlyNames},
			Strategy: strategy, Planexec: plannerInput, MainPlanexec: mainPlannerInput, Orchestrator: execution.Mode == model.JobExecutionModeOrchestrator,
		}, opts...)
	} else if strategy == model.JobStrategyPlanexec {
		pid, err = d.planexec.Spawn(ctx, d.Kernel, plannerInput, opts...)
	} else {
		pid, err = d.simple.Spawn(ctx, d.Kernel,
			react.Input{SystemPrompt: p.systemPrompt, Prompt: p.userPrompt}, opts...)
	}
	if err != nil {
		return "", goerr.Wrap(err, "spawn the job agent",
			goerr.V("job_id", p.key.JobID), goerr.V("run_id", p.runID))
	}
	// Only a batch command tracks; see TrackSpawns.
	d.track(pid)
	return pid, nil
}

// caseTaskContext is the subject block a Job run's sub-agents are told. The
// Slack pair comes from the Case, so it is empty for a case that has no Slack
// binding and the sub-agent prompt then omits those lines rather than offering
// an empty id.
//
// now is the run's start time. The Job planner is told the same instant through
// its own system prompt (BuildSystemPrompt, from PromptInputs.Now), so planner
// and sub-agents cannot disagree about what "today" means inside one run.
func caseTaskContext(key model.JobRunKey, c *model.Case, now time.Time) agent.TaskContext {
	out := agent.TaskContext{WorkspaceID: key.WorkspaceID, CaseID: key.CaseID, Now: now}
	if c != nil {
		out.SlackChannelID = c.SlackChannelID
		out.SlackThreadTS = c.SlackThreadTS
	}
	return out
}

// jobRunProcessKey is the idempotency key one Job run's Process is filed under.
// The separator is a NUL so no component can forge a boundary; the value is never
// parsed back.
func jobRunProcessKey(key model.JobRunKey, runID string) string {
	return strings.Join([]string{"job-run", key.WorkspaceID,
		strconv.FormatInt(key.CaseID, 10), key.JobID, runID}, "\x00")
}

// onSimpleFinish closes out a finished simple-strategy run: the run log, the
// Slack completion marker, and the optional reflection pass.
//
// It reloads everything it needs from the Process scope rather than capturing it
// at spawn, because a durable run finishes on whichever instance committed its
// terminal transition — which need not be the one that started it.
func (d *DurableRuntime) onSimpleFinish(ctx context.Context, pid agentkit.ProcessID, res agentkit.FinishResult[react.Output]) error {
	r := d.runner
	if r == nil {
		return goerr.New("durable runtime has no job runner")
	}
	proc, err := d.Kernel.GetProcess(ctx, pid)
	if err != nil {
		return goerr.Wrap(err, "read the finished job run", goerr.V("process", pid))
	}
	if cascadeChildProcess(proc) {
		return nil
	}
	sc := agentkernel.ScopeFrom(proc.Metadata)
	releaseUsage, loadErr := d.loadExecutionUsage(ctx, proc)
	defer releaseUsage()
	key := model.JobRunKey{WorkspaceID: sc.WorkspaceID, CaseID: sc.CaseID, JobID: sc.JobID}

	var runErr error
	switch res.Status {
	case agentkit.ProcessSucceeded:
	case agentkit.ProcessFailed:
		runErr = failureError(res.Failure)
	default:
		runErr = goerr.New("job run did not complete", goerr.V("status", string(proc.Status)))
	}
	runErr = d.executionAccountingError(sc, errors.Join(runErr, loadErr))

	// The run record first: it is what the case agent page lists, so it must be
	// closed even if the Slack marker or the reflection below fails.
	runtrace.FinishRun(ctx, r.deps.Repo, key, sc.JobRunID, d.processUsage(sc, proc), runErr, r.clock())

	j, c := d.reloadRunContext(ctx, sc)
	d.postCompletionMarker(ctx, sc, j, runErr)
	if runErr == nil {
		d.reflect(ctx, proc, sc, j, c)
	}
	return nil
}

// onPlanexecFinish closes out a finished planexec-strategy run.
//
// It has one outcome the simple strategy cannot produce: a question. There the run
// is NOT closed — the form is posted, the log is parked at AWAITING_INPUT, and the
// answer starts a fresh run continuing this one's conversation.
func (d *DurableRuntime) onPlanexecFinish(ctx context.Context, pid agentkit.ProcessID,
	res agentkit.FinishResult[planexec.Output[planexec.TextResult]],
) error {
	r := d.runner
	if r == nil {
		return goerr.New("durable runtime has no job runner")
	}
	proc, err := d.Kernel.GetProcess(ctx, pid)
	if err != nil {
		return goerr.Wrap(err, "read the finished job run", goerr.V("process", pid))
	}
	if cascadeChildProcess(proc) {
		return nil
	}
	sc := agentkernel.ScopeFrom(proc.Metadata)
	releaseUsage, loadErr := d.loadExecutionUsage(ctx, proc)
	defer releaseUsage()
	key := model.JobRunKey{WorkspaceID: sc.WorkspaceID, CaseID: sc.CaseID, JobID: sc.JobID}
	usage := d.processUsage(sc, proc)

	var runErr error
	switch res.Status {
	case agentkit.ProcessSucceeded:
		if res.Output != nil && (res.Output.Kind == planexec.OutputFallback || res.Output.Kind == planexec.OutputHandoff) {
			runErr = goerr.New(fallbackReason(res.Output.FallbackReason))
		}
	case agentkit.ProcessFailed:
		runErr = failureError(res.Failure)
	default:
		runErr = goerr.New("job run did not complete", goerr.V("status", string(proc.Status)))
	}
	runErr = d.executionAccountingError(sc, errors.Join(runErr, loadErr))

	runtrace.FinishRun(ctx, r.deps.Repo, key, sc.JobRunID, usage, runErr, r.clock())

	j, c := d.reloadRunContext(ctx, sc)
	d.postCompletionMarker(ctx, sc, j, runErr)
	if runErr == nil {
		d.reflect(ctx, proc, sc, j, c)
	}
	return nil
}

// jobAsker posts an interactive run's question to the case thread and parks the
// run log on it, recording the Process and await key the answer must reach.
type jobAsker struct {
	runtime *DurableRuntime
}

// Ask posts the question form and suspends the run log.
//
// The Slack thread and the requester are resolved from the case now rather than
// snapshotted at spawn: a run can reach its question long after it started, and
// the case may have been rethreaded in between.
func (a jobAsker) Ask(ctx context.Context, pid agentkit.ProcessID, meta map[string]string,
	key agentkit.AwaitKey, q planexec.Question,
) error {
	d := a.runtime
	r := d.runner
	if r == nil {
		return goerr.New("durable runtime has no job runner")
	}
	sc := agentkernel.ScopeFrom(meta)
	runKey := model.JobRunKey{WorkspaceID: sc.WorkspaceID, CaseID: sc.CaseID, JobID: sc.JobID}

	logRec, err := r.deps.Repo.JobRunLog().Get(ctx, runKey, sc.JobRunID)
	if err != nil {
		return goerr.Wrap(err, "load the run log to park it on a question",
			goerr.V("run_id", sc.JobRunID))
	}
	// The pair is what the resume calls Kernel.Respond with; the Interactor writes
	// it out with the rest of the suspended log.
	logRec.AgentProcessID = string(pid)
	logRec.AgentAwaitKey = string(key)

	_, c := d.reloadRunContext(ctx, sc)
	channelID, threadTS, requester := "", "", ""
	if c != nil {
		channelID, threadTS, requester = c.SlackChannelID, c.SlackThreadTS, c.ReporterID
	}
	interactor := newJobInteractor(r.deps.Repo, r.deps.InteractionPoster, runKey,
		sc.JobRunID, channelID, threadTS, requester, logRec, nil, r.clock)

	items := make([]interaction.Item, len(q.Items))
	for i, it := range q.Items {
		items[i] = interaction.Item{
			ID: it.ID, Text: it.Text,
			Type:    interaction.ItemType(it.Type),
			Options: it.Options,
		}
	}
	if _, err := interactor.Solicit(ctx,
		interaction.Request{Reason: q.Reason, Items: items}); err != nil {
		return goerr.Wrap(err, "solicit the run's question",
			goerr.V("job_id", runKey.JobID), goerr.V("run_id", sc.JobRunID))
	}
	return nil
}

// resumeDurable delivers a human's answer to the suspended run and lets it
// continue.
//
// The run is one Process for its whole life, so this responds to the await it
// parked on rather than starting anything: its budget, its history and its run id
// all carry across the wait. The run log was already transitioned back to RUNNING
// by the caller, which is what makes a crash here leave a RUNNING log rather than
// one stuck at AWAITING_INPUT.
func (r *JobRunner) resumeDurable(ctx context.Context, processID, awaitKey string,
	logRec *model.JobRunLog, pending model.PendingInteraction, answers []interaction.Answer,
) error {
	d := r.deps.Durable
	if d == nil || d.Kernel == nil {
		return goerr.New("the suspended run has no agent runtime to resume on",
			goerr.V("run_id", logRec.RunID))
	}
	if awaitKey == "" {
		return goerr.New("the suspended run names no await to answer",
			goerr.V("run_id", logRec.RunID))
	}
	answer := planexec.RenderAnswers(pendingToQuestion(pending), toQuestionAnswers(answers))
	if err := d.Kernel.Respond(ctx, agentkit.ProcessID(processID),
		agentkit.AwaitKey(awaitKey), []byte(answer)); err != nil {
		return goerr.Wrap(err, "deliver the answer to the suspended run",
			goerr.V("run_id", logRec.RunID), goerr.V("process", processID))
	}
	return nil
}

// pendingToQuestion reconstructs the question that was asked, so each answer can
// be labelled with the item it belongs to.
func pendingToQuestion(p model.PendingInteraction) planexec.Question {
	items := make([]planexec.QuestionItem, len(p.Items))
	for i, it := range p.Items {
		items[i] = planexec.QuestionItem{
			ID: it.ID, Text: it.Text,
			Type:    planexec.QuestionItemType(it.Type),
			Options: it.Options,
		}
	}
	return planexec.Question{Reason: p.Reason, Items: items}
}

// toQuestionAnswers converts the host-neutral answers into the runtime's shape.
func toQuestionAnswers(answers []interaction.Answer) []planexec.QuestionAnswer {
	out := make([]planexec.QuestionAnswer, len(answers))
	for i, a := range answers {
		out[i] = planexec.QuestionAnswer{
			ID:       a.ID,
			Choice:   a.Choice,
			Choices:  a.Choices,
			FreeText: a.FreeText,
		}
	}
	return out
}

// processUsage reads a finished run's totals off its Process, which is the only
// place a run whose transitions span claims and instances accumulates them, and
// prices them at the rate of the model that run generated through.
//
// The cost is computed here rather than at read time because it is the run's own
// fact: the same token counts cost different amounts on different models, and a
// configured price may be corrected after the run is over.
func (d *DurableRuntime) processUsage(sc agentkernel.Scope, proc *agentkit.Process) runtrace.Usage {
	return runtrace.Usage{
		InputTokens:              proc.Metrics.InputTokens,
		OutputTokens:             proc.Metrics.OutputTokens,
		CacheCreationInputTokens: proc.Metrics.CacheCreationInputTokens,
		CacheReadInputTokens:     proc.Metrics.CacheReadInputTokens,
		LLMCalls:                 proc.Metrics.LLMCalls,
		ToolCalls:                proc.Metrics.ToolCalls,
		CostNanoUSD:              int64(d.Models.Cost(sc, proc.Metrics)),
		Model:                    d.Models.ModelName(sc),
	}
}

// fallbackReason renders a fallback that reached no conclusion.
func fallbackReason(reason string) string {
	if reason == "" {
		return "the run reached no conclusion"
	}
	return reason
}

// reloadRunContext fetches the Job definition and the Case a finished run was
// about. Either may come back nil: the workspace could have been reconfigured
// while the run was in flight, and that must not stop the run being closed out.
func (d *DurableRuntime) reloadRunContext(ctx context.Context, sc agentkernel.Scope) (*model.Job, *model.Case) {
	r := d.runner
	entry, err := r.deps.Registry.Get(sc.WorkspaceID)
	if err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "load the workspace of a finished job run",
			goerr.V("workspace_id", sc.WorkspaceID), goerr.T(errutil.TagBenign)),
			"job: load workspace after run")
		return nil, nil
	}
	var j *model.Job
	for _, candidate := range entry.Jobs {
		if candidate != nil && candidate.ID == sc.JobID {
			j = candidate
			break
		}
	}
	c, err := r.deps.Repo.Case().Get(ctx, sc.WorkspaceID, sc.CaseID)
	if err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "load the case of a finished job run",
			goerr.V("case_id", sc.CaseID)), "job: load case after run")
		return j, nil
	}
	return j, c
}

// postCompletionMarker posts the run's terminal operational-log line.
func (d *DurableRuntime) postCompletionMarker(ctx context.Context, sc agentkernel.Scope, j *model.Job, runErr error) {
	if j == nil {
		return
	}
	r := d.runner
	// The marker follows the Job's quiet flag, exactly as the in-process path
	// does; withQuiet is what every post site self-gates on.
	ctx = withQuiet(ctx, j.Quiet)
	// The language the run was spawned with, not the registry's current one: a
	// run started under one configuration is closed out under the same, even
	// when the instance finishing it has a different one loaded.
	ctx = i18n.ContextWithWorkspaceLang(ctx, i18n.Lang(sc.Lang))
	text := i18n.T(ctx, i18n.MsgJobRunCompleted, j.ID)
	if runErr != nil {
		text = i18n.T(ctx, i18n.MsgJobRunFailed, j.ID,
			runtrace.Truncate(runErr.Error(), model.MaxInlineBytes))
	}
	r.postSessionLog(ctx, sc.ChannelID, sc.ThreadTS, text)
}

// reflect runs the optional post-run reflection pass over the run's transcript.
//
// The transcript comes from the Process's own history version, not from the
// by-run-id history repository the in-process executor wrote: a durable run's
// conversation is stored per Process, and the ref naming its final version is on
// the Process row.
func (d *DurableRuntime) reflect(ctx context.Context, proc *agentkit.Process, sc agentkernel.Scope, j *model.Job, c *model.Case) {
	r := d.runner
	if j == nil || !j.Reflection || r.deps.Reflector == nil {
		return
	}
	if c == nil || c.IsPrivate {
		// A private case's contents must not reach shared workspace knowledge.
		return
	}
	if proc.HistoryRef == "" {
		// A run that committed no conversation has nothing to reflect on.
		return
	}
	history, err := d.History.Load(ctx, proc.ID, proc.HistoryRef)
	if err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "load a finished job run's history for reflection",
			goerr.V("process", proc.ID), goerr.V("job_id", sc.JobID)),
			"job: load reflection history")
		return
	}
	if history == nil {
		return
	}
	if err := r.deps.Reflector.Reflect(ctx, agentjob.ReflectRequest{
		WorkspaceID:    sc.WorkspaceID,
		CaseID:         sc.CaseID,
		JobID:          j.ID,
		JobName:        j.Name,
		JobDescription: j.Description,
		History:        history,
	}); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "job reflection",
			goerr.V("job_id", j.ID), goerr.V("case_id", sc.CaseID)), "job: reflection")
	}
}

// failureError turns a recorded Failure into an error the run log can carry.
func failureError(f *agentkit.Failure) error {
	if f == nil {
		return goerr.New("job run failed")
	}
	return goerr.New(f.Message, goerr.V("code", string(f.Code)))
}
