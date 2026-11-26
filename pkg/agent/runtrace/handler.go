// Package runtrace persists a case-scoped agent run as JobRunLog +
// JobRunEvent records so the case agent page can list it and show its
// per-call timeline.
//
// It is the single home for the gollem trace.Handler that turns LLM / tool
// call boundaries into JobRunEvent rows, shared by the Job runner
// (pkg/usecase/job) and the mention hosts (pkg/usecase/agent/casebound and
// .../threadcase). The Job runner drives its own JobRunLog lifecycle (lease,
// suspend/resume, reflection) and uses only Handler + Sequencer here; the
// mention hosts, which have no such lifecycle, use Recorder for the whole
// open/close.
package runtrace

import (
	"context"
	"encoding/json"
	"maps"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gollem-dev/gollem/trace"
	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
)

const eventPersistenceTimeout = 5 * time.Second

// Routing carries the immutable identifiers stamped on every JobRunEvent
// emitted by a Handler instance. Captured at construction time so individual
// hook calls do not need to thread them through.
type Routing struct {
	WorkspaceID string
	CaseID      int64
	JobID       string
	RunID       string
	TraceID     string
	// ProcessID names the Process this Handler records for. It belongs here
	// rather than on the individual hooks for the same reason the identifiers
	// above do: one Handler serves exactly one claim of one Process, so the value
	// is fixed for the Handler's whole life.
	//
	// Empty for a caller that drives no child Processes (the in-process Job
	// executor), which is the same thing an event written before the field
	// existed carries.
	ProcessID string
}

// handlerSpanKey is the context key under which Handler stashes per-span
// bookkeeping. Using a private struct ensures no cross-package collision with
// gollem's own span context.
type handlerSpanKey struct{}

// handlerSpan is the per-call bookkeeping the handler caches between a Start*
// hook and its matching End* hook.
type handlerSpan struct {
	kind      string // "llm" / "tool"
	startedAt time.Time

	// conv is the conversation an LLM call recorded on this span belongs to,
	// when it is not the claim's own. A tool span carries one because a tool
	// that reaches an LLM holds a conversation of its own; an LLM span inherits
	// its enclosing tool's. Nil means the handler's own conversation.
	conv *conversation

	// Tool-only.
	toolName string
	toolArgs map[string]any
	// parentSeq is the LLM_RESPONSE this tool call carries out, resolved when
	// the execution STARTS. It cannot be read at the end: a tool that reaches an
	// LLM itself (the knowledge tools' embedding calls, webfetch's page
	// analysis) records a response of its own while it runs, and the tool call
	// would then point at an event nested inside itself.
	parentSeq int64
}

// Handler is a gollem trace.Handler that appends one JobRunEvent per LLM call
// (LLM_REQUEST + LLM_RESPONSE) and one per tool execution (TOOL_CALL). It is
// wired once per run via gollem.WithTrace(handler).
//
// Sequence numbers are allocated by the repository inside each write, not by the
// Handler. That is what lets several Handlers — a resumed turn's, another
// instance's claim of the same durable run, and the run owner's own RUN_ERROR —
// append to one timeline without agreeing on a counter first.
type Handler struct {
	eventRepo interfaces.JobRunEventRepository
	routing   Routing
	clock     func() time.Time
	truncator payloadTruncator

	// rootConv is the conversation an LLM call belongs to when neither its span
	// nor its context names one. A Handler is built per claim, and a claim's own
	// agent holds exactly one conversation, so this is that one. It carries its
	// own mutex, so it is not guarded by mu.
	rootConv *conversation

	// State mutated under mu by hook callbacks.
	mu                 sync.Mutex
	phase              string // current phase label; default "execute"
	agentLabel         string // current sub-agent label
	lastLLMResponseSeq int64  // most recent LLM_RESPONSE Sequence (for TOOL_CALL parents)
	totals             runTotals

	// toolByName splits the tool wall-clock per tool. Kept out of runTotals
	// because that struct is returned by value (a map inside it would be
	// shared with the caller); CallStats copies this one explicitly.
	toolByName map[string]ToolCallStats
	// knownTools is the set of tool names the model was actually offered,
	// collected from the LLM requests this handler saw. It is the allow-list
	// for toolByName keys — see addToolStatsLocked.
	knownTools map[string]struct{}
}

// unregisteredToolKey buckets every tool execution whose name was not among
// the tools the model was offered.
const unregisteredToolKey = "unregistered"

// maxToolNameKeys bounds how many distinct keys toolByName may hold, so one
// run can never turn into an unbounded set of log fields.
const maxToolNameKeys = 64

// CallStats is the wall-clock time and call counts one Handler observed inside
// LLM and tool calls. The durations are the SUM of the per-call spans: planexec
// wires one Handler into sub-agents that run concurrently, so the sum can
// exceed the turn's own elapsed time. The counts mirror
// JobRunLog.LLMCallCount / ToolCallCount.
type CallStats struct {
	LLMCalls       int64
	LLMDurationMs  int64
	ToolCalls      int64
	ToolDurationMs int64
	// ToolByName splits ToolCalls / ToolDurationMs per tool. Nil when no tool
	// ran. Keys are restricted to the tool names the model was offered, plus
	// unregisteredToolKey for anything else.
	ToolByName map[string]ToolCallStats
}

// ToolCallStats is one tool's share of CallStats.
type ToolCallStats struct {
	Calls      int64
	DurationMs int64
}

// runTotals is the running tally across everything a Handler saw: tokens and
// call counts. planexec wires the SAME handler into the planner and all
// parallel sub-agents, so one Handler's totals cover the whole turn —
// including sub-agent calls that happen concurrently, which is why the
// counters are guarded by the handler mutex.
type runTotals struct {
	InputTokens  int64
	OutputTokens int64
	// CacheCreationInputTokens / CacheReadInputTokens are the prompt-cache
	// share of InputTokens (which already includes them), tallied so a run's
	// cache hit rate is readable off the log without scanning the timeline.
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	// LLMCalls is the number of LLM calls counted. Zero distinguishes "never
	// reached the model" from "the provider reported no token usage".
	LLMCalls int64
	// ToolCalls is the number of tool executions counted. Together with
	// LLMCalls it gives the turn's step count.
	ToolCalls int64
	// LLMDurationMs / ToolDurationMs are the summed wall-clock of the calls
	// counted above, measured around each call by the Start*/End* hook pair.
	// They are reported on the run's summary log line only — neither is
	// persisted on JobRunLog.
	LLMDurationMs  int64
	ToolDurationMs int64
}

var _ trace.Handler = (*Handler)(nil)

// NewHandler constructs a Handler bound to the given run. clock defaults to
// time.Now().UTC() when nil.
func NewHandler(
	eventRepo interfaces.JobRunEventRepository,
	routing Routing,
	clock func() time.Time,
) *Handler {
	if eventRepo == nil {
		panic("runtrace.Handler: eventRepo is nil")
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Handler{
		eventRepo: eventRepo,
		routing:   routing,
		clock:     clock,
		truncator: defaultPayloadTruncator{},
		phase:     phaseExecute,
		rootConv:  newConversation(),
	}
}

// phaseExecute is the default phase label carried by every event until the
// owner transitions the handler to another phase.
const phaseExecute = "execute"

// StartAgentExecute is a no-op: run lifecycle is tracked by JobRunLog, not the
// per-event timeline.
func (h *Handler) StartAgentExecute(ctx context.Context) context.Context {
	return ctx
}

// EndAgentExecute is a no-op for the same reason.
func (h *Handler) EndAgentExecute(ctx context.Context, err error) {}

// StartLLMCall records the start time in a context-scoped span. No event is
// appended yet; we have no LLMCallData until End.
//
// A call opened INSIDE a tool execution — a knowledge tool's embedding,
// webfetch's page analysis — inherits that tool's conversation, so its request
// is recorded as a diff against the tool's own message list rather than against
// the agent's. The link is made from the enclosing span rather than from a
// context value the caller could add, because the context a handler is handed
// at End* is the one it returned at Start* (trace.Multi records them per
// handler), so a value added by the runtime after the claim opened never
// arrives here.
func (h *Handler) StartLLMCall(ctx context.Context) context.Context {
	span := &handlerSpan{kind: "llm", startedAt: h.clock()}
	if parent := spanFromContext(ctx); parent != nil {
		span.conv = parent.conv
	}
	return context.WithValue(ctx, handlerSpanKey{}, span)
}

// EndLLMCall appends two events: LLM_REQUEST built from data.Request +
// data.Model, then LLM_RESPONSE built from data.Response + tokens + elapsed
// wall-clock. If err is non-nil the response event still captures whatever
// partial data was returned; the surrounding run failure is recorded
// separately via EmitRunError.
//
// BOTH events are stamped with the call's COMPLETION time: OccurredAt is not
// the append time, but it is not the call's start either. The measured call
// latency lives in LLMResponsePayload.DurationMs — never derive it from the
// gap between the two events, which only reflects how long the two appends
// took. See the JobRunEvent.OccurredAt godoc.
func (h *Handler) EndLLMCall(ctx context.Context, data *trace.LLMCallData, err error) {
	span := spanFromContext(ctx)
	endedAt := h.clock()
	var durationMs int64
	if span != nil && !span.startedAt.IsZero() {
		durationMs = max(endedAt.Sub(span.startedAt).Milliseconds(), 0)
	}

	// Count the attempt before inspecting data. A provider that could not open
	// the stream calls this with a nil LLMCallData (gollem's openai client does
	// exactly that when CreateChatCompletionStream fails), and reaching the model
	// is still a step the run took — the same reason EndToolExec counts a failed
	// tool execution. The duration is folded in here too: a call that failed
	// after waiting on the provider still spent that wall-clock.
	h.mu.Lock()
	h.totals.LLMCalls++
	h.totals.LLMDurationMs += durationMs
	h.mu.Unlock()

	if data == nil {
		// No call data means no request / response body to record, and we skip
		// the events rather than fabricate them. The attempt is already counted,
		// so LLMCallCount can exceed the number of LLM_RESPONSE events.
		return
	}

	// Remember which tools the model was actually offered. addToolStatsLocked
	// uses this as the allow-list for per-tool keys.
	if data.Request != nil && len(data.Request.Tools) > 0 {
		h.mu.Lock()
		if h.knownTools == nil {
			h.knownTools = make(map[string]struct{}, len(data.Request.Tools))
		}
		for _, ts := range data.Request.Tools {
			h.knownTools[ts.Name] = struct{}{}
		}
		h.mu.Unlock()
	}

	// LLM_REQUEST. Only what this conversation has not recorded yet is written;
	// see conversation.apply and model.LLMRequestPayload.MessagesPrefixLen.
	reqEv := h.baseEvent(model.JobRunEventKindLLMRequest, endedAt)
	reqEv.LLMRequest = h.truncator.LLMRequestFromTrace(data)
	h.conversationFor(ctx, span).apply(reqEv.LLMRequest)
	h.append(ctx, reqEv)

	// LLM_RESPONSE
	respEv := h.baseEvent(model.JobRunEventKindLLMResponse, endedAt)
	respEv.LLMResponse = h.truncator.LLMResponseFromTrace(data, durationMs)
	h.append(ctx, respEv)

	// Track the response Sequence so subsequent TOOL_CALL events can point
	// ParentSequence at it, and fold this call's tokens into the run total.
	h.mu.Lock()
	h.lastLLMResponseSeq = respEv.Sequence
	h.totals.InputTokens += respEv.LLMResponse.InputTokens
	h.totals.OutputTokens += respEv.LLMResponse.OutputTokens
	h.totals.CacheCreationInputTokens += respEv.LLMResponse.CacheCreationInputTokens
	h.totals.CacheReadInputTokens += respEv.LLMResponse.CacheReadInputTokens
	h.mu.Unlock()
}

// conversationFor resolves which conversation an LLM call belongs to: the one
// carried on its span (a call nested inside a tool), then one scoped to the
// context (a sub-agent), and otherwise the claim's own.
func (h *Handler) conversationFor(ctx context.Context, span *handlerSpan) *conversation {
	if span != nil && span.conv != nil {
		return span.conv
	}
	if c := conversationFrom(ctx); c != nil {
		return c
	}
	return h.rootConv
}

// runTotals returns the tally counted so far by THIS handler. It is a
// per-handler delta, not the run's lifetime total: a resumed run builds a
// fresh handler, so the caller adds this to whatever the persisted log
// already carries (see AddRunTotals).
func (h *Handler) runTotals() runTotals {
	if h == nil {
		return runTotals{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.totals
}

// AddRunTotals folds the handler's counted tokens and call counts into the run
// log. Callers invoke it once per handler, immediately before persisting the
// log (Suspend / Finish), so a suspended turn's totals survive into the resumed
// turn's. Both arguments may be nil (the resume prepare-failure path has no
// handler), in which case the log is left untouched.
func AddRunTotals(log *model.JobRunLog, h *Handler) {
	if log == nil || h == nil {
		return
	}
	t := h.runTotals()
	log.InputTokens += t.InputTokens
	log.OutputTokens += t.OutputTokens
	log.CacheCreationInputTokens += t.CacheCreationInputTokens
	log.CacheReadInputTokens += t.CacheReadInputTokens
	log.LLMCallCount += t.LLMCalls
	log.ToolCallCount += t.ToolCalls
}

// StartToolExec caches the tool name + args + start timestamp on a
// context-scoped span, together with the LLM_RESPONSE the call answers. No event
// is appended yet; we need the result before we can emit a complete TOOL_CALL.
//
// The parent is resolved HERE rather than at the end. A tool that reaches an LLM
// itself records an LLM_RESPONSE of its own while it runs, and the last response
// seen by then is that one — so an end-time lookup would point the tool call at
// an event nested inside it.
func (h *Handler) StartToolExec(ctx context.Context, toolName string, args map[string]any) context.Context {
	h.mu.Lock()
	parentSeq := h.parentSequenceLocked(ctx)
	h.mu.Unlock()

	return context.WithValue(ctx, handlerSpanKey{}, &handlerSpan{
		kind:      "tool",
		startedAt: h.clock(),
		toolName:  toolName,
		toolArgs:  args,
		parentSeq: parentSeq,
		// A tool that reaches an LLM itself holds a conversation of its own.
		// Allocated for every tool execution rather than on demand, so the LLM
		// calls nested in it need no synchronisation to find it.
		conv: newConversation(),
	})
}

// EndToolExec appends a single TOOL_CALL event with ParentSequence pointing at
// the LLM_RESPONSE that asked for the call. The event's OccurredAt is the
// execution's completion time; its measured duration is the StartedAt/EndedAt
// pair inside ToolCallPayload.
func (h *Handler) EndToolExec(ctx context.Context, result map[string]any, err error) {
	span := spanFromContext(ctx)
	endedAt := h.clock()

	var startedAt time.Time
	var toolName string
	var args map[string]any
	var parentSeq int64
	if span != nil && span.kind == "tool" {
		startedAt = span.startedAt
		toolName = span.toolName
		args = span.toolArgs
		parentSeq = span.parentSeq
	} else {
		// No span to read: the Start hook never ran, or something replaced the
		// span in the context. Resolve the parent now — late is better than
		// unset, which JobRunEvent.Validate rejects outright.
		h.mu.Lock()
		parentSeq = h.parentSequenceLocked(ctx)
		h.mu.Unlock()
	}
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	durationMs := max(endedAt.Sub(startedAt).Milliseconds(), 0)

	ev := h.baseEvent(model.JobRunEventKindToolCall, endedAt)
	h.mu.Lock()
	ev.ParentSequence = parentSeq
	// Counted here rather than at Start so a tool that never returned (the
	// process died mid-execution) is not billed as a completed step.
	h.totals.ToolCalls++
	h.totals.ToolDurationMs += durationMs
	h.addToolStatsLocked(toolName, durationMs)
	h.mu.Unlock()

	ev.ToolCall = h.truncator.ToolCallFromTrace(toolName, args, result, err, startedAt, endedAt)
	h.append(ctx, ev)
}

// parentSequenceLocked returns the Sequence of the LLM_RESPONSE this tool call
// carries out. The caller MUST hold h.mu.
//
// A durable run's LLM call and the tool calls it asked for are separate
// checkpointed transitions, so a claim can start between them — and this Handler,
// built for that claim, never saw the response. Reading it back from the run is
// what keeps the parent link intact across that boundary; without it the event
// would carry ParentSequence 0, which JobRunEvent.Validate rejects, and the tool
// call would be dropped from the timeline instead of merely being unlinked.
//
// The lookup happens at most once per claim, because the answer is then cached in
// lastLLMResponseSeq and every later response overwrites it locally.
//
// Under a gollem mock LLM it returns 0 and the TOOL_CALL append is then rejected
// by JobRunEvent.Validate, which shows up in test output as a non-fatal
// "ToolCall must reference a parent LLMResponse" report. That is a property of the
// mock, not a defect: gollem emits its LLM-call trace hooks from the provider
// client packages only, so no LLM_RESPONSE is ever recorded for a mocked call and
// there is nothing for the tool call to point at. It cannot happen against a real
// provider.
func (h *Handler) parentSequenceLocked(ctx context.Context) int64 {
	if h.lastLLMResponseSeq != 0 {
		return h.lastLLMResponseSeq
	}
	seq, err := h.eventRepo.LatestLLMResponseSequence(ctx,
		model.JobRunKey{
			WorkspaceID: h.routing.WorkspaceID,
			CaseID:      h.routing.CaseID,
			JobID:       h.routing.JobID,
		}, h.routing.RunID)
	if err != nil {
		// Non-fatal, like every other trace failure: report it and leave the
		// parent unset. The append that follows will be rejected and reported in
		// turn, which is the honest outcome — a tool call whose parent cannot be
		// established is not something to invent a link for.
		errutil.Handle(ctx, goerr.Wrap(err, "look up the parent llm response",
			goerr.V("run_id", h.routing.RunID)), "runtrace: resolve tool call parent")
		return 0
	}
	h.lastLLMResponseSeq = seq
	return seq
}

// EnsureExternalTurn records a placeholder LLM call when the run has none yet.
//
// A run whose loop is driven by an external harness (pkg/agent/harness) makes
// its model calls outside this process, so no LLM_RESPONSE is ever recorded —
// and a TOOL_CALL with no response to point at is rejected by
// JobRunEvent.Validate, which would drop every tool call of such a run from its
// timeline. One placeholder per run gives them a parent. It says what it is in
// its text, and carries no tokens: nothing here is counted as spend.
func (h *Handler) EnsureExternalTurn(ctx context.Context, note string) {
	h.mu.Lock()
	seq := h.parentSequenceLocked(ctx)
	h.mu.Unlock()
	if seq != 0 {
		return
	}
	spanCtx := h.StartLLMCall(ctx)
	h.EndLLMCall(spanCtx, &trace.LLMCallData{
		Model:    "external-harness",
		Request:  &trace.LLMRequest{},
		Response: &trace.LLMResponse{Texts: []string{note}},
	}, nil)
}

// addToolStatsLocked folds one execution into the per-tool tally. The caller
// holds h.mu.
//
// The key is checked against the tool names the model was actually offered:
// toolName reaches us from the provider's function call, so an invented name
// would otherwise become a log field name and an unbounded source of
// cardinality. Anything unrecognised — including an empty name from a lost
// span, and any name past maxToolNameKeys — goes into one bucket rather than
// being renamed into something it is not. The raw name is still recorded
// verbatim on the TOOL_CALL event.
func (h *Handler) addToolStatsLocked(toolName string, durationMs int64) {
	key := unregisteredToolKey
	if _, ok := h.knownTools[toolName]; ok {
		key = toolName
	}
	if h.toolByName == nil {
		h.toolByName = make(map[string]ToolCallStats, 8)
	}
	if _, exists := h.toolByName[key]; !exists && len(h.toolByName) >= maxToolNameKeys {
		key = unregisteredToolKey
	}
	stats := h.toolByName[key]
	stats.Calls++
	stats.DurationMs += durationMs
	h.toolByName[key] = stats
}

// CallStats returns what this handler has observed so far. Like runTotals it is
// a per-handler tally, not the run's lifetime total: a resumed run builds a
// fresh handler. The returned map is a copy, so the caller may keep it without
// sharing state with the handler. Safe on a nil receiver.
func (h *Handler) CallStats() CallStats {
	if h == nil {
		return CallStats{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := CallStats{
		LLMCalls:       h.totals.LLMCalls,
		LLMDurationMs:  h.totals.LLMDurationMs,
		ToolCalls:      h.totals.ToolCalls,
		ToolDurationMs: h.totals.ToolDurationMs,
	}
	if len(h.toolByName) > 0 {
		out.ToolByName = make(map[string]ToolCallStats, len(h.toolByName))
		maps.Copy(out.ToolByName, h.toolByName)
	}
	return out
}

// StartSubAgent flags the handler so subsequent events carry the named
// AgentLabel, and scopes a conversation of its own to the returned context: a
// sub-agent's message list is not a continuation of its caller's.
//
// On the durable runtime nothing reaches this hook — a sub-agent there is a
// separate Process, so it gets a separate claim and its own Handler, which
// already separates the two conversations. This covers a gollem-driven agent,
// where the same Handler serves both.
func (h *Handler) StartSubAgent(ctx context.Context, name string) context.Context {
	h.mu.Lock()
	h.agentLabel = name
	h.mu.Unlock()
	return withConversation(ctx)
}

// EndSubAgent clears the AgentLabel back to empty.
func (h *Handler) EndSubAgent(ctx context.Context, err error) {
	h.mu.Lock()
	h.agentLabel = ""
	h.mu.Unlock()
}

// StartChildAgent mirrors StartSubAgent for the child-agent variant.
func (h *Handler) StartChildAgent(ctx context.Context, name string) context.Context {
	return h.StartSubAgent(ctx, name)
}

// EndChildAgent mirrors EndSubAgent.
func (h *Handler) EndChildAgent(ctx context.Context, err error) {
	h.EndSubAgent(ctx, err)
}

// AddEvent is a no-op; reserved for future debug-event payloads.
func (h *Handler) AddEvent(ctx context.Context, kind string, data any) {}

// Finish is a no-op: events have been appended per-call. The hook is kept for
// trace.Handler contract compatibility.
func (h *Handler) Finish(ctx context.Context) error { return nil }

// phaseReflection labels the events emitted by the post-execution reflection
// agent so they are distinguishable from the main "execute" phase events in
// the JobRunEvent timeline.
const phaseReflection = "reflection"

// EnterReflectionPhase relabels subsequent events as the reflection phase.
// The Job runner calls it once, after the executor returns and before invoking
// the reflector, so the reflection agent's LLM / tool events are attributed to
// "reflection". Safe because the run is single-threaded at that point.
func (h *Handler) EnterReflectionPhase() {
	h.mu.Lock()
	h.phase = phaseReflection
	h.mu.Unlock()
}

// EmitRunError appends a RUN_ERROR event. Called by the run's owner on lifecycle
// failures (prepare / execute / finish stages).
//
// It orders correctly against the per-call events without sharing anything with
// them: the Sequence is allocated by the repository, so the owner and the handler
// cannot pick the same number even when they append at the same moment.
func (h *Handler) EmitRunError(ctx context.Context, stage, message string) error {
	ev := h.baseEvent(model.JobRunEventKindRunError, h.clock())
	ev.RunError = &model.RunErrorPayload{
		Stage:   stage,
		Message: Truncate(message, model.MaxInlineBytes),
	}
	if err := h.appendNext(ctx, ev); err != nil {
		return goerr.Wrap(err, "append run_error event",
			goerr.V("run_id", h.routing.RunID))
	}
	return nil
}

// baseEvent stamps the common identifier / phase / occurred-at fields onto a
// fresh event. The caller fills in the kind-specific payload. EventID is a
// freshly minted UUIDv7 (timestamp-prefixed for Firestore-console readability);
// the authoritative monotonic order is the Sequence field, not the doc ID.
//
// Sequence is deliberately left unset: the repository allocates it inside the
// write (AppendNext), so the number cannot be handed out twice.
func (h *Handler) baseEvent(kind model.JobRunEventKind, at time.Time) *model.JobRunEvent {
	h.mu.Lock()
	phase := h.phase
	agentLabel := h.agentLabel
	h.mu.Unlock()
	return &model.JobRunEvent{
		WorkspaceID: h.routing.WorkspaceID,
		CaseID:      h.routing.CaseID,
		JobID:       h.routing.JobID,
		RunID:       h.routing.RunID,
		TraceID:     h.routing.TraceID,
		EventID:     uuid.Must(uuid.NewV7()).String(),
		OccurredAt:  at,
		Kind:        kind,
		Phase:       phase,
		AgentLabel:  agentLabel,
		ProcessID:   h.routing.ProcessID,
	}
}

// append persists the event. Per-event failures are non-fatal: log via
// errutil.Handle so a single bad event does not drop the rest of the run's
// trail.
func (h *Handler) append(ctx context.Context, ev *model.JobRunEvent) {
	// AppendNext, so the Sequence is allocated inside the write. Two appenders on
	// the same run — a durable run reclaimed by another instance, or the run
	// owner's RUN_ERROR racing a per-call event — would otherwise pick the same
	// number and List would order the timeline arbitrarily between them.
	if err := h.appendNext(ctx, ev); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "append job run event",
			goerr.V("run_id", h.routing.RunID),
			goerr.V("sequence", ev.Sequence),
			goerr.V("kind", string(ev.Kind))),
			"runtrace: persist agent trace event")
		return
	}
	if logger := logging.From(ctx); logger != nil {
		attrs := []any{
			"run_id", h.routing.RunID,
			"sequence", ev.Sequence,
			"kind", string(ev.Kind),
		}
		// The measured latency, on the one line that already exists per event.
		// No new per-event line is emitted: the run's aggregate is on the run
		// summary, and this stays at DEBUG for per-call drill-down.
		if ev.LLMResponse != nil {
			attrs = append(attrs, "duration_ms", ev.LLMResponse.DurationMs)
		}
		if ev.ToolCall != nil {
			attrs = append(attrs,
				"tool_name", ev.ToolCall.ToolName,
				"duration_ms", max(ev.ToolCall.EndedAt.Sub(ev.ToolCall.StartedAt).Milliseconds(), 0))
		}
		logger.Debug("appended job run event", attrs...)
	}
}

func (h *Handler) appendNext(ctx context.Context, ev *model.JobRunEvent) error {
	// A claim cancellation must stop agent work, but it must not discard the
	// event that describes how that work ended. The timeout keeps this detached
	// Firestore write from delaying shutdown indefinitely.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventPersistenceTimeout)
	defer cancel()
	return h.eventRepo.AppendNext(persistCtx, ev)
}

func spanFromContext(ctx context.Context) *handlerSpan {
	s, _ := ctx.Value(handlerSpanKey{}).(*handlerSpan)
	return s
}

// payloadTruncator builds the kind-specific payloads from gollem trace data,
// applying length caps so the resulting Firestore doc stays well under the
// 1 MiB limit.
type payloadTruncator interface {
	LLMRequestFromTrace(data *trace.LLMCallData) *model.LLMRequestPayload
	LLMResponseFromTrace(data *trace.LLMCallData, durationMs int64) *model.LLMResponsePayload
	ToolCallFromTrace(toolName string, args, result map[string]any, err error, startedAt, endedAt time.Time) *model.ToolCallPayload
}

// defaultPayloadTruncator is the production implementation: it copies gollem
// trace data into our payload types, applying Truncate to every text/JSON
// field.
type defaultPayloadTruncator struct{}

func (defaultPayloadTruncator) LLMRequestFromTrace(data *trace.LLMCallData) *model.LLMRequestPayload {
	if data == nil {
		return &model.LLMRequestPayload{}
	}
	p := &model.LLMRequestPayload{Model: data.Model}
	if data.Request != nil {
		p.Messages = make([]model.LLMMessage, 0, len(data.Request.Messages))
		for _, m := range data.Request.Messages {
			p.Messages = append(p.Messages, convertMessage(m))
		}
		p.Tools = make([]model.LLMToolSpec, 0, len(data.Request.Tools))
		for _, ts := range data.Request.Tools {
			p.Tools = append(p.Tools, model.LLMToolSpec{
				Name:        Truncate(ts.Name, model.MaxInlineBytes),
				Description: Truncate(ts.Description, model.MaxInlineBytes),
			})
		}
	}
	return p
}

func (defaultPayloadTruncator) LLMResponseFromTrace(data *trace.LLMCallData, durationMs int64) *model.LLMResponsePayload {
	if data == nil {
		return &model.LLMResponsePayload{DurationMs: durationMs}
	}
	p := &model.LLMResponsePayload{
		Model:                    data.Model,
		InputTokens:              int64(data.InputTokens),
		OutputTokens:             int64(data.OutputTokens),
		CacheCreationInputTokens: int64(data.CacheCreationInputTokens),
		CacheReadInputTokens:     int64(data.CacheReadInputTokens),
		DurationMs:               durationMs,
	}
	if data.Response != nil {
		p.Texts = make([]string, 0, len(data.Response.Texts))
		for _, txt := range data.Response.Texts {
			p.Texts = append(p.Texts, Truncate(txt, model.MaxInlineBytes))
		}
		p.FunctionCalls = make([]model.LLMFunctionCall, 0, len(data.Response.FunctionCalls))
		for _, fc := range data.Response.FunctionCalls {
			if fc == nil {
				continue
			}
			p.FunctionCalls = append(p.FunctionCalls, model.LLMFunctionCall{
				ID:            fc.ID,
				Name:          fc.Name,
				ArgumentsJSON: Truncate(marshalJSON(fc.Arguments), model.MaxInlineBytes),
			})
		}
	}
	return p
}

func (defaultPayloadTruncator) ToolCallFromTrace(toolName string, args, result map[string]any, err error, startedAt, endedAt time.Time) *model.ToolCallPayload {
	tc := &model.ToolCallPayload{
		ToolName:      toolName,
		ArgumentsJSON: Truncate(marshalJSON(args), model.MaxInlineBytes),
		ResultJSON:    Truncate(marshalJSON(result), model.MaxInlineBytes),
		StartedAt:     startedAt,
		EndedAt:       endedAt,
	}
	if err != nil {
		tc.IsError = true
		tc.ErrorMessage = Truncate(err.Error(), model.MaxInlineBytes)
	}
	return tc
}

func convertMessage(m trace.Message) model.LLMMessage {
	out := model.LLMMessage{
		Role:     m.Role,
		Contents: make([]model.LLMContentBlock, 0, len(m.Contents)),
	}
	for _, c := range m.Contents {
		out.Contents = append(out.Contents, convertContent(c))
	}
	return out
}

func convertContent(c trace.MessageContent) model.LLMContentBlock {
	block := model.LLMContentBlock{
		Type:       c.Type,
		Text:       Truncate(c.Text, model.MaxInlineBytes),
		ID:         c.ID,
		Name:       c.Name,
		ToolCallID: c.ToolCallID,
		MediaType:  c.MediaType,
		URL:        c.URL,
		Title:      c.Title,
	}
	if c.Arguments != nil {
		block.ArgumentsJSON = Truncate(marshalJSON(c.Arguments), model.MaxInlineBytes)
	}
	if c.Result != nil {
		block.ResultJSON = Truncate(marshalJSON(c.Result), model.MaxInlineBytes)
	}
	return block
}

// marshalJSON serialises v to JSON, falling back to an empty string on error
// or nil-ish input. We never want a serialisation hiccup to abort the trace. A
// typed nil map (`map[string]any(nil)`) is treated as empty rather than
// literal "null" so empty payloads round-trip cleanly.
func marshalJSON(v any) string {
	if v == nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok && m == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// Truncate returns s capped at max bytes, snapped back to a UTF-8 rune
// boundary so the result never contains a partial multi-byte character.
// Firestore rejects strings that are not valid UTF-8, and a blind byte-slice
// can leave a trailing fragment of a JP/CJK rune which would make the doc
// unwritable.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
