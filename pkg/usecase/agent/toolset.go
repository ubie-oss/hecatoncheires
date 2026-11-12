package agent

import (
	"slices"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/actionwriter"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casemulti"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casewriter"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/core"
	githubtool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/github"
	knowledgetool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/knowledge"
	memotool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/memo"
	notiontool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/notion"
	slacktool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slackpost"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/webfetch"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/wsmeta"
)

// ToolSet IDs known to the planner. Sub-agents request a subset of these
// per investigation task and the resolver below maps each ID to a concrete
// []gollem.Tool slice.
const (
	ToolSetCoreRO   = "core_ro"
	ToolSetSlackRO  = "slack_ro"
	ToolSetNotion   = "notion"
	ToolSetGitHub   = "github"
	ToolSetWebFetch = "webfetch"
	ToolSetJira     = "jira"
	// ToolSetCore is the FULL action toolset (create / update / archive), as
	// opposed to the read-only core_ro handed to investigation sub-agents. The
	// channel-mode case agent and the thread-mode mention agent ask for it.
	ToolSetCore = "core"
	// ToolSetMemo is the Case-scoped memo toolset (memo__*). Built only when a
	// memo mutator and a memo schema are configured for the workspace.
	ToolSetMemo = "memo"
	// ToolSetKnowledge is the workspace knowledge toolset INCLUDING the write
	// tools. The read tools are always available to every agent regardless of
	// what it requested (see Resolve); this ID is what additionally grants
	// create/update, and a host withholds it while processing a private case so
	// that case's contents cannot leak into workspace-wide knowledge.
	ToolSetKnowledge = "knowledge"
	// ToolSetCaseWrite is the writer toolset for the single case the turn is
	// pinned to: the full casewriter set (case__update_case, case__assign,
	// case__unassign, and the mode-appropriate case__update_case_status /
	// case__close_case). Every mention-driven host grants it, so a sub-agent can
	// carry out any case edit the user asked for. Note that a mention turn's
	// terminal `materialize` decision replaces title / description wholesale, so
	// a host offering both must tell the planner to pick one path per turn (see
	// threadcase's system prompt).
	ToolSetCaseWrite = "case_write"
	// ToolSetCaseMulti is the cross-case ("workspace-scoped") toolset used by the
	// workspace-channel agent. Unlike core/case_write (pinned to one case),
	// its tools take case_id as a call-time argument so a single turn can operate
	// across every case the requesting user can access. Advertised only via
	// KnownToolSetIDsWorkspaceChannel (never the default lists) so it is not
	// offered to the per-case mention / proposal planners.
	ToolSetCaseMulti = "case_multi"
	// ToolSetSlackWrite is the read-only Slack set PLUS slack__post_message,
	// pinned to the channel of the case the run is on. Like ToolSetKnowledge it
	// REPLACES the read-only set rather than adding to it, so no tool is offered
	// twice. Only the assist agent asks for it: assist exists to write its
	// findings back into the case channel, whereas a mention turn's reply is
	// posted by its host, not by a tool the model may call at will.
	ToolSetSlackWrite = "slack_write"
	// ToolSetCoreJob is the action toolset an UNATTENDED run gets: the read tools
	// plus create / update / status / assignee / steps, but NOT archive,
	// unarchive or delete_action_step. Those three are withheld because a Job
	// acts on its own judgement with nobody reviewing it, and a wrong archive is
	// work the team can no longer see.
	ToolSetCoreJob = "core_job"
	// ToolSetSlackPost is the channel-pinned poster (slack__post_to_case_channel).
	// A Job's output reaches people only through it, which is why an unattended
	// run has it while an interactive turn — whose reply its host posts — does not.
	ToolSetSlackPost = "slack_post"
	// ToolSetWSMeta is the workspace-metadata read set (list_workspaces /
	// get_workspace). It is what the case-draft planner picks a workspace with:
	// that flow is not pinned to one workspace, so it must read the candidates'
	// field schemas and configured sources before it can propose anything.
	ToolSetWSMeta = "wsmeta"
	// ToolSetMemoRO is the read half of ToolSetMemo (memo__list_memos /
	// memo__get_memo), and ToolSetCaseMultiRO the read half of ToolSetCaseMulti.
	// Neither is in any planner-facing palette: they exist so that
	// ReadOnlyToolSetIDs can declare a planner's own tools without the writers.
	ToolSetMemoRO      = "memo_ro"
	ToolSetCaseMultiRO = "case_multi_ro"
)

// KnownToolSetIDs is the canonical list of identifiers a planner is allowed
// to request. Anything outside this list is rejected at plan validation.
var KnownToolSetIDs = []string{
	ToolSetCoreRO,
	ToolSetSlackRO,
	ToolSetNotion,
	ToolSetGitHub,
	ToolSetWebFetch,
	ToolSetJira,
}

// KnownToolSetIDsNoCore is KnownToolSetIDs without the core (action) toolset.
// Thread-mode create / materialize turns advertise this list to the planner:
// they are given no Action tools, so the planner must never be offered the
// core read tools (list/get action). Paired with ToolSetDeps.OmitCore so the
// resolver also withholds the underlying tools.
var KnownToolSetIDsNoCore = []string{
	ToolSetSlackRO,
	ToolSetNotion,
	ToolSetGitHub,
	ToolSetWebFetch,
	ToolSetJira,
}

// KnownToolSetIDsThreadWrite is KnownToolSetIDsNoCore plus the case writer
// toolset, the action toolsets and the memo toolset. Thread-mode agents
// advertise this to the planner on mention turns, where a concrete case exists
// to act on and a human asked for the change: a sub-agent may then edit,
// assign, and transition that case, read (core_ro) or create / update /
// archive (core) its Actions, and read or write its memos. The action ids
// resolve to tools only when the mention turn's scope sets
// Scope.ThreadModeActions (the workspace enables [case] actions), and the memo
// id only when the workspace enables memos; the host probes the palette, so an
// id that resolves to nothing is never advertised. Materialize and creation turns advertise the
// plain KnownToolSetIDsNoCore instead, so the planner is never offered a writer
// tool the resolver cannot wire — the prompt-vs-capability mismatch the
// architecture rule forbids.
var KnownToolSetIDsThreadWrite = append(append([]string{}, KnownToolSetIDsNoCore...), ToolSetCaseWrite, ToolSetCoreRO, ToolSetCore, ToolSetMemo)

// KnownToolSetIDsWorkspaceChannel is the planner-advertised list for the
// workspace-channel agent: the cross-case toolset plus the read-only auxiliary
// toolsets. It deliberately omits core_ro (the case-pinned action read tools) —
// case_multi carries the cross-case action tools instead.
var KnownToolSetIDsWorkspaceChannel = []string{
	ToolSetCaseMulti,
	ToolSetSlackRO,
	ToolSetNotion,
	ToolSetGitHub,
	ToolSetWebFetch,
	ToolSetJira,
}

// KnownToolSetIDsCaseChannel is the full palette of the channel-mode case
// agent: the mutating action tools, the single-case writer tools, memos,
// knowledge writes, and every read-only auxiliary set. Unlike the planner-facing
// lists above it is not a menu an LLM chooses from — the channel-mode agent runs
// a single ReAct loop and is handed the whole set at once.
var KnownToolSetIDsCaseChannel = []string{
	ToolSetCore,
	ToolSetSlackRO,
	ToolSetNotion,
	ToolSetGitHub,
	ToolSetWebFetch,
	ToolSetJira,
	ToolSetCaseWrite,
	ToolSetMemo,
	ToolSetKnowledge,
}

// KnownToolSetIDsAssist is the palette of the assist agent: the mutating action
// tools, Slack read plus post, and the read-only auxiliary sets. It deliberately
// omits case_write, memo and knowledge — assist runs unattended on every open
// case of a workspace, and today's assist agent has none of them.
var KnownToolSetIDsAssist = []string{
	ToolSetCore,
	ToolSetSlackWrite,
	ToolSetNotion,
	ToolSetGitHub,
	ToolSetWebFetch,
	ToolSetJira,
}

// KnownToolSetIDsJob is the palette of an unattended Job run. It matches what
// the pre-agentkit buildJobTools assembled: the Job-safe action set, the case
// writer, the channel-pinned poster, Slack reads, the read-only integrations,
// memos and knowledge. Compared with the interactive mention agent it withholds
// archive / unarchive / delete_action_step (see ToolSetCoreJob) and GitHub.
//
// core_ro is listed beside core_job for a thread-mode workspace that enables
// [case] actions: there a Job may read the case's Actions but not write them
// (ToolSetDeps.CoreReadOnly), so core_job resolves to nothing and core_ro is
// the id that carries the reads. Where core_job does resolve, Resolve drops
// core_ro from a request naming both, so no tool is offered twice.
var KnownToolSetIDsJob = []string{
	ToolSetCoreJob,
	ToolSetCoreRO,
	ToolSetCaseWrite,
	ToolSetSlackPost,
	ToolSetSlackRO,
	ToolSetNotion,
	ToolSetWebFetch,
	ToolSetJira,
	ToolSetMemo,
	ToolSetKnowledge,
}

// KnownToolSetIDsProposal is the palette of the case-draft agent. It is
// KnownToolSetIDs plus wsmeta: the draft flow is not pinned to a workspace, so
// reading the candidates' field schemas and configured sources is the first thing
// it must do.
var KnownToolSetIDsProposal = append(append([]string{}, KnownToolSetIDs...), ToolSetWSMeta)

// IsKnownToolSetID reports whether id is a member of KnownToolSetIDs.
func IsKnownToolSetID(id string) bool {
	return slices.Contains(KnownToolSetIDs, id)
}

// readOnlyToolSetIDs maps every toolset id to the id of its read tools. An empty
// value means the set has no read tool of its own to keep: slack_post and
// case_write only write, and the knowledge read tools are included by Resolve
// whatever is requested.
var readOnlyToolSetIDs = map[string]string{
	ToolSetCoreRO:      ToolSetCoreRO,
	ToolSetCore:        ToolSetCoreRO,
	ToolSetCoreJob:     ToolSetCoreRO,
	ToolSetSlackRO:     ToolSetSlackRO,
	ToolSetSlackWrite:  ToolSetSlackRO,
	ToolSetMemo:        ToolSetMemoRO,
	ToolSetMemoRO:      ToolSetMemoRO,
	ToolSetCaseMulti:   ToolSetCaseMultiRO,
	ToolSetCaseMultiRO: ToolSetCaseMultiRO,
	ToolSetNotion:      ToolSetNotion,
	ToolSetGitHub:      ToolSetGitHub,
	ToolSetWebFetch:    ToolSetWebFetch,
	ToolSetJira:        ToolSetJira,
	ToolSetWSMeta:      ToolSetWSMeta,
	ToolSetSlackPost:   "",
	ToolSetCaseWrite:   "",
	ToolSetKnowledge:   "",
}

// ReadOnlyToolSetIDs replaces each id with the id of its read tools, dropping
// the ids that have none and keeping the first occurrence of each result.
//
// It is how a plan-execute agent's planner is declared only the read half of
// its palette: a write the planner makes itself bypasses the task it would
// otherwise be planned as, and with it the toolset that task was granted.
//
// An id this table does not classify is an error rather than passed through, so
// a toolset added later cannot reach a planner with its writers until someone
// decides which of its tools are reads.
func ReadOnlyToolSetIDs(ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		ro, ok := readOnlyToolSetIDs[id]
		if !ok {
			return nil, goerr.New("toolset id has no read-only classification",
				goerr.V("toolset", id))
		}
		if ro != "" && !slices.Contains(out, ro) {
			out = append(out, ro)
		}
	}
	return out, nil
}

// ToolSetResolver builds gollem.Tool slices for sub-agents based on a list
// of ToolSet IDs. The resolver is created once per turn (with the deps that
// vary per turn — workspace, case, slack/notion/github clients) and called
// per sub-agent.
type ToolSetResolver struct {
	core []gollem.Tool
	// coreFull is the mutating action tool set (ToolSetCore). It is separate
	// from core (read-only) rather than a superset flag because one resolver
	// can be asked for either by different tasks in the same run.
	coreFull []gollem.Tool
	// coreJob is the unattended-run action set (ToolSetCoreJob): reads plus the
	// non-destructive writes.
	coreJob []gollem.Tool
	// slackPost is the channel-pinned poster (ToolSetSlackPost). Empty unless a
	// poster and a case channel are both known.
	slackPost []gollem.Tool
	// memo is the Case-scoped memo tool set (ToolSetMemo).
	memo []gollem.Tool
	// memoRO is memo without the write tool (ToolSetMemoRO), built under the
	// same condition as memo.
	memoRO []gollem.Tool
	// knowledgeWrite is the knowledge tool set including create/update
	// (ToolSetKnowledge). Empty unless a mutator is wired.
	knowledgeWrite []gollem.Tool
	slack          []gollem.Tool
	// slackWrite is the Slack set including post_message (ToolSetSlackWrite).
	// Without a case channel it degrades to the same tools as slack, so a case
	// with no Slack channel keeps its Slack reads.
	slackWrite []gollem.Tool
	notion     []gollem.Tool
	github     []gollem.Tool
	webfetch   []gollem.Tool
	// jira is the already-expanded Jira read tool set (see
	// pkg/agent/tool/jira). Unlike notion/github/webfetch this is not built
	// from a client here: it is handed in pre-expanded via ToolSetDeps.Jira
	// because gollem has no exported ToolSet-to-[]Tool helper.
	jira []gollem.Tool
	// caseWrite is the single-case writer tool set (case_write). Unlike
	// knowledge it is NOT always included: a sub-agent gets it only when the
	// planner requested ToolSetCaseWrite for that task. Empty unless
	// ToolSetDeps.CaseWrite identifies a concrete case to write to.
	caseWrite []gollem.Tool
	// knowledge is the read-only workspace knowledge tool set. It is always
	// included in every Resolve result (not gated by a planner-requested ID):
	// investigation sub-agents may always consult shared knowledge, but never
	// mutate it (write tools are wired only in the case-bound / job paths).
	knowledge []gollem.Tool
	// caseMulti is the cross-case ("workspace-scoped") tool set (case_multi).
	// Unlike caseWrite it carries full cross-case read+write tools taking
	// case_id at call time. Empty unless ToolSetDeps.CaseMulti.CaseUC is set;
	// gated on the planner requesting ToolSetCaseMulti. Used by the
	// workspace-channel agent, never the per-case mention / proposal planners.
	caseMulti []gollem.Tool
	// caseMultiRO is caseMulti's read tools (ToolSetCaseMultiRO), built under
	// the same condition as caseMulti.
	caseMultiRO []gollem.Tool
	// wsmeta is the workspace-metadata read set (ToolSetWSMeta): the registered
	// workspaces and their field schemas / sources. Empty unless a registry is
	// wired.
	wsmeta []gollem.Tool
}

// ToolSetDeps carries the per-turn deps that flavor each toolset's binding.
// Optional fields (SlackSearch / NotionClient / GitHubClient) may be nil; the
// corresponding toolset is empty in that case.
type ToolSetDeps struct {
	Core      core.Deps
	Slack     slacktool.Deps
	Notion    notiontool.Deps
	GitHub    *githubtool.Client
	WebFetch  *webfetch.Client
	Knowledge knowledgetool.Deps

	// Jira carries the already-expanded Jira read tools (see
	// pkg/agent/tool/jira). nil/empty means Jira is not configured, so the
	// "jira" ToolSet ID resolves to nothing.
	Jira []gollem.Tool

	// SlackPost backs the slack_post toolset. Built when a poster and a channel
	// are both known; a zero value leaves the toolset empty so requesting the id
	// resolves to nothing rather than to a tool that posts nowhere.
	SlackPost slackpost.Deps

	// CaseWrite backs the case_write toolset (the full single-case writer set).
	// The tools are built when CaseUC and CaseID identify a concrete case; a zero
	// value (no case yet, or no mutator wired) leaves the toolset empty so
	// requesting the ID resolves to nothing. StatusSet selects the mode-specific
	// "mark done" tool (case__update_case_status when set, case__close_case when
	// not) and Schema drives case__update_case's custom-field coercion.
	CaseWrite casewriter.Deps

	// OmitCore omits the core (action) toolset entirely. Set for every run on a
	// thread-mode workspace except the mention turn (see
	// kernel.Scope.ThreadModeActions): those agents are given no Actions, so
	// even the read-only list/get-action tools must not exist. Without this the
	// resolver would always build them (they only need Repo), since the
	// core read tools do not depend on ActionUC being wired.
	OmitCore bool

	// CoreReadOnly builds only the read half of the core (action) toolset:
	// core_ro resolves to list / get actions (and their steps), while core and
	// core_job resolve to nothing. Set for a Job on a thread-mode workspace that
	// enables [case] actions (kernel.Scope.ThreadModeActionReads). OmitCore
	// takes precedence when both are set.
	CoreReadOnly bool

	// CaseMulti backs the case_multi (cross-case) toolset. Built only when
	// CaseMulti.CaseUC is non-nil (the workspace-channel host wires it); a nil
	// CaseUC leaves the toolset empty so requesting the ID resolves to nothing.
	CaseMulti casemulti.Deps

	// Memo backs the memo toolset. Built only when Memo.MemoUC and a memo
	// schema are present; a zero value leaves the toolset empty so requesting
	// the ID resolves to nothing.
	Memo memotool.Deps

	// WSMeta backs the wsmeta toolset (list_workspaces / get_workspace). Only the
	// case-draft flow needs it: every other host already knows which workspace it
	// runs in, and hands that workspace's schema to its tools directly.
	WSMeta wsmeta.Deps
}

// NewToolSetResolver builds the per-toolset slices once so each sub-agent
// just picks the union of its requested IDs. The "core" pool is the read-only
// subset (list / get only) — investigation sub-agents must not mutate the
// case while a turn is forming.
func NewToolSetResolver(d ToolSetDeps) *ToolSetResolver {
	var coreTools []gollem.Tool
	var coreFullTools []gollem.Tool
	var coreJobTools []gollem.Tool
	switch {
	case d.OmitCore:
		// No action tools at all, not even the reads.
	case d.CoreReadOnly:
		coreTools = core.NewReadOnly(d.Core)
	default:
		coreTools = core.NewReadOnly(d.Core)
		coreFullTools = core.New(d.Core)
		// The unattended set is the reads plus the non-destructive writes. It is
		// assembled from the same two builders the pre-agentkit Job path used, so
		// the withheld three (archive / unarchive / delete_action_step) stay
		// withheld by construction rather than by a list that could drift.
		coreJobTools = append(append([]gollem.Tool{}, coreTools...), actionwriter.New(d.Core)...)
	}
	var knowledge []gollem.Tool
	var knowledgeWrite []gollem.Tool
	if d.Knowledge.Accessor != nil {
		knowledge = knowledgetool.NewReadOnly(d.Knowledge)
		// The write tools need a mutator. A host that wires none (or withholds
		// it for a private case) leaves this empty, so requesting the knowledge
		// id resolves to the read tools it already had.
		if d.Knowledge.Mutator != nil {
			knowledgeWrite = knowledgetool.New(d.Knowledge)
		}
	}
	// Memo tools need both a mutator and the workspace's memo schema; the schema
	// drives field coercion in create/update.
	var memoTools []gollem.Tool
	var memoROTools []gollem.Tool
	if d.Memo.MemoUC != nil && d.Memo.Schema != nil {
		memoTools = memotool.New(d.Memo)
		memoROTools = memotool.NewReadOnly(d.Memo)
	}
	// The writer tools need a mutator and a concrete case to be pinned to. A
	// create turn (no case yet) or a host that wires no CaseUC leaves the set
	// empty, so a stray case_write request resolves to nothing rather than
	// producing tools pinned to case 0.
	var caseWrite []gollem.Tool
	if d.CaseWrite.CaseUC != nil && d.CaseWrite.CaseID != 0 {
		caseWrite = casewriter.New(d.CaseWrite)
	}
	// The cross-case toolset is built only when a CaseUsecase is wired (the
	// workspace-channel host); casemulti.New returns nil otherwise.
	var caseMulti []gollem.Tool
	var caseMultiRO []gollem.Tool
	if d.CaseMulti.CaseUC != nil {
		caseMulti = casemulti.New(d.CaseMulti)
		caseMultiRO = casemulti.NewReadOnly(d.CaseMulti)
	}
	// The posting tool needs the channel of the case the run is on; NewForAssist
	// degrades to the read-only set without one. It is built unconditionally so
	// that a case with no Slack channel still keeps its Slack READS — withholding
	// the whole set there would take away tools that work.
	slackWrite := slacktool.NewForAssist(d.Slack)
	// The poster is pinned to one channel and cannot be redirected by the model,
	// so without a channel there is nothing to build.
	var slackPost []gollem.Tool
	if d.SlackPost.Poster != nil && d.SlackPost.ChannelID != "" {
		slackPost = slackpost.New(d.SlackPost)
	}
	// The workspace-metadata tools read the registry, so they need nothing
	// per-case; a host that wires no registry leaves the set empty.
	var wsmetaTools []gollem.Tool
	if d.WSMeta.Registry != nil {
		wsmetaTools = wsmeta.New(d.WSMeta)
	}
	return &ToolSetResolver{
		core:           coreTools,
		coreFull:       coreFullTools,
		coreJob:        coreJobTools,
		slackPost:      slackPost,
		memo:           memoTools,
		memoRO:         memoROTools,
		knowledgeWrite: knowledgeWrite,
		slack:          slacktool.NewReadOnly(d.Slack),
		slackWrite:     slackWrite,
		notion:         notiontool.New(d.Notion),
		github:         githubtool.New(d.GitHub),
		webfetch:       webfetch.New(d.WebFetch),
		jira:           d.Jira,
		caseWrite:      caseWrite,
		knowledge:      knowledge,
		caseMulti:      caseMulti,
		caseMultiRO:    caseMultiRO,
		wsmeta:         wsmetaTools,
	}
}

// Resolve returns the concatenated tool list for the requested IDs. Unknown
// IDs are skipped (they should already have been rejected by plan validation,
// but Resolve never panics so a stray ID does not crash a turn).
func (r *ToolSetResolver) Resolve(ids []string) []gollem.Tool {
	if r == nil {
		return nil
	}
	// Knowledge read tools are always available to every sub-agent, regardless
	// of which toolset IDs the planner requested.
	if len(ids) == 0 {
		if len(r.knowledge) == 0 {
			return nil
		}
		out := make([]gollem.Tool, len(r.knowledge))
		copy(out, r.knowledge)
		return out
	}
	// The knowledge base set is read-only, and requesting ToolSetKnowledge
	// REPLACES it with the read+write set rather than adding to it — the write
	// set already contains the read tools, and offering a tool twice makes the
	// model's tool list ambiguous.
	base := r.knowledge
	if slices.Contains(ids, ToolSetKnowledge) && len(r.knowledgeWrite) > 0 {
		base = r.knowledgeWrite
	}

	// slack_write already contains the read tools, so a request naming both
	// resolves to the write set alone — offering a tool twice makes the model's
	// tool list ambiguous.
	if slices.Contains(ids, ToolSetSlackWrite) && len(r.slackWrite) > 0 {
		ids = withoutToolSet(ids, ToolSetSlackRO)
	}
	// core and core_job already contain the core read tools, so a request
	// naming either alongside core_ro resolves without the duplicate reads.
	if (slices.Contains(ids, ToolSetCore) && len(r.coreFull) > 0) ||
		(slices.Contains(ids, ToolSetCoreJob) && len(r.coreJob) > 0) {
		ids = withoutToolSet(ids, ToolSetCoreRO)
	}

	// Pre-compute capacity to avoid repeated growth.
	total := len(base)
	for _, id := range ids {
		total += len(r.setFor(id))
	}
	out := make([]gollem.Tool, 0, total)
	out = append(out, base...)
	for _, id := range ids {
		out = append(out, r.setFor(id)...)
	}
	return out
}

// withoutToolSet returns ids with drop removed, leaving the caller's slice
// untouched.
func withoutToolSet(ids []string, drop string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// Has reports whether id contributes at least one tool. It is what a host filters
// its planner-facing palette with, so an id that would resolve to nothing is never
// offered as a choice.
//
// It deliberately does NOT count the knowledge read tools Resolve always includes:
// those are present for every sub-agent regardless of what was requested, so
// counting them would report every id as available.
//
// ToolSetKnowledge is the one id whose availability is not its own slice —
// requesting it REPLACES that always-present base with the read+write set — so it
// is answered by whether that write-bearing set was built.
func (r *ToolSetResolver) Has(id string) bool {
	if r == nil {
		return false
	}
	if id == ToolSetKnowledge {
		return len(r.knowledgeWrite) > 0
	}
	return len(r.setFor(id)) > 0
}

// setFor maps one toolset id to its built slice. Unknown ids resolve to nothing
// so a stray id cannot crash a turn.
//
// ToolSetKnowledge resolves to nothing here on purpose: it selects which
// knowledge set Resolve uses as its always-included base, so returning it again
// would duplicate every knowledge tool.
func (r *ToolSetResolver) setFor(id string) []gollem.Tool {
	switch id {
	case ToolSetCoreRO:
		return r.core
	case ToolSetCore:
		return r.coreFull
	case ToolSetCoreJob:
		return r.coreJob
	case ToolSetSlackPost:
		return r.slackPost
	case ToolSetSlackRO:
		return r.slack
	case ToolSetSlackWrite:
		return r.slackWrite
	case ToolSetNotion:
		return r.notion
	case ToolSetGitHub:
		return r.github
	case ToolSetWebFetch:
		return r.webfetch
	case ToolSetJira:
		return r.jira
	case ToolSetCaseWrite:
		return r.caseWrite
	case ToolSetCaseMulti:
		return r.caseMulti
	case ToolSetCaseMultiRO:
		return r.caseMultiRO
	case ToolSetMemo:
		return r.memo
	case ToolSetMemoRO:
		return r.memoRO
	case ToolSetWSMeta:
		return r.wsmeta
	default:
		return nil
	}
}
