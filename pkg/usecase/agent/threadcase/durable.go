package threadcase

import (
	"context"
	"errors"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"

	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/runtrace"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/planexec"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

// Strategy state versions. Bump one only alongside a DecodeState that still
// reads the older shape — a running deployment always has in-flight Processes on
// it.
const (
	threadMentionVersion = 1
	threadCreateVersion  = 1
)

// Target locates a finished run: the thread it reports into, and the case and
// session it belongs to.
//
// It is rebuilt from the Process metadata rather than captured at spawn, because
// the completion handler runs after the turn — possibly on another instance,
// where the spawning call's variables no longer exist.
type Target struct {
	WorkspaceID string
	// CaseID is 0 for a create turn: the case does not exist yet.
	CaseID    int64
	SessionID string
	// ChannelID / ThreadTS are the run's own thread — the case thread, which the
	// Session is keyed on and where the case's content belongs.
	ChannelID string
	ThreadTS  string
	// UIChannelID / UIThreadTS are the thread the requester is watching. They
	// differ from the pair above only for a case raised by a reaction in another
	// channel; otherwise they hold the same values.
	UIChannelID string
	UIThreadTS  string
	// ProcessID is the run that produced this outcome. A host that records a
	// question needs it so the turn started by the answer can inherit this run's
	// conversation.
	ProcessID string
}

// Host is the Slack-facing surface a finished thread-mode turn needs. Each method
// is called at most once per turn, from the completion handler.
type Host interface {
	// ApplyMention applies a completed mention turn's terminal decision: post the
	// reply, or write the proposed content onto the case and confirm it.
	ApplyMention(ctx context.Context, target Target, decision *Decision) error
	// CreateCase commits a completed create turn's proposal and posts its outcome.
	CreateCase(ctx context.Context, target Target, payload CreatePayload) error
	// AskQuestion posts the planner's question and records it on the session. The
	// turn has ended; the user's answer starts the next one.
	AskQuestion(ctx context.Context, target Target, question QuestionPayload) error
	// ReportFallback tells the user the turn reached no conclusion. reason is the
	// technical cause; the host decides how much of it to show.
	ReportFallback(ctx context.Context, target Target, reason string) error
}

// Durable runs the thread-mode agents on the agentkit runtime: one plan-execute
// agent for a mention on an existing case, and one for materialising a new case
// from the conversation that triggered it.
//
// It coexists with the in-process planexec runner: a deployment that has not
// wired this keeps taking UseCase.RunTurn's synchronous path.
type Durable struct {
	repo     interfaces.Repository
	registry *model.WorkspaceRegistry
	host     Host
	locator  agentkernel.Locator
	// models prices a finished turn for its run record, at the rate of the model
	// the turn generated through.
	models agentkernel.ModelPolicy

	mention agentkit.Agent[planexec.Input]
	create  agentkit.Agent[planexec.Input]
	kernel  *agentkit.Kernel
	// probe filters the planner palette down to the toolset ids that resolve to a
	// tool for this run. nil leaves the palette unfiltered.
	probe *agentkernel.ToolSetProbe
}

// NewDurable builds the durable thread-mode host. The registry is required
// because the create turn validates its proposed field values against the
// workspace schema, and a durable run resolves that schema from its own scope
// rather than from a value captured at spawn.
//
// locator is used only to tell a re-delivered Slack event from a busy thread; a
// nil locator makes every delivery look fresh, which the idempotency key still
// covers.
func NewDurable(repo interfaces.Repository, registry *model.WorkspaceRegistry,
	host Host, locator agentkernel.Locator, models agentkernel.ModelPolicy,
) (*Durable, error) {
	if repo == nil {
		return nil, goerr.New("repository is required")
	}
	if registry == nil {
		return nil, goerr.New("workspace registry is required")
	}
	if host == nil {
		return nil, goerr.New("host is required")
	}
	return &Durable{
		repo: repo, registry: registry, host: host, locator: locator, models: models,
	}, nil
}

// Register registers both thread-mode agents and wires this host as their
// completion handler. Call it before building the Kernel, and Bind after.
func (d *Durable) Register(
	reg *agentkit.Registry, taskAgent agentkit.Agent[react.Input],
	progress planexec.Progress, limiter agentkit.Limiter, store agentkit.HistoryStore,
) error {
	if d == nil {
		return goerr.New("durable threadcase is nil")
	}

	mention, err := planexec.Register(reg, agentkernel.AgentCaseThread, threadMentionVersion,
		taskAgent, progress, limiter,
		planexec.Config[Decision]{Remaining: d.models.RemainingFunc()},
		agentkit.WithHistoryStore[planexec.Output[Decision]](store),
		agentkit.WithOnFinish(d.onMentionFinish),
	)
	if err != nil {
		return goerr.Wrap(err, "register the thread-mode mention agent")
	}
	d.mention = mention

	create, err := planexec.Register(reg, agentkernel.AgentCaseThreadCreate, threadCreateVersion,
		taskAgent, progress, limiter,
		planexec.Config[CreateDecision]{
			// Validation only, no side effect: a rejected proposal is fed back to the
			// model and regenerated, which is the whole point — a bad field value used
			// to kill the turn with no feedback. The case itself is committed by the
			// host AFTER the turn, because a persistence failure is not something the
			// model can repair by re-emitting the same JSON.
			Finalizers: []planexec.Finalizer[CreateDecision]{d.validateAgainstSchema},
			Remaining:  d.models.RemainingFunc(),
		},
		agentkit.WithHistoryStore[planexec.Output[CreateDecision]](store),
		agentkit.WithOnFinish(d.onCreateFinish),
	)
	if err != nil {
		return goerr.Wrap(err, "register the thread-mode create agent")
	}
	d.create = create
	return nil
}

// Bind hands over the Kernel the registered agents run on, and the probe that
// tells this host which toolset ids actually resolve to a tool for a given run.
func (d *Durable) Bind(k *agentkit.Kernel, probe *agentkernel.ToolSetProbe) {
	if d != nil {
		d.kernel = k
		d.probe = probe
	}
}

// ready reports whether a turn can be spawned.
func (d *Durable) ready() bool {
	return d != nil && d.kernel != nil && d.mention.Name() != "" && d.create.Name() != ""
}

// StartTurn spawns one thread-mode turn and returns as soon as the run is
// recorded. Its decision is applied by the completion handler.
func (d *Durable) StartTurn(ctx context.Context, req TurnRequest) (*Result, error) {
	if !d.ready() {
		return nil, goerr.New("durable threadcase is not bound to an agent runtime")
	}
	if err := validateRequest(&req); err != nil {
		return nil, err
	}

	isCreate := req.Mode == ModeCreate
	scope := d.scope(ctx, req, isCreate)
	name := agentkernel.AgentCaseThread
	if isCreate {
		name = agentkernel.AgentCaseThreadCreate
	}
	if err := agentkernel.ValidateSpawn(name, scope); err != nil {
		return nil, goerr.Wrap(err, "validate the thread-mode turn scope")
	}

	// Asking the store first is what tells a re-delivery apart from a busy thread:
	// Spawn resolves an idempotency key silently, returning the existing id without
	// saying that it is existing.
	if d.alreadyStarted(ctx, req) {
		return &Result{Status: StatusIdempotent}, nil
	}

	in, err := d.input(ctx, req, scope, isCreate)
	if err != nil {
		return nil, err
	}
	opts := []agentkit.SpawnOption{
		agentkit.WithSubject(agentkernel.ThreadSubject(req.Session.ID)),
		agentkit.WithIdempotencyKey(agentkernel.TriggerKey(req.ChannelID, req.ThreadTS, req.TriggerTS)),
		agentkit.WithMetadata(scope.Metadata()),
	}
	opts = append(opts, d.inheritOpts(ctx, req.InheritFrom)...)

	if isCreate {
		_, err = d.create.Spawn(ctx, d.kernel, in, opts...)
	} else {
		_, err = d.mention.Spawn(ctx, d.kernel, in, opts...)
	}
	switch {
	case errors.Is(err, agentkit.ErrSubjectBusy):
		return &Result{Status: StatusBusy,
			BusyOwner: d.busySession(ctx, req.ChannelID, req.ThreadTS)}, nil
	case err != nil:
		return nil, goerr.Wrap(err, "spawn the thread-mode agent",
			goerr.V("session_id", req.Session.ID), goerr.V("agent", name))
	}

	// The mention this turn processes is what the next turn's delta scan starts
	// after, so the cursor moves only for a turn that actually started. Stamping a
	// refused turn would hide the mention it names AND every thread reply before it
	// from whichever turn runs next — the delta scan skips everything at or below
	// the cursor — and a busy thread is exactly when a person keeps typing.
	//
	// It is a narrow monotonic update rather than a Session write: by now a worker
	// may already have claimed the Process, and that run's completion handler writes
	// the same row. A full write from here would clobber the outcome it recorded,
	// including a pending question the user is looking at.
	if req.MentionTS != "" {
		if advErr := d.repo.Session().AdvanceLastMention(ctx,
			req.ChannelID, req.ThreadTS, req.MentionTS); advErr != nil {
			errutil.Handle(ctx, goerr.Wrap(advErr, "advance the mention cursor",
				goerr.V("session_id", req.Session.ID)), "advance the mention cursor")
		}
	}

	// The run record is opened only once the run exists, so a refused turn leaves
	// no orphan RUNNING row. A create turn keeps none: it runs before the case
	// exists, and the case agent page lists runs BY case.
	//
	// A very short run can finish before this line, leaving a RUNNING log its
	// completion handler already passed. That row is never LISTED — the case agent
	// page reads the JobRun summary, which is materialised at Finish — so the
	// outcome is the same as a run interrupted before it finished, which the page
	// already accounts for.
	if !isCreate {
		d.openRunLog(ctx, scope, in.SystemPrompt)
	}

	return &Result{Status: StatusStarted}, nil
}

// input builds the planner launch input for one turn.
//
// A mention turn hands its sub-agents the full case-writer set, so edits,
// assignee changes and status transitions are all tool calls inside the loop
// rather than host-applied decisions. A create turn has no case yet, so it stays
// observation-only and materialises the new case from its terminal output.
func (d *Durable) input(ctx context.Context, req TurnRequest, scope agentkernel.Scope, isCreate bool) (planexec.Input, error) {
	palette := agent.KnownToolSetIDsNoCore
	allowWrites := false
	if req.Mode == ModeMention {
		palette = agent.KnownToolSetIDsThreadWrite
		allowWrites = true
	}
	// Only offer the planner what this run's tools actually resolve to. A create
	// turn has no case, so case_write resolves to nothing there; an unconfigured
	// integration resolves to nothing anywhere. Advertising either would hand a
	// task a toolset its sub-agent never receives.
	knownToolIDs, err := d.probe.Available(ctx, scope, palette)
	if err != nil {
		return planexec.Input{}, goerr.Wrap(err, "resolve the thread-mode tool palette",
			goerr.V("session_id", req.Session.ID))
	}
	uiChannel, uiThread := req.UIChannelID, req.UIThreadTS
	if uiChannel == "" {
		uiChannel, uiThread = req.ChannelID, req.ThreadTS
	}
	// The conversation a sub-agent may be asked to read is the turn's own thread,
	// which is the case thread for an existing case and the triggering thread on a
	// create turn (no case exists yet). Without these ids a task holding the Slack
	// read tools has to invent them.
	//
	// One instant serves the whole turn: the planner reads it from the user
	// message and every sub-agent from the task context, so the two cannot
	// disagree about what "today" means inside one turn.
	now := time.Now().UTC()
	taskCtx := agent.TaskContext{
		WorkspaceID:    req.Workspace.Workspace.ID,
		SlackChannelID: req.ChannelID,
		SlackThreadTS:  req.ThreadTS,
		Now:            now,
	}
	if req.Case != nil {
		taskCtx.CaseID = req.Case.ID
	}
	taskContext, tcErr := taskCtx.Render()
	if tcErr != nil {
		return planexec.Input{}, tcErr
	}
	userInput, uiErr := buildUserInput(now, req.SystemMessages, req.DeltaMessages, ConversationMessage{
		Timestamp: req.MentionTS,
		UserID:    req.MentionUserID,
		UserName:  req.MentionUserName,
		Text:      req.MentionText,
	})
	if uiErr != nil {
		return planexec.Input{}, uiErr
	}
	systemPrompt, spErr := buildSystemPrompt(req.Case, req.Workspace, req.Mode, req.CreateInstruction)
	if spErr != nil {
		return planexec.Input{}, spErr
	}
	return planexec.Input{
		SystemPrompt: systemPrompt,
		UserInput:    userInput,
		// Without this the run has no language directive at all: the planner, the
		// terminal output and the direct reply are each left to infer the language
		// from the thread, and a turn whose prompts are English (which they all are)
		// answers a Japanese thread in English.
		LanguageLabel: i18n.LanguageLabel(ctx),
		KnownToolIDs:  knownToolIDs,
		TaskContext:   taskContext,
		// Milestones are drawn where the person who triggered the turn is looking,
		// which for a reaction-raised case is not the case thread. MessageTS is the
		// acknowledgement the host already posted there, so the run updates that
		// message instead of adding a second one.
		Progress: planexec.ProgressTarget{
			ChannelID: uiChannel, ThreadTS: uiThread, MessageTS: req.ProgressMessageTS,
		},
		AllowSubAgentWrites: allowWrites,
		AllowQuestion:       true,
		// A create turn must materialise a case, which the direct fast path
		// deliberately never does.
		AllowDirect: !isCreate,
	}, nil
}

// scope describes the run to the runtime: what it acts on, whose access it acts
// under, and which tools it may build.
func (d *Durable) scope(ctx context.Context, req TurnRequest, isCreate bool) agentkernel.Scope {
	sc := agentkernel.Scope{
		WorkspaceID: req.Workspace.Workspace.ID,
		ChannelID:   req.ChannelID,
		ThreadTS:    req.ThreadTS,
		UIChannelID: req.UIChannelID,
		UIThreadTS:  req.UIThreadTS,
		SessionID:   req.Session.ID,
		// The mentioning user is the access actor for the whole run. Without one the
		// usecase layer reads the run as a system context and BYPASSES private-case
		// access control.
		ActorUserID: req.MentionUserID,
		Lang:        string(i18n.LangFromContext(ctx)),
		ToolSets:    []string{agentkernel.ToolSetsAll},
	}
	if req.Case != nil {
		sc.CaseID = req.Case.ID
		sc.PrivateCase = req.Case.IsPrivate
		// A mention on an existing case may manage that case's Actions. Create
		// and materialize turns keep the thread-mode default of no Action tools.
		sc.ThreadModeActions = req.Mode == ModeMention
	}
	// A create turn keeps no run record; a mention turn gets its own fresh per-turn
	// JobID, which is what keeps it out of the Automated Jobs list while still
	// appearing in the case's run history.
	if !isCreate {
		sc.JobID = uuid.Must(uuid.NewV7()).String()
		sc.JobRunID = uuid.Must(uuid.NewV7()).String()
		sc.EventType = model.EventTypeMention
	}
	return sc
}

// validateAgainstSchema is the create turn's finalizer: it checks the proposed
// fields against the workspace schema the run belongs to, resolved from the run's
// own scope. Registration happens once at startup, so closing over a single
// workspace here would validate every workspace's runs against that one.
func (d *Durable) validateAgainstSchema(_ context.Context, meta map[string]string, out *CreateDecision) error {
	entry, err := d.registry.Get(agentkernel.ScopeFrom(meta).WorkspaceID)
	if err != nil {
		return goerr.Wrap(err, "resolve the workspace for field validation")
	}
	if _, verr := validateCreateDecision(entry, out); verr != nil {
		return verr
	}
	return nil
}

// alreadyStarted reports whether a previous delivery of this same Slack event
// already started a run.
func (d *Durable) alreadyStarted(ctx context.Context, req TurnRequest) bool {
	if d.locator == nil {
		return false
	}
	key := agentkernel.TriggerKey(req.ChannelID, req.ThreadTS, req.TriggerTS)
	pid, err := d.locator.ByTrigger(ctx, key)
	if err != nil {
		// Not knowing means treating it as a fresh delivery: dropping a real mention
		// is worse than the duplicate a wrong guess would cause, and the idempotency
		// key still stops a second run from being created.
		errutil.Handle(ctx, goerr.Wrap(err, "look up the run for this trigger"),
			"look up the run for this trigger")
		return false
	}
	return pid != ""
}

// busySession returns the Session whose turn holds the thread, for the "already
// working on this" message. The Session is what the host renders from, and the
// thread is its key, so no Process lookup is involved.
func (d *Durable) busySession(ctx context.Context, channelID, threadTS string) *model.Session {
	ssn, err := d.repo.Session().GetByThread(ctx, channelID, threadTS)
	if err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "read the session holding this thread",
			goerr.V("channel_id", channelID), goerr.V("thread_ts", threadTS)),
			"read the session holding this thread")
		return nil
	}
	return ssn
}

func (d *Durable) openRunLog(ctx context.Context, sc agentkernel.Scope, systemPrompt string) {
	if _, err := runtrace.Open(ctx, runtrace.OpenParams{
		Repo:         d.repo,
		WorkspaceID:  sc.WorkspaceID,
		CaseID:       sc.CaseID,
		JobID:        sc.JobID,
		RunID:        sc.JobRunID,
		TraceID:      sc.JobRunID,
		EventType:    model.EventTypeMention,
		ExecutorKind: model.ExecutorKindPlanexec,
		SystemPrompt: systemPrompt,
		StartedAt:    time.Now().UTC(),
	}); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "open the thread-mode mention run log",
			goerr.V("session_id", sc.SessionID)), "open the thread-mode mention run log")
	}
}

// onMentionFinish applies a finished mention turn.
func (d *Durable) onMentionFinish(ctx context.Context, pid agentkit.ProcessID,
	res agentkit.FinishResult[planexec.Output[Decision]],
) error {
	sc, target, err := d.finished(ctx, pid)
	if err != nil {
		return err
	}

	var runErr error
	switch {
	case res.Status == agentkit.ProcessSucceeded && res.Output != nil:
		out := *res.Output
		switch out.Kind {
		case planexec.OutputFinal:
			if out.Data == nil {
				runErr = goerr.New("the mention turn finished with no decision")
				d.reportFallback(ctx, target, runErr.Error())
				d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
				break
			}
			if aerr := d.host.ApplyMention(ctx, target, out.Data); aerr != nil {
				runErr = goerr.Wrap(aerr, "apply the mention decision")
				errutil.Handle(ctx, runErr, "apply the mention decision")
			}
			d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
		case planexec.OutputDirect:
			// The direct fast path answered in prose. It is applied as a respond
			// decision, exactly as a parsed one would be.
			if aerr := d.host.ApplyMention(ctx, target,
				&Decision{Kind: DecisionRespond, Message: out.Text}); aerr != nil {
				runErr = goerr.Wrap(aerr, "post the direct mention reply")
				errutil.Handle(ctx, runErr, "post the direct mention reply")
			}
			d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
		case planexec.OutputQuestion:
			d.askQuestion(ctx, target, out.Question)
		default:
			runErr = fallbackError(out.FallbackReason)
			d.reportFallback(ctx, target, runErr.Error())
			d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
		}
	case res.Status == agentkit.ProcessFailed:
		runErr = failureError(res.Failure)
		d.reportFallback(ctx, target, runErr.Error())
		d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
	case res.Status == agentkit.ProcessCancelled:
		runErr = goerr.New("run cancelled")
		// No user-facing message: someone stopped this deliberately. The outcome is
		// still stamped, because "how did the last turn end" must have an answer for
		// every way a turn can end — a thread left unstamped reads as one whose turn
		// never finished.
		d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
	}

	d.finishRunLog(ctx, sc, pid, runErr)
	return nil
}

// onCreateFinish commits a finished create turn's proposal.
func (d *Durable) onCreateFinish(ctx context.Context, pid agentkit.ProcessID,
	res agentkit.FinishResult[planexec.Output[CreateDecision]],
) error {
	_, target, err := d.finished(ctx, pid)
	if err != nil {
		return err
	}

	switch {
	case res.Status == agentkit.ProcessSucceeded && res.Output != nil:
		out := *res.Output
		switch out.Kind {
		case planexec.OutputFinal:
			d.commitCreate(ctx, target, out.Data)
		case planexec.OutputQuestion:
			d.askQuestion(ctx, target, out.Question)
		default:
			// OutputDirect is unreachable here: a create turn disables the direct path
			// because it cannot materialise a case. It is treated as a fallback rather
			// than ignored, so an unexpected shape still tells the user something.
			d.reportFallback(ctx, target, fallbackError(out.FallbackReason).Error())
			d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
		}
	case res.Status == agentkit.ProcessFailed:
		d.reportFallback(ctx, target, failureError(res.Failure).Error())
		d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
	case res.Status == agentkit.ProcessCancelled:
		// Stamped without a message, for the same reason as the mention path.
		d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
	}
	return nil
}

// commitCreate re-validates the accepted proposal and hands it to the host.
//
// The fields are recomputed rather than carried from the finalizer: the finalizer
// ran in an earlier transition, possibly on another instance, and its enriched
// values were never part of the checkpointed output. Recomputing is safe because
// the validation is a pure function of the proposal and the schema.
func (d *Durable) commitCreate(ctx context.Context, target Target, data *CreateDecision) {
	if data == nil {
		d.reportFallback(ctx, target, "the create turn finished with no proposal")
		d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
		return
	}
	entry, err := d.registry.Get(target.WorkspaceID)
	if err != nil {
		d.reportFallback(ctx, target, err.Error())
		d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
		return
	}
	fields, verr := validateCreateDecision(entry, data)
	if verr != nil {
		d.reportFallback(ctx, target, verr.Error())
		d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
		return
	}
	if cerr := d.host.CreateCase(ctx, target, CreatePayload{
		Title:       data.Title,
		Description: data.Description,
		Fields:      fields,
	}); cerr != nil {
		errutil.Handle(ctx, goerr.Wrap(cerr, "create the thread-mode case"),
			"create the thread-mode case")
		d.reportFallback(ctx, target, cerr.Error())
	}
	d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
}

// askQuestion posts the planner's question and records that the session is
// waiting on the answer.
func (d *Durable) askQuestion(ctx context.Context, target Target, q *planexec.Question) {
	if q == nil {
		d.reportFallback(ctx, target, "the turn ended on a question with nothing to ask")
		return
	}
	payload := QuestionPayload{Reason: q.Reason, Items: make([]QuestionItem, len(q.Items))}
	for i, it := range q.Items {
		payload.Items[i] = QuestionItem{
			ID:      it.ID,
			Text:    it.Text,
			Type:    QuestionItemType(it.Type),
			Options: it.Options,
		}
	}
	// The question state is confirmed only once the form is actually posted AND
	// recorded. Stamping it after a failed AskQuestion would leave the thread
	// claiming to be waiting on a form that does not exist: the submit handler reads
	// the missing PendingQuestion as stale and drops the answer, and a plain thread
	// reply resumes a turn with no question to answer. A failure is a turn that
	// reached no conclusion, which is what fallback already means.
	if err := d.host.AskQuestion(ctx, target, payload); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "post the thread-mode question"),
			"post the thread-mode question")
		d.reportFallback(ctx, target, err.Error())
		// Any outcome other than a question, so a later plain reply does not resume a
		// turn that has no question to answer.
		d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithCaseBoundReply)
		return
	}
	d.endSession(ctx, target.ChannelID, target.ThreadTS, model.SessionEndedWithQuestion)
}

// inheritOpts returns the option that continues prevID's conversation in the turn
// about to be spawned, or nothing when there is nothing to continue.
//
// It checks the issuing run first because the Kernel REFUSES a Spawn whose issuer
// committed no conversation. Passing the option blindly would turn "the run that
// asked never got far enough to record anything" into "the answer fails outright",
// which is strictly worse than answering from a fresh conversation.
func (d *Durable) inheritOpts(ctx context.Context, prevID string) []agentkit.SpawnOption {
	if prevID == "" {
		return nil
	}
	proc, err := d.kernel.GetProcess(ctx, agentkit.ProcessID(prevID))
	if err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "read the run whose conversation this turn continues",
			goerr.V("from", prevID)), "read the run whose conversation this turn continues")
		return nil
	}
	if proc == nil || proc.HistoryRef == "" {
		return nil
	}
	return []agentkit.SpawnOption{agentkit.WithInheritedHistory(agentkit.ProcessID(prevID))}
}

func (d *Durable) reportFallback(ctx context.Context, target Target, reason string) {
	if err := d.host.ReportFallback(ctx, target, reason); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "report the thread-mode fallback"),
			"report the thread-mode fallback")
	}
}

// finished reads the scope and target of a run that has just ended.
func (d *Durable) finished(ctx context.Context, pid agentkit.ProcessID) (agentkernel.Scope, Target, error) {
	proc, err := d.kernel.GetProcess(ctx, pid)
	if err != nil {
		return agentkernel.Scope{}, Target{}, goerr.Wrap(err, "read the finished run",
			goerr.V("process", pid))
	}
	sc := agentkernel.ScopeFrom(proc.Metadata)
	uiChannel, uiThread := sc.UITarget()
	return sc, Target{
		WorkspaceID: sc.WorkspaceID,
		CaseID:      sc.CaseID,
		SessionID:   sc.SessionID,
		ChannelID:   sc.ChannelID,
		ThreadTS:    sc.ThreadTS,
		UIChannelID: uiChannel,
		UIThreadTS:  uiThread,
		ProcessID:   string(pid),
	}, nil
}

// endSession stamps how the turn ended. The host reads it to decide whether a
// later event resumes a pending question or starts a fresh turn.
//
// It is a one-field repository call rather than a read-modify-write. This runs
// after the turn, and agentkit released the thread's subject at the terminal
// commit — so by now a later turn may be running and writing the same row. A full
// write from here would restore this turn's stale copy of whatever that turn
// recorded, the mention cursor included.
func (d *Durable) endSession(ctx context.Context, channelID, threadTS string, ended model.SessionEndReason) {
	if channelID == "" || threadTS == "" {
		return
	}
	if err := d.repo.Session().StampLastAction(ctx, channelID, threadTS, ended); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "stamp the session outcome",
			goerr.V("channel_id", channelID), goerr.V("thread_ts", threadTS)),
			"stamp the session outcome")
	}
}

// finishRunLog closes the mention run record. The usage totals come off the
// Process because the run's transitions span claims and possibly instances, so no
// single in-process handler saw them all.
func (d *Durable) finishRunLog(ctx context.Context, sc agentkernel.Scope, pid agentkit.ProcessID, runErr error) {
	if sc.JobRunID == "" || sc.WorkspaceID == "" || sc.CaseID == 0 {
		return
	}
	proc, err := d.kernel.GetProcess(ctx, pid)
	if err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "read the finished run for its usage",
			goerr.V("process", pid)), "read the finished run for its usage")
		return
	}
	m := proc.Metrics
	runtrace.FinishRun(ctx, d.repo,
		model.JobRunKey{WorkspaceID: sc.WorkspaceID, CaseID: sc.CaseID, JobID: sc.JobID},
		sc.JobRunID,
		runtrace.Usage{
			InputTokens:              m.InputTokens,
			OutputTokens:             m.OutputTokens,
			CacheCreationInputTokens: m.CacheCreationInputTokens,
			CacheReadInputTokens:     m.CacheReadInputTokens,
			LLMCalls:                 m.LLMCalls,
			ToolCalls:                m.ToolCalls,
			CostNanoUSD:              int64(d.models.Cost(sc, m)),
			Model:                    d.models.ModelName(sc),
		},
		runErr, time.Now().UTC())
}

// fallbackError turns a fallback reason that crossed the durable boundary as a
// string back into an error, so every non-success path reports through one shape.
func fallbackError(reason string) error {
	if reason == "" {
		return goerr.New("the turn ended without a conclusion")
	}
	return goerr.New(reason)
}

func failureError(f *agentkit.Failure) error {
	if f == nil {
		return goerr.New("the run failed")
	}
	if f.Message == "" {
		return goerr.New("the run failed", goerr.V("code", string(f.Code)))
	}
	return goerr.New(f.Message, goerr.V("code", string(f.Code)))
}
