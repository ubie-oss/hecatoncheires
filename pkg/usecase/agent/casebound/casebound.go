package casebound

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

// agentVersion is the strategy state version stamped on every Process this agent
// creates. Bump it only alongside a DecodeState that still reads the older
// shape — a running deployment always has in-flight processes on the old one.
const agentVersion = 1

// UseCase starts and finishes case-channel agent turns.
//
// A turn is a durable process: StartTurn builds the prompts and spawns it, then
// returns. The run is driven by the agent worker one checkpointed transition at a
// time, and its answer is posted by onFinish.
type UseCase struct {
	repo    interfaces.Repository
	host    Host
	locator agentkernel.Locator
	// models prices a finished turn for its run record, at the rate of the model
	// the turn generated through. A mention turn names no model of its own, so in
	// practice that is the deployment's default one — but reading it from the
	// policy rather than assuming so keeps the record correct if that changes.
	models agentkernel.ModelPolicy

	// agent and kernel are filled by Register and Bind. They cannot be
	// constructor arguments: registering the agent needs this UseCase as its
	// completion handler, and building the Kernel needs the registry that
	// registration fills, so the three are necessarily wired in that order.
	agent  agentkit.Agent[react.Input]
	kernel *agentkit.Kernel
}

// New builds a casebound UseCase. locator is used only to describe the run
// holding a thread when a turn is refused as busy; a nil locator leaves that
// description empty rather than failing the turn.
func New(repo interfaces.Repository, host Host, locator agentkernel.Locator,
	models agentkernel.ModelPolicy,
) (*UseCase, error) {
	if repo == nil {
		return nil, goerr.New("repository is required")
	}
	if host == nil {
		return nil, goerr.New("host is required")
	}
	return &UseCase{repo: repo, host: host, locator: locator, models: models}, nil
}

// Register registers the case-channel agent and wires this UseCase as its
// completion handler. Call it before building the Kernel, and Bind after.
func (uc *UseCase) Register(reg *agentkit.Registry, limiter agentkit.Limiter, store agentkit.HistoryStore) error {
	if uc == nil {
		return goerr.New("casebound usecase is nil")
	}
	handle, err := react.Register(reg, agentkernel.AgentCaseChannel, agentVersion, limiter,
		agentkit.WithHistoryStore[react.Output](store),
		agentkit.WithOnFinish(uc.onFinish),
	)
	if err != nil {
		return goerr.Wrap(err, "register the case-channel agent")
	}
	uc.agent = handle
	return nil
}

// Bind hands over the Kernel the registered agent runs on.
func (uc *UseCase) Bind(k *agentkit.Kernel) { uc.kernel = k }

// TurnRequest collects what the host resolved before handing control over. The
// runtime never touches the Slack service: the conversation is pre-fetched and
// everything user-facing goes back through Host.
type TurnRequest struct {
	// Session is the persisted Session for this thread. It must already exist:
	// its ID is the turn-lock subject, so the row has to be claimed before a
	// turn can be spawned.
	Session *model.Session

	ChannelID   string
	ThreadTS    string
	MentionTS   string
	MentionText string
	// MentionUserID is the Slack user who mentioned the bot. It becomes the
	// access actor for the whole run, so tools see exactly what that person may
	// see.
	//
	// It is empty when an app — a Slack workflow, an integration bot — posted
	// the mention, because such a post names no Slack user. The run then acts as
	// the case channel itself (model.CaseChannelActorID): it may reach this case
	// and public cases, and no other private case. It never runs with no actor
	// at all: a run with no actor is read by the usecase layer as a system
	// context and would bypass private-case access control entirely.
	MentionUserID string
	BotUserID     string

	Workspace     *model.WorkspaceEntry
	Case          *model.Case
	Actions       []*model.Action
	CurrentAction *model.Action

	// SystemMessages is the conversation snapshot inlined into the system
	// prompt; it is non-empty only for a fresh session. DeltaMessages is what
	// arrived since the previous mention.
	SystemMessages []ConversationMessage
	DeltaMessages  []ConversationMessage

	// TriggerTS is the Slack TS of the event that started this turn. It is the
	// idempotency key, which is what makes a re-delivered Slack event resolve to
	// the run it already started instead of starting a second one.
	TriggerTS string
}

// Status discriminates what StartTurn did.
type Status int

const (
	// StatusStarted means a run was spawned; its answer arrives later.
	StatusStarted Status = iota
	// StatusBusy means another turn holds this thread.
	StatusBusy
	// StatusDuplicate means this trigger already started a run; drop it
	// silently, with no Slack post.
	StatusDuplicate
)

// Result is the outcome of StartTurn.
type Result struct {
	Status Status
	// ProcessID names the run. Set for StatusStarted, and for StatusDuplicate
	// where it names the run the earlier delivery started.
	ProcessID agentkit.ProcessID
	// Busy describes the run holding the thread, when it could still be read.
	Busy *agentkernel.BusyTurn
}

// StartTurn spawns one case-channel turn and returns as soon as the run is
// recorded. The LLM calls, the tool calls and the reply all happen afterwards on
// the agent worker.
func (uc *UseCase) StartTurn(ctx context.Context, req TurnRequest) (*Result, error) {
	if err := uc.ready(); err != nil {
		return nil, err
	}
	if err := validateRequest(&req); err != nil {
		return nil, err
	}

	systemPrompt := buildSystemPrompt(req.Case, req.Workspace, req.ChannelID, req.ThreadTS,
		time.Now().UTC(), req.CurrentAction, req.Actions, req.SystemMessages)
	userInput := buildUserInput(req.DeltaMessages, req.MentionText, req.MentionTS)

	scope := uc.scope(ctx, req)
	if err := agentkernel.ValidateSpawn(agentkernel.AgentCaseChannel, scope); err != nil {
		return nil, goerr.Wrap(err, "validate the case-channel turn scope")
	}

	// The idempotency key is resolved before the subject by agentkit, so a
	// re-delivered Slack event comes back as the run it already started rather
	// than as "busy" — the precedence the previous turn lock applied. Asking the
	// store first is what lets this tell the two apart, because Spawn returns an
	// existing id without saying that it did.
	if existing := uc.existingRun(ctx, req); existing != "" {
		return &Result{Status: StatusDuplicate, ProcessID: existing}, nil
	}

	pid, err := uc.agent.Spawn(ctx, uc.kernel,
		react.Input{SystemPrompt: systemPrompt, Prompt: userInput},
		agentkit.WithSubject(agentkernel.ThreadSubject(req.Session.ID)),
		agentkit.WithIdempotencyKey(agentkernel.TriggerKey(req.ChannelID, req.ThreadTS, req.TriggerTS)),
		agentkit.WithMetadata(scope.Metadata()),
	)
	switch {
	case errors.Is(err, agentkit.ErrSubjectBusy):
		return &Result{Status: StatusBusy, Busy: uc.lookupBusy(ctx, req.Session.ID)}, nil
	case err != nil:
		return nil, goerr.Wrap(err, "spawn the case-channel agent",
			goerr.V("session_id", req.Session.ID))
	}

	// The run record is opened only once the run exists, so a refused turn leaves
	// no RUNNING log behind: such a log never reaches Finish, and a log without a
	// Finish never gets the JobRun summary the case agent page lists from — it
	// would be an invisible orphan row written on every busy mention.
	//
	// It is observability either way: a failure here leaves the turn untraced but
	// does not stop it.
	uc.openRunLog(ctx, scope, systemPrompt)

	// The mention this turn processes is what the next turn's delta scan starts
	// after. It is stamped now rather than at the end because a second mention
	// arriving mid-run must not re-read messages this run already holds.
	uc.stampSession(ctx, req.ChannelID, req.ThreadTS, req.MentionTS)

	return &Result{Status: StatusStarted, ProcessID: pid}, nil
}

func (uc *UseCase) ready() error {
	if uc == nil {
		return goerr.New("casebound usecase is nil")
	}
	if uc.kernel == nil || uc.agent.Name() == "" {
		return goerr.New("casebound usecase is not bound to an agent runtime")
	}
	return nil
}

// scope describes the run to the runtime: what it acts on, whose access it acts
// under, and which tools it may build.
func (uc *UseCase) scope(ctx context.Context, req TurnRequest) agentkernel.Scope {
	return agentkernel.Scope{
		WorkspaceID: req.Workspace.Workspace.ID,
		CaseID:      req.Case.ID,
		ChannelID:   req.ChannelID,
		ThreadTS:    req.ThreadTS,
		SessionID:   req.Session.ID,
		ActorUserID: actorOf(&req),
		Lang:        string(i18n.LangFromContext(ctx)),
		// The channel-mode case agent runs one ReAct loop and is handed its
		// whole palette at once; there is no planner to choose a subset.
		ToolSets:    []string{agentkernel.ToolSetsAll},
		PrivateCase: req.Case.IsPrivate,
		// Each mention turn gets its own JobID because it is not a configured
		// Job. That is what keeps it out of the Automated Jobs list while still
		// appearing in the case's run history.
		JobID:     uuid.Must(uuid.NewV7()).String(),
		JobRunID:  uuid.Must(uuid.NewV7()).String(),
		EventType: model.EventTypeMention,
	}
}

// existingRun returns the run a previous delivery of this same Slack event
// already started, or "" when this is the first delivery.
func (uc *UseCase) existingRun(ctx context.Context, req TurnRequest) agentkit.ProcessID {
	if uc.locator == nil {
		return ""
	}
	key := agentkernel.TriggerKey(req.ChannelID, req.ThreadTS, req.TriggerTS)
	pid, err := uc.locator.ByTrigger(ctx, key)
	if err != nil {
		// Not knowing means treating it as a fresh delivery. Dropping a real
		// mention is worse than the duplicate the wrong guess would cause, and
		// the idempotency key still stops a second run from being created.
		errutil.Handle(ctx, goerr.Wrap(err, "look up the run for this trigger"),
			"look up the run for this trigger")
		return ""
	}
	return pid
}

func (uc *UseCase) openRunLog(ctx context.Context, sc agentkernel.Scope, systemPrompt string) {
	if _, err := runtrace.Open(ctx, runtrace.OpenParams{
		Repo:         uc.repo,
		WorkspaceID:  sc.WorkspaceID,
		CaseID:       sc.CaseID,
		JobID:        sc.JobID,
		RunID:        sc.JobRunID,
		TraceID:      sc.JobRunID,
		EventType:    model.EventTypeMention,
		ExecutorKind: model.ExecutorKindSingleLoop,
		SystemPrompt: systemPrompt,
		StartedAt:    time.Now().UTC(),
	}); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "open the mention run log",
			goerr.V("session_id", sc.SessionID)), "open the mention run log")
	}
}

func (uc *UseCase) lookupBusy(ctx context.Context, sessionID string) *agentkernel.BusyTurn {
	if uc.locator == nil {
		return nil
	}
	busy, err := uc.locator.Busy(ctx, agentkernel.ThreadSubject(sessionID))
	if err != nil {
		errutil.Handle(ctx, err, "read the run holding this thread")
		return nil
	}
	return busy
}

// onFinish posts the answer and closes the run record. agentkit calls it once,
// after the terminal transition committed, on whichever instance committed it.
//
// Delivery is best-effort by design (agentkit ADR-0014): it never fires twice,
// but a crash between the commit and the call loses it. That matches what the
// previous runtime offered — it posted in-process, so the same crash lost the
// reply — and it avoids the alternative, which is posting from inside the
// transition and double-posting on a replay.
func (uc *UseCase) onFinish(ctx context.Context, pid agentkit.ProcessID, res agentkit.FinishResult[react.Output]) error {
	proc, err := uc.kernel.GetProcess(ctx, pid)
	if err != nil {
		return goerr.Wrap(err, "read the finished run", goerr.V("process", pid))
	}
	sc := agentkernel.ScopeFrom(proc.Metadata)

	var runErr error
	switch {
	case res.Status == agentkit.ProcessSucceeded && res.Output != nil:
		if perr := uc.host.Reply(ctx, sc.ChannelID, sc.ThreadTS, res.Output.Text()); perr != nil {
			runErr = goerr.Wrap(perr, "post the agent reply")
			errutil.Handle(ctx, runErr, "post the agent reply")
		}
	case res.Status == agentkit.ProcessFailed:
		runErr = failureError(res.Failure)
		if perr := uc.host.ReportFailure(ctx, sc.ChannelID, sc.ThreadTS, runErr.Error()); perr != nil {
			errutil.Handle(ctx, perr, "report the agent failure")
		}
	case res.Status == agentkit.ProcessCancelled:
		// A cancelled turn was stopped deliberately; whoever cancelled it knows.
		runErr = goerr.New("run cancelled")
	}

	uc.finishRunLog(ctx, sc, proc.Metrics, runErr)
	return nil
}

func failureError(f *agentkit.Failure) error {
	if f == nil {
		return goerr.New("run failed")
	}
	return goerr.New(f.Message, goerr.V("code", string(f.Code)))
}

// finishRunLog closes the run record with the usage agentkit metered on the
// Process. The Process is the authority: a durable run's transitions spread
// across claims and instances, so only the row accumulating them holds the total.
func (uc *UseCase) finishRunLog(ctx context.Context, sc agentkernel.Scope, m agentkit.Metrics, runErr error) {
	if sc.JobRunID == "" || sc.WorkspaceID == "" || sc.CaseID == 0 {
		return
	}
	runtrace.FinishRun(ctx, uc.repo,
		model.JobRunKey{WorkspaceID: sc.WorkspaceID, CaseID: sc.CaseID, JobID: sc.JobID},
		sc.JobRunID,
		runtrace.Usage{
			InputTokens:              m.InputTokens,
			OutputTokens:             m.OutputTokens,
			CacheCreationInputTokens: m.CacheCreationInputTokens,
			CacheReadInputTokens:     m.CacheReadInputTokens,
			LLMCalls:                 m.LLMCalls,
			ToolCalls:                m.ToolCalls,
			CostNanoUSD:              int64(uc.models.Cost(sc, m)),
			Model:                    uc.models.ModelName(sc),
		},
		runErr, time.Now().UTC())
}

// stampSession moves the mention cursor to the mention this turn is processing.
//
// It is deliberately a one-field, monotonic repository call rather than a
// Session.Put. Put replaces the whole document, and this write races the turn it
// has just started: agentkit releases the thread's subject when the terminal
// transition commits — BEFORE the completion handler runs — so by the time this
// line executes a later turn may already have been spawned and stamped its own
// cursor. A full write from here would restore this turn's older LastMentionTS,
// and a stale cursor is not cosmetic: the next turn's delta scan would re-read
// messages an earlier turn already processed, and act on them again.
//
// LastAction is deliberately NOT written. Its only reader is
// Session.ResumeOnReply, and a case-bound Session never reaches it: the
// thread-reply filter returns at the case-bound check first (F7, see
// SlackUseCases.shouldResumeOnReply). Writing it would mean a full-document write
// for a field nothing reads.
func (uc *UseCase) stampSession(ctx context.Context, channelID, threadTS, mentionTS string) {
	if mentionTS == "" {
		return
	}
	if err := uc.repo.Session().AdvanceLastMention(ctx, channelID, threadTS, mentionTS); err != nil {
		errutil.Handle(ctx, err, "persist the session mention position")
	}
}

// validateRequest enforces the minimum invariants StartTurn needs.
func validateRequest(req *TurnRequest) error {
	if req == nil {
		return goerr.New("request is nil")
	}
	if req.Session == nil || req.Session.ID == "" {
		return goerr.New("a persisted Session is required")
	}
	if req.MentionTS == "" {
		return goerr.New("MentionTS is required")
	}
	if req.TriggerTS == "" {
		return goerr.New("TriggerTS is required")
	}
	if req.Case == nil {
		return goerr.New("Case is required")
	}
	if req.Workspace == nil {
		return goerr.New("Workspace is required")
	}
	if actorOf(req) == "" {
		return goerr.New("the turn has no access actor: no mentioning user, and the mention was not posted in the case's own channel",
			goerr.V("channel_id", req.ChannelID),
			goerr.V("case_channel_id", req.Case.SlackChannelID))
	}
	return nil
}

// actorOf returns the access actor the run acts under, or "" when there is
// none it may use.
//
// A mention by a person acts as that person. A mention an app posted names no
// person, and acts as the case channel instead — but only when it was posted in
// the channel this case owns: that actor may read and change the case, and is
// justified only because the reply lands in front of the members who can
// already see it. A thread-mode case does not own its channel (every case of
// the workspace shares it), so it never qualifies.
func actorOf(req *TurnRequest) string {
	if req.MentionUserID != "" {
		return req.MentionUserID
	}
	c := req.Case
	if c == nil || c.SlackChannelID == "" || c.SlackThreadTS != "" || c.SlackChannelID != req.ChannelID {
		return ""
	}
	return model.CaseChannelActorID(c.SlackChannelID)
}

// buildUserInput assembles the user-facing text handed to the agent. Unprocessed
// thread messages are prepended in chronological order with a header so the
// agent can tell them from the new prompt; the current mention is always last.
func buildUserInput(delta []ConversationMessage, mentionText, mentionTS string) string {
	if len(delta) == 0 {
		return mentionText
	}
	var b strings.Builder
	b.WriteString("# Unprocessed thread messages since last mention\n")
	for _, m := range delta {
		// Skip the current mention itself if it appears in the delta.
		if m.Timestamp == mentionTS {
			continue
		}
		name := m.UserName
		if name == "" {
			name = m.UserID
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", m.Timestamp, name, m.Text)
	}
	b.WriteString("\n# Current mention\n")
	b.WriteString(mentionText)
	return b.String()
}
