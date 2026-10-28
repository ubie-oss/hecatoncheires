package usecase

import "errors"

// Sentinel errors for use case layer
var (
	// Not found errors
	ErrCaseNotFound       = errors.New("case not found")
	ErrActionNotFound     = errors.New("action not found")
	ErrActionStepNotFound = errors.New("action step not found")
	// ErrActionCommentNotFound is returned when the targeted comment does not
	// exist under the given Action.
	ErrActionCommentNotFound = errors.New("action comment not found")

	// Status errors
	ErrCaseAlreadyClosed = errors.New("case is already closed")
	ErrCaseAlreadyOpen   = errors.New("case is already open")
	// ErrCaseIsDraft is returned when a status-transition operation (close /
	// reopen) is invoked on a case that is still in DRAFT. Drafts only leave
	// DRAFT via SubmitDraft (→ OPEN) or DiscardDraft (delete).
	ErrCaseIsDraft = errors.New("case is in draft state")
	// ErrCaseNotDraft is returned by draft-specific operations (Submit /
	// Discard) when the targeted case is not in DRAFT.
	ErrCaseNotDraft = errors.New("case is not a draft")
	// ErrCaseThreadModeUseStatus is returned by CloseCase / ReopenCase when the
	// targeted case is thread-mode (bound to a Slack thread). Thread-mode cases
	// change lifecycle by moving the configurable board status via
	// UpdateCaseStatus, which keeps BoardStatus and the lifecycle Status in sync;
	// closing / reopening one directly would set Status while leaving BoardStatus
	// on a mismatched column, desyncing the two. The boundary rejects it so a
	// mis-wired caller fails loudly instead of producing the inconsistent state.
	ErrCaseThreadModeUseStatus = errors.New("thread-mode case lifecycle must change via board status")

	// Archive errors
	//
	// ErrCaseNotClosed is returned by ArchiveCase when the targeted case is not
	// CLOSED (it is OPEN or DRAFT). Archiving hides a case from the default
	// Cases list, the board, the dashboard and the Job scan at once; an OPEN
	// case vanishing from all of them gives an operator no way to see why, so
	// only a CLOSED case may be archived.
	ErrCaseNotClosed = errors.New("case is not closed")
	// ErrCaseAlreadyArchived is returned by ArchiveCase when the case is
	// already archived. Archiving is not silently idempotent so a caller can
	// distinguish "already done" from "now done"; BulkArchiveCases is what
	// treats it as a skip.
	ErrCaseAlreadyArchived = errors.New("case is already archived")
	// ErrCaseNotArchived is returned by UnarchiveCase when the case is not
	// archived.
	ErrCaseNotArchived = errors.New("case is not archived")
	// ErrCaseArchived is returned by ReopenCase / UpdateCaseStatus when the
	// targeted case is archived. An archived case that went back to OPEN would
	// appear in no list, no board, no dashboard and no Job scan — unreachable
	// state. Unarchive it first.
	ErrCaseArchived = errors.New("case is archived")
	// ErrMissingRequiredOnSubmit is returned by SubmitDraft when the draft
	// is missing one or more required custom fields. The wrapping goerr
	// carries the field IDs and human-friendly names (see MissingFieldIDsKey /
	// MissingFieldNamesKey) so the frontend can point the user at exactly
	// which inputs to fill.
	ErrMissingRequiredOnSubmit = errors.New("draft is missing required fields")
	// ErrDraftTitleRequired is returned by SubmitDraft when the draft was
	// saved with an empty title. Slack channel naming requires a non-empty
	// title, so this is enforced at promote time even though Save-as-Draft
	// accepted the empty value.
	ErrDraftTitleRequired = errors.New("draft title is required before submit")
	// ErrFieldValidationFailed wraps a field-level validation failure from
	// the workspace's field schema (option lookup, type coercion, etc.).
	// Used so the GraphQL layer can surface "fix the field" as a specific
	// code without grepping wrapped error messages.
	ErrFieldValidationFailed = errors.New("field validation failed")
	// ErrActivationFailed wraps a post-promotion activation failure (Slack
	// channel creation, channel invites, welcome message). The draft is
	// rolled back to DRAFT before this error returns, so callers / frontends
	// should advise "retry submit".
	ErrActivationFailed = errors.New("case activation failed")

	// ErrCasePrivateThreadModeUnsupported is returned by the case create flows
	// when isPrivate=true is requested for a thread-mode workspace. A private
	// case's only effect is a dedicated private Slack channel, which channel
	// mode creates and thread mode has no equivalent for (thread-mode cases
	// reuse the monitored channel and never carry IsPrivate). The frontend
	// already hides the private toggle in thread mode; the invariant is enforced
	// here at the usecase boundary so every entry point (GraphQL, Slack modal,
	// agent tool, import) is covered, not just the web form.
	ErrCasePrivateThreadModeUnsupported = errors.New("private case is not supported in thread mode")

	// ErrThreadModeSlackUnconfigured is returned when a thread-mode workspace
	// must bring a case to OPEN but the Slack service or the monitored channel
	// is not configured. Thread-mode cases live in the monitored channel's
	// thread and never provision a dedicated channel, so falling back to the
	// channel-mode activation path (which would call CreateChannel) is exactly
	// the invariant violation to avoid. Startup config validation normally
	// rejects a thread-mode workspace without a monitor channel, so this guards
	// direct WorkspaceEntry construction (tests / eval) and fails closed.
	ErrThreadModeSlackUnconfigured = errors.New("thread-mode workspace requires slack service and monitor channel")

	// ErrCaseThreadModeNoActions is returned by ActionUseCase write paths when
	// the parent (or reparent target) Case is thread-mode. Thread-mode cases
	// track progress through the configurable board status (Kanban) and have no
	// Actions — the configurable status attaches to the Case itself there, while
	// in channel mode it attaches to Actions (see model.Case.BoardStatus). The
	// invariant is enforced at the usecase boundary so every entry point
	// (GraphQL, Slack, agent tools, eval) is covered, not just the agent tool
	// wiring that withholds the action tools for thread-mode workspaces.
	ErrCaseThreadModeNoActions = errors.New("thread-mode case cannot have actions")

	// Existing Slack channel errors (case creation linking an existing channel)
	//
	// ErrExistingSlackChannelDisabled is returned when an existing channel is
	// requested (or its candidates listed) for a workspace where
	// ExistingSlackChannelAllowed is false.
	ErrExistingSlackChannelDisabled = errors.New("linking an existing slack channel is not enabled for this workspace")
	// ErrSlackChannelNotLinkable is returned when the requested channel cannot
	// become a case channel: it is a configured monitor / workspace channel,
	// is missing, archived or invisible to the bot, is private without the
	// bot, or the acting user is not a member.
	ErrSlackChannelNotLinkable = errors.New("slack channel cannot be linked to a case")
	// ErrSlackChannelAlreadyLinked is returned when a case in any workspace
	// already uses the requested channel.
	ErrSlackChannelAlreadyLinked = errors.New("slack channel is already linked to a case")
	// ErrSlackChannelJoinFailed is returned when the bot could not join the
	// requested public channel. Unlike the two errors above it can stem from
	// the deployment (a missing channels:join scope), so it is reported.
	ErrSlackChannelJoinFailed = errors.New("bot failed to join slack channel")

	// Action Slack-post state errors
	ErrSlackMessageAlreadyPosted = errors.New("action already has a Slack message")
	ErrCaseHasNoSlackChannel     = errors.New("parent case has no Slack channel")

	// Action archive state errors
	ErrActionAlreadyArchived = errors.New("action is already archived")
	ErrActionNotArchived     = errors.New("action is not archived")

	// Access control errors
	ErrAccessDenied = errors.New("access denied to private case")

	// Agent Job manual-trigger errors
	//
	// ErrJobNotFound is returned when a manual trigger names a Job that is
	// not in the Case's triggerable set — an unknown id, a Job disabled in
	// the workspace TOML, or a scheduled-only Job on a Case that is not
	// OPEN. The set is exactly what ListCaseJobs returns, so a Job the web
	// UI does not list cannot be started through the API either.
	ErrJobNotFound = errors.New("job not found")
	// ErrJobAlreadyRunning is returned when a manual trigger targets a
	// (workspace, case, job) tuple that already holds a live lease or is
	// suspended awaiting user input. The authoritative exclusion is the
	// lease taken inside the runner; this check exists so the operator gets
	// an immediate answer instead of a silent no-op.
	ErrJobAlreadyRunning = errors.New("job is already running")

	// Other errors
	ErrDuplicateField = errors.New("duplicate field")

	// ErrUnknownUser is returned by a case write when an assignee id or a
	// user / multi-user field value references a user that does not exist in
	// the SlackUser store. It guards against an agent (or API client)
	// persisting a hallucinated / mistyped user id. Slack sync delay is
	// treated as non-existence per project policy.
	ErrUnknownUser = errors.New("unknown user")

	// ErrInvalidArgument is returned by usecase methods when the caller
	// provides input that violates a domain invariant (unknown ID, list
	// element that does not belong to the workspace, etc.). Distinct from
	// ErrAccessDenied / ErrNotFound so the GraphQL layer can map it to
	// BAD_USER_INPUT rather than INTERNAL.
	ErrInvalidArgument = errors.New("invalid argument")
)

// Context keys for error values
const (
	CaseIDKey          = "case_id"
	ActionIDKey        = "action_id"
	ActionStepIDKey    = "action_step_id"
	ActionCommentIDKey = "action_comment_id"
	// MissingFieldIDsKey / MissingFieldNamesKey are populated on
	// ErrMissingRequiredOnSubmit so the GraphQL error mapper can expose the
	// list to the frontend (which renders them as the offending inputs).
	MissingFieldIDsKey   = "missing_field_ids"
	MissingFieldNamesKey = "missing_field_names"
	// CurrentStatusKey is populated on status-transition errors so the
	// frontend can tell the user the current state of the case (e.g. "this
	// draft has already been opened — refresh to see the change").
	CurrentStatusKey = "current_status"
)
