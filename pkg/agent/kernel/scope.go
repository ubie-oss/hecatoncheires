// Package kernel assembles the agentkit Kernel this application runs its
// agents on: the tool factory, the observability middleware, and the typed view
// of the per-Process scope that both of them read.
package kernel

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// Metadata keys. They are unexported because Scope is the only supported way to
// read or write Process.Metadata: a caller reaching for a raw string key is one
// typo away from a silently empty scope, and the failure would surface as an
// agent with no tools rather than as a compile error.
const (
	metaWorkspaceID  = "workspace_id"
	metaCaseID       = "case_id"
	metaChannelID    = "channel_id"
	metaThreadTS     = "thread_ts"
	metaSessionID    = "session_id"
	metaActorUserID  = "actor_user_id"
	metaLang         = "lang"
	metaToolSets     = "toolsets"
	metaPrivateCase  = "private_case"
	metaJobID        = "job_id"
	metaJobRunID     = "job_run_id"
	metaEventType    = "event_type"
	metaSlotGated    = "slot_gated"
	metaUIChannelID  = "ui_channel_id"
	metaUIThreadTS   = "ui_thread_ts"
	metaProcessingTS = "processing_ts"
	metaPreviewTS    = "preview_ts"
	metaProposalID   = "proposal_id"
	metaLLMModel     = "llm_model"
	metaBudget       = "budget_nano_usd"

	metaThreadModeActions = "thread_mode_actions"
	metaThreadModeReads   = "thread_mode_reads"
)

// ToolSetsAll is the toolsets value meaning "everything this agent kind is
// entitled to". A root Process uses it; a sub-agent Process instead carries the
// explicit subset its task was planned with.
const ToolSetsAll = "*"

// toolSetSeparator joins the toolsets list into one metadata value. The value
// carries one meaning — an ordered list of toolset ids — so the separator is not
// packing several semantics into a single string.
const toolSetSeparator = ","

// Scope is the typed view of Process.Metadata: the infrastructure-facing
// identifiers a claim needs to rebuild an agent's tools, its language, its
// access actor and its run records.
//
// It is data, not a credential. Every field is derived server-side before Spawn
// from an already-validated request; nothing here is re-verified at claim time,
// and nothing here may be treated as proof of anything (agentkit ADR-0011).
type Scope struct {
	// WorkspaceID identifies the workspace whose configuration and tools this
	// Process runs under. Empty only for the workspace-agnostic draft flow.
	WorkspaceID string
	// CaseID is the case this Process is pinned to, or 0 when there is none
	// (a draft turn, or a create turn before the case exists).
	CaseID int64
	// ChannelID / ThreadTS locate the run's own Slack thread — the one its Session
	// is keyed on, and the one its answer belongs in.
	ChannelID string
	ThreadTS  string
	// UIChannelID / UIThreadTS locate the thread the person who triggered the run
	// is watching, for the runs where that is a DIFFERENT thread: a case raised by
	// a reaction lives in the monitored channel, while the reactor is watching the
	// thread they reacted in. Progress, questions and failure notices go here;
	// the case's own content still goes to ChannelID / ThreadTS.
	//
	// Empty means the two are the same thread, which is the case for every other
	// run. UITarget resolves that.
	UIChannelID string
	UIThreadTS  string
	// ProcessingTS and PreviewTS name the Slack message a case-draft turn's result
	// replaces, and are mutually exclusive: ProcessingTS is the "working on it"
	// placeholder a fresh mention posted, PreviewTS is the existing draft preview a
	// workspace switch updates in place. They live on the scope because the turn
	// that posted them returns long before the result exists, and the completion
	// handler runs on whichever instance committed the last transition.
	ProcessingTS string
	PreviewTS    string
	// SessionID is the model.Session this thread belongs to. It doubles as the
	// turn-lock subject id.
	SessionID string
	// ActorUserID is the Slack user whose access this run acts under. The claim
	// middleware turns it into the request-scoped auth token, so a run without
	// one acts with no user scope at all.
	ActorUserID string
	// Lang is the i18n language tag for user-facing copy this run produces.
	Lang string
	// ToolSets is the list of toolset ids this Process may use, or the single
	// element ToolSetsAll.
	ToolSets []string
	// PrivateCase withholds the workspace-wide knowledge write tools: a private
	// case's contents must not reach shared knowledge through an agent write.
	PrivateCase bool
	// JobID / JobRunID / EventType tie the run to its JobRunLog record. Empty
	// for runs that keep no such record.
	JobID     string
	JobRunID  string
	EventType string
	// SlotGated subjects the run to the deployment-wide concurrency gate: a
	// claim on it waits for a free execution slot before any transition runs.
	//
	// The host decides it at spawn rather than the runtime inferring it, so the
	// kernel needs no opinion about which kinds of run are rate-limited. Today
	// only scheduled Job runs set it — an interactive turn is a person waiting
	// for an answer, and a lifecycle or manual run is a single deliberate
	// action, so making either queue behind a batch would be the wrong trade.
	SlotGated bool
	// ProposalID names the case draft this run writes its result into. Empty for
	// every run that is not a case-draft turn.
	//
	// It travels on the run because the Session's ProposalID is MUTABLE: a later
	// mention on the same thread points the Session at a new draft, and it can do
	// so while this run is still going. A completion handler that read the Session
	// instead would write this run's draft into whatever draft the thread points at
	// by then.
	ProposalID string
	// LLMModel is the reference name of the model this run generates through —
	// an [[llm_model]] entry's alias, or its model name when it declares none.
	// Empty means the deployment's default model. A child Process inherits the
	// metadata, so a run's sub-agents generate through the same model.
	//
	// It carries neither the provider nor the price on purpose: both are read
	// from the reference name by the ModelPolicy built at startup. A run whose
	// reference name is no longer defined therefore falls back to the default
	// client AND the default price together — carrying a rate here would let it
	// generate with one model while being metered at another's.
	LLMModel string
	// Budget is the greatest amount this run may spend. Zero means "not
	// specified", and the deployment's default budget applies.
	Budget pricing.NanoUSD
	// ThreadModeActions grants the action (core) toolsets to a run on a
	// thread-mode workspace, which withholds them by default. Only the
	// thread-mode mention turn sets it: a person asked for the work, and the
	// turn is pinned to an existing case. Every other thread-mode run — case
	// creation, materialize, Jobs, the workspace agent — keeps the default, so
	// none of them is handed Action tools its prompt does not describe. A child
	// Process inherits the flag with the metadata, which is what lets the
	// mention turn's sub-agents carry out the action writes it plans.
	ThreadModeActions bool
	// ThreadModeReads grants ONLY the read half of the core toolset (core_ro)
	// to a run on a thread-mode workspace: the case_ref lookups always, and
	// list / get actions (and their steps) when the workspace enables
	// [case] actions — decided when the run's tools are built, not here. The Job
	// host sets it on every thread-mode run: an unattended run may consult what
	// it needs to decide, but the Actions of a thread-mode workspace are left to
	// the people and the mention agent working the case, so the Job is given no
	// action writer. It is ignored when ThreadModeActions is set, which already
	// grants the reads along with the writes.
	ThreadModeReads bool
}

// Validate enforces the invariants the claim path depends on, so a wiring
// mistake fails at Spawn rather than as an agent that silently has no tools.
func (s Scope) Validate() error {
	if (s.ChannelID == "") != (s.ThreadTS == "") {
		return goerr.New("channel id and thread ts must be set together",
			goerr.V("channel_id", s.ChannelID), goerr.V("thread_ts", s.ThreadTS))
	}
	if (s.UIChannelID == "") != (s.UIThreadTS == "") {
		return goerr.New("ui channel id and ui thread ts must be set together",
			goerr.V("ui_channel_id", s.UIChannelID), goerr.V("ui_thread_ts", s.UIThreadTS))
	}
	// The two name different lifecycles of the same slot — a placeholder to
	// replace, or a preview to update in place — so a run carrying both would have
	// two answers to "where does the result go".
	if s.ProcessingTS != "" && s.PreviewTS != "" {
		return goerr.New("a run may name a processing placeholder or a preview to update, not both",
			goerr.V("processing_ts", s.ProcessingTS), goerr.V("preview_ts", s.PreviewTS))
	}
	if s.CaseID != 0 && s.WorkspaceID == "" {
		return goerr.New("workspace id is required when a case id is set",
			goerr.V("case_id", s.CaseID))
	}
	if s.CaseID < 0 {
		return goerr.New("case id must not be negative", goerr.V("case_id", s.CaseID))
	}
	if len(s.ToolSets) == 0 {
		return goerr.New("at least one toolset is required")
	}
	if slices.Contains(s.ToolSets, "") {
		return goerr.New("toolset ids must not be empty")
	}
	if s.JobRunID != "" && s.JobID == "" {
		return goerr.New("job id is required when a job run id is set",
			goerr.V("job_run_id", s.JobRunID))
	}
	// A gated run must be fully identified, because the gate records WHICH run
	// holds a slot. Without that the gate rejects the acquisition, the claim is
	// refused, and — since a refusal deliberately does not spend the retry budget
	// — the Process waits forever for capacity it can never be granted. Catching
	// it at Spawn turns a run that never starts into an error someone can read.
	if s.SlotGated && (s.WorkspaceID == "" || s.CaseID == 0 || s.JobID == "") {
		return goerr.New("a slot-gated run must name its workspace, case and job",
			goerr.V("workspace_id", s.WorkspaceID),
			goerr.V("case_id", s.CaseID),
			goerr.V("job_id", s.JobID))
	}
	// A negative budget would be read as "not specified" downstream and silently
	// hand the run the default allowance, which is the opposite of what whoever
	// wrote it meant.
	if s.Budget < 0 {
		return goerr.New("budget must not be negative", goerr.V("budget", int64(s.Budget)))
	}
	// The reference name is NOT checked against the defined models here: the
	// scope does not know the set. Existence is checked at startup and by
	// `validate`, and a run naming a model this build cannot resolve falls back
	// to the default one.
	return nil
}

// Metadata renders the scope for Spawn. Empty values are omitted so a reader
// can tell "not set" from "set to empty", and so the stored map stays small.
func (s Scope) Metadata() map[string]string {
	m := map[string]string{}
	put := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	put(metaWorkspaceID, s.WorkspaceID)
	if s.CaseID != 0 {
		m[metaCaseID] = strconv.FormatInt(s.CaseID, 10)
	}
	put(metaChannelID, s.ChannelID)
	put(metaThreadTS, s.ThreadTS)
	put(metaUIChannelID, s.UIChannelID)
	put(metaUIThreadTS, s.UIThreadTS)
	put(metaProcessingTS, s.ProcessingTS)
	put(metaPreviewTS, s.PreviewTS)
	put(metaProposalID, s.ProposalID)
	put(metaSessionID, s.SessionID)
	put(metaActorUserID, s.ActorUserID)
	put(metaLang, s.Lang)
	put(metaToolSets, joinToolSets(s.ToolSets))
	if s.PrivateCase {
		m[metaPrivateCase] = "1"
	}
	put(metaJobID, s.JobID)
	put(metaJobRunID, s.JobRunID)
	put(metaEventType, s.EventType)
	if s.SlotGated {
		m[metaSlotGated] = "1"
	}
	put(metaLLMModel, s.LLMModel)
	if s.Budget > 0 {
		m[metaBudget] = strconv.FormatInt(int64(s.Budget), 10)
	}
	if s.ThreadModeActions {
		m[metaThreadModeActions] = "1"
	}
	if s.ThreadModeReads {
		m[metaThreadModeReads] = "1"
	}
	return m
}

// ScopeFrom reads a scope back out of Process.Metadata.
//
// A malformed numeric value falls back to the zero value rather than failing:
// the map was written by Metadata on this same code path, so a bad value means
// the record was hand-edited or written by an older build, and refusing to run
// the Process would strand it with no way forward.
func ScopeFrom(m map[string]string) Scope {
	caseID, _ := strconv.ParseInt(m[metaCaseID], 10, 64)
	budget, _ := strconv.ParseInt(m[metaBudget], 10, 64)
	return Scope{
		WorkspaceID:  m[metaWorkspaceID],
		CaseID:       caseID,
		ChannelID:    m[metaChannelID],
		ThreadTS:     m[metaThreadTS],
		UIChannelID:  m[metaUIChannelID],
		UIThreadTS:   m[metaUIThreadTS],
		ProcessingTS: m[metaProcessingTS],
		PreviewTS:    m[metaPreviewTS],
		ProposalID:   m[metaProposalID],
		SessionID:    m[metaSessionID],
		ActorUserID:  m[metaActorUserID],
		Lang:         m[metaLang],
		ToolSets:     splitToolSets(m[metaToolSets]),
		PrivateCase:  m[metaPrivateCase] == "1",
		JobID:        m[metaJobID],
		JobRunID:     m[metaJobRunID],
		EventType:    m[metaEventType],
		SlotGated:    m[metaSlotGated] == "1",
		LLMModel:     m[metaLLMModel],
		Budget:       pricing.NanoUSD(budget),

		ThreadModeActions: m[metaThreadModeActions] == "1",
		ThreadModeReads:   m[metaThreadModeReads] == "1",
	}
}

// UITarget returns the thread the requester is watching, falling back to the
// run's own thread when they are the same. Callers use it instead of reading
// UIChannelID directly, so the "empty means the same thread" rule lives in one
// place.
func (s Scope) UITarget() (channelID, threadTS string) {
	if s.UIChannelID != "" {
		return s.UIChannelID, s.UIThreadTS
	}
	return s.ChannelID, s.ThreadTS
}

// WithToolSets returns a copy of the metadata map carrying a different toolset
// list. It is what a strategy uses when spawning a child, because
// SpawnChild's WithMetadata REPLACES the parent's map rather than merging into
// it: rebuilding the map from scratch there would drop the workspace and case
// the child needs to have any tools at all.
func WithToolSets(parent map[string]string, toolSets []string) map[string]string {
	next := maps.Clone(parent)
	if next == nil {
		next = map[string]string{}
	}
	if joined := joinToolSets(toolSets); joined != "" {
		next[metaToolSets] = joined
	} else {
		delete(next, metaToolSets)
	}
	return next
}

// WithBudget returns a copy of the metadata map carrying a different spend
// ceiling. It is the counterpart of WithToolSets for the other thing a strategy
// decides about a child it is spawning, and it copies for the same reason:
// SpawnChild's WithMetadata REPLACES the parent's map.
//
// A zero or negative amount REMOVES the key. That reads back as "not specified"
// and hands the child the deployment default — the same meaning Scope.Metadata
// gives an unset budget, so the two cannot disagree about what an absent key
// means.
func WithBudget(parent map[string]string, amount pricing.NanoUSD) map[string]string {
	next := maps.Clone(parent)
	if next == nil {
		next = map[string]string{}
	}
	if amount > 0 {
		next[metaBudget] = strconv.FormatInt(int64(amount), 10)
	} else {
		delete(next, metaBudget)
	}
	return next
}

func joinToolSets(ids []string) string {
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			kept = append(kept, id)
		}
	}
	return strings.Join(kept, toolSetSeparator)
}

func splitToolSets(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for id := range strings.SplitSeq(v, toolSetSeparator) {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}
