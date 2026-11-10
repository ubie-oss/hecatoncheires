package i18n

import (
	"context"
	"fmt"
	"strings"

	"github.com/m-mizutani/goerr/v2"
)

// The context holds two languages in separate keys. Keeping them apart makes
// the result independent of write order: several Slack paths set the user's
// locale again after the workspace was resolved (the access-denied notices,
// for one), and a single overwritten value would let that write discard the
// workspace's setting.
type (
	userLangKey      struct{}
	workspaceLangKey struct{}
)

// ContextWithLang sets the language of the person the output is addressed to:
// the Slack user's locale, or the language an agent run saved at spawn.
func ContextWithLang(ctx context.Context, lang Lang) context.Context {
	return context.WithValue(ctx, userLangKey{}, lang)
}

// ContextWithWorkspaceLang sets the workspace's configured language, which
// LangFromContext prefers over the language set by ContextWithLang. An empty
// lang returns ctx unchanged, so a caller can pass the value of a workspace
// that configures none.
func ContextWithWorkspaceLang(ctx context.Context, lang Lang) context.Context {
	if lang == "" {
		return ctx
	}
	return context.WithValue(ctx, workspaceLangKey{}, lang)
}

// LangFromContext returns the workspace language when set, otherwise the
// language set by ContextWithLang, otherwise an empty string.
func LangFromContext(ctx context.Context) Lang {
	if lang, ok := ctx.Value(workspaceLangKey{}).(Lang); ok && lang != "" {
		return lang
	}
	if lang, ok := ctx.Value(userLangKey{}).(Lang); ok {
		return lang
	}
	return ""
}

// Lang represents a supported language.
type Lang string

const (
	LangEN Lang = "en"
	LangJA Lang = "ja"
)

// MsgKey is an iota-based enum for translation keys.
type MsgKey int

const (
	// Slash Command Modal
	MsgModalCreateCaseTitle MsgKey = iota
	MsgModalCreateCaseSubmit
	MsgModalCreateCaseCancel
	MsgModalNextButton
	MsgFieldTitle
	MsgFieldDescription
	MsgFieldTitlePlaceholder
	MsgFieldDescPlaceholder
	MsgFieldWorkspace

	// Case creation confirmation
	MsgCaseCreated            // "Case #%d *%s* has been created."
	MsgCaseCreatedWithChannel // "Case #%d *%s* has been created. Channel: <#%s>"

	// Action notifications
	MsgActionHeader // "Action: %s %s"
	MsgActionAssignToMe
	MsgActionInProgress
	MsgActionCompleted
	MsgActionNoAssign
	MsgActionStatus // "Status: %s"

	// Action interactive controls (Slack Block Kit)
	MsgActionOpenInWeb
	MsgActionStatusPlaceholder
	MsgActionAssigneePlaceholder

	// Action change notifications (Slack thread)
	MsgActionCreated                  // ":heavy_plus_sign: %s created action %s" (thread-mode case thread)
	MsgActionChangeTitle              // ":pencil2: %s changed title: %q -> %q"
	MsgActionChangeStatus             // ":arrows_counterclockwise: %s changed status: %s -> %s"
	MsgActionChangeAssigneeAssigned   // ":bust_in_silhouette: %s assigned %s"
	MsgActionChangeAssigneeUnassigned // ":bust_in_silhouette: %s unassigned %s"
	MsgActionChangeAssigneeReplaced   // ":bust_in_silhouette: %s changed assignee: %s -> %s"
	MsgActionChangeArchived           // ":file_cabinet: %s archived action %q"
	MsgActionChangeUnarchived         // ":outbox_tray: %s unarchived action %q"
	MsgChangeActorSystem              // "system" (shared by action and case change notifications)
	MsgActionStepAdded                // ":heavy_plus_sign: %s added step %q"
	MsgActionStepRemoved              // ":heavy_minus_sign: %s removed step %q"
	MsgActionStepDone                 // ":white_check_mark: %s completed step %q"
	MsgActionStepReopened             // ":arrow_backward: %s reopened step %q"
	MsgActionStepRenamed              // ":pencil2: %s renamed step %q -> %q"
	MsgActionCommentAdded             // ":speech_balloon: %s commented: <%s|Open comment>"
	MsgActionCommentAddedNoLink       // ":speech_balloon: %s commented"

	// Agent
	MsgAgentThinking
	MsgAgentAnalyzing
	MsgAgentProcessing
	MsgAgentInvestigating
	MsgAgentLookingInto
	MsgAgentOnIt
	MsgAgentError
	MsgAgentSessionInfo
	// MsgKeyAgentBusy is used when a new mention arrives while a previous
	// turn on the same Slack thread is still running. The host posts this
	// reply so the user knows the agent is occupied and will respond once
	// the prior turn finishes.
	MsgKeyAgentBusy

	// Thread-mode case messages. Posted to the monitored channel's thread.
	MsgThreadCaseCreated  // "🧵 Case registered. <%s|Open in web UI>" (url)
	MsgThreadCaseUpdated  // ":pencil2: Updated the case details."
	MsgThreadCaseClosed   // ":white_check_mark: Closed this case (%s)." (status name)
	MsgThreadCaseQuestion // ":question: %s" (reason) — question header line

	// Thread-mode case initialization (create) flow.
	MsgThreadCaseCreating       // "🔍 Got it — looking into this…" (initial progress)
	MsgThreadCaseCreateFallback // "I couldn't pull this together into a case yet. Add a little more detail and mention me again."
	MsgThreadCaseSummaryHeader  // "✅ Created a case" (Block Kit summary header)
	MsgThreadCaseSummaryTitle   // "Title" (field label)
	MsgThreadCaseSummaryDesc    // "Description" (field label)
	MsgThreadCaseSummaryStatus  // "Status" (field label)
	MsgThreadCaseSummaryLink    // "<%s|Open in web UI>" (url)
	// One-line summary for [slack] case_summary = "link".
	MsgThreadCaseSummaryLine       // "%s: <%s|%s>" (header, url, title)
	MsgThreadCaseSummaryLineNoLink // "%s: %s" (header, title) — variant when no base URL is configured

	// Reaction-triggered (cross-channel) case creation.
	MsgReactionCasePlaceholder  // "🔍 Creating a case…" — placeholder root posted before creation; replaced in place by the case summary
	MsgReactionCaseOrigin       // "Reported by <@%s> · <%s|source message>" (reporter user id, source message permalink) — posted as a reply under the summary root
	MsgReactionCaseOriginNoLink // "Reported by <@%s>" (reporter user id) — origin reply variant when the source permalink is unavailable
	MsgReactionCaseBacklink     // ":white_check_mark: Case created: <%s|Open in web UI> · <%s|View thread>" (case url, thread permalink)

	// MsgProposalProcessingCompleted replaces the initial "⏳ Drafting…"
	// placeholder once Materialize has posted the preview at the thread
	// end. It tells the user the placeholder is no longer the live status
	// and to look at the new preview message further down the thread.
	MsgProposalProcessingCompleted

	// Bookmark
	MsgBookmarkOpenCase

	// Cross-workspace
	MsgCrossWorkspaceConnectUnavailable

	// Edit Case Modal
	MsgModalEditCaseTitle
	MsgModalEditCaseSubmit
	MsgCaseUpdated
	MsgErrCaseNotAccessible

	// Private case
	MsgFieldPrivateCase
	MsgFieldPrivateCaseDesc

	// Case-creation "Options" checkbox group (parent label and
	// the additional Draft-mode option that sits alongside Private)
	MsgFieldCaseOptions   // Block label "Options"
	MsgFieldDraftMode     // Option label "Draft mode"
	MsgFieldDraftModeDesc // Description for the Draft mode option
	MsgFieldTestCase      // Option label "Test case"
	MsgFieldTestCaseDesc  // Description for the Test case option

	// Case assignees field
	MsgFieldCaseAssignees

	// Errors
	MsgErrOpenDialog
	MsgErrWorkspaceSelection
	MsgErrCreateCase
	MsgErrEditCase

	// Command choice modal (case channel /cmd without subcommand)
	MsgModalCommandChoiceTitle
	MsgFieldCommandChoice
	MsgChoiceUpdateCase
	MsgChoiceCreateAction

	// Action creation modal
	MsgModalCreateActionTitle
	MsgModalCreateActionSubmit
	MsgFieldAction // "Action" label
	MsgFieldActionTitle
	MsgFieldActionTitlePlaceholder
	MsgFieldActionDescription
	MsgFieldActionDescPlaceholder
	MsgFieldActionAssignee
	MsgFieldActionStatusLabel
	MsgFieldActionDueDate

	// Errors related to commands
	MsgErrUnknownSubcommand // "Unknown subcommand: %s. Available: update, action."
	MsgErrCreateAction      // "Failed to create action. Please try again."

	// Save-as-Draft (Case creation modal)
	MsgDraftSaveAsButton           // Button label "Save as draft"
	MsgDraftSavedEphemeral         // "Saved as draft #%d. Open the Drafts page on the web to continue." (%d = case ID). Used when no web URL is configured.
	MsgDraftSavedEphemeralWithLink // "Saved as draft #%d. <%s|%s> to continue." (%d = case ID, %s = draft URL, %s = link label). Preferred when baseURL is set.
	MsgDraftLinkFallbackLabel      // Link label used when the draft has no title; e.g. "Open the draft on the web"
	MsgDraftSavedModalTitle        // Title of the splash modal shown after Save as Draft.
	MsgDraftSavedModalBody         // Body text of the splash modal: "Saved as draft #%d." (%d = case ID)
	MsgDraftSaveFailedEphemeral    // Error ephemeral when Save as Draft fails server-side.

	// Job run session log (Slack thread). Posted by JobRunner around each
	// Job run to consolidate the run's operational log into one thread.
	MsgJobRunStarting  // "starting... `%s`" (%s = job id)
	MsgJobRunCompleted // "✅ job `%s` completed" (%s = job id)
	MsgJobRunFailed    // "❌ job `%s` failed: %s" (%s = job id, %s = error)

	// MsgAgentMentionRunName is the display name shown on the case agent page
	// for a mention-triggered agent run (a JobRunLog recorded under the
	// reserved mention JobID). Real Job runs use their TOML name; this labels
	// the mention pseudo-job.
	MsgAgentMentionRunName

	// Agent mention-draft preview & Edit modal (pkg/usecase/slack_mention_*).
	MsgMentionPreviewFallback // Notification fallback text for the preview ephemeral.
	MsgMentionPreviewTestNote // Context note shown on the preview when the draft is a test case.
	MsgMentionWorkspaceLabel  // Workspace selector placeholder/label on the preview.
	MsgMentionEditWorkspace   // "*Workspace*: %s ..." heading in the Edit modal (%s = workspace name).
	MsgMentionBtnSubmit       // "Submit" button (preview + Edit modal).
	MsgMentionBtnEdit         // "Edit" button on the preview.
	MsgMentionBtnCancel       // "Cancel" button on the preview.
	MsgMentionEditModalTitle  // Title of the draft Edit modal.
	MsgMentionSubmitFailed    // Appended to the preview fallback when case creation failed and the user should Edit.

	// Slack notification fallback texts (chat.postMessage / chat.update
	// fallback argument: notification preview & accessibility text).
	MsgMentionCanceledFallback         // Fallback for the "draft canceled" tail message.
	MsgMentionQuestionFallback         // Fallback for the mention-draft question form.
	MsgMentionPreviewFallbackWithTitle // "Case draft: %s" (%s = draft title).
	MsgCaseCreatedFallback             // "Created case #%d: %s" (%d = case ID, %s = title).
	MsgThreadCaseQuestionFallback      // Fallback for the thread-create question form.

	// Agent question forms (Job / mention-draft / thread-create).
	MsgQuestionFormUnanswered // Ephemeral sent when a submit leaves questions blank (%s = bulleted question list).

	// User-facing error messages (pkg/utils/uierr + pkg/usecase/uierr.go).
	// Every failure surfaced to a Slack user is rendered as a fixed 3-part
	// frame: "⚠️ {What}" / "*{LabelDetail}*: {Detail}[ (cause)]" /
	// "*{LabelFix}*: {Remediation}" / "ref: {id}". Each category below supplies
	// its What/Detail/Fix; the two labels are shared. All are arg-free — the
	// variable cause (Slack error code, field names, truncated reason) is
	// carried in UserFacing.Cause and appended by the renderer.
	MsgUIErrLabelDetail // "Technical note"
	MsgUIErrLabelFix    // "What you can do"

	MsgUIErrNotInChannelWhat // bot not a member of the destination channel
	MsgUIErrNotInChannelDetail
	MsgUIErrNotInChannelFix
	MsgUIErrMissingScopeWhat // Slack app lacks a required OAuth scope
	MsgUIErrMissingScopeDetail
	MsgUIErrMissingScopeFix
	MsgUIErrSlackAuthWhat // Slack token invalid / revoked / inactive
	MsgUIErrSlackAuthDetail
	MsgUIErrSlackAuthFix
	MsgUIErrConfigWhat // workspace configuration problem
	MsgUIErrConfigDetail
	MsgUIErrConfigFix
	MsgUIErrAccessDeniedWhat // caller not permitted (private case)
	MsgUIErrAccessDeniedDetail
	MsgUIErrAccessDeniedFix
	MsgUIErrWorkspaceAccessDeniedWhat // workspace [authz] policy denied the caller
	MsgUIErrWorkspaceAccessDeniedDetail
	MsgUIErrWorkspaceAccessDeniedFix
	MsgUIErrNoAccessibleWorkspaceWhat // no workspace configured, or none allows the caller
	MsgUIErrNoAccessibleWorkspaceDetail
	MsgUIErrNoAccessibleWorkspaceFix
	MsgWorkspaceAccessDeniedModalTitle // Title of the modal a denied view_submission is replaced with.
	MsgUIErrFieldValidationWhat        // required/invalid case fields
	MsgUIErrFieldValidationDetail
	MsgUIErrFieldValidationFix
	MsgUIErrAgentNoConclusionWhat // planner exhausted budget / gave up
	MsgUIErrAgentNoConclusionDetail
	MsgUIErrAgentNoConclusionFix
	MsgUIErrStorageWhat // Firestore read/write failure
	MsgUIErrStorageDetail
	MsgUIErrStorageFix
	MsgUIErrRateLimitedWhat // Slack rate limit
	MsgUIErrRateLimitedDetail
	MsgUIErrRateLimitedFix
	MsgUIErrSlackGenericWhat // other Slack API failure
	MsgUIErrSlackGenericDetail
	MsgUIErrSlackGenericFix
	MsgUIErrUnexpectedWhat // unclassified internal error (bug)
	MsgUIErrUnexpectedDetail
	MsgUIErrUnexpectedFix

	// MsgWorkspaceAgentFallback is posted when the workspace-channel agent turn
	// ends in a fallback (loop budget exhausted or internal error) with no reply.
	MsgWorkspaceAgentFallback

	// Case change notifications (thread-mode Slack thread)
	MsgCaseChangeTitle              // ":pencil2: %s changed the case title: %q -> %q"
	MsgCaseChangeStatus             // ":arrows_counterclockwise: %s changed the case status: %s -> %s"
	MsgCaseChangeAssigneeAssigned   // ":bust_in_silhouette: %s assigned %s"
	MsgCaseChangeAssigneeUnassigned // ":bust_in_silhouette: %s unassigned %s"
	MsgCaseChangeArchived           // ":file_cabinet: %s archived this case"
	MsgCaseChangeUnarchived         // ":outbox_tray: %s unarchived this case"

	msgKeyCount // sentinel for validation
)

var (
	messages    map[Lang][msgKeyCount]string
	defaultLang Lang
)

// Init initializes the global translator with the given default language.
// Must be called once at startup before any T() calls.
//
// The translation tables themselves are populated by package init() in
// messages.go — calling Init() only updates the default language fallback,
// so T() is safe to call from tests without an explicit Init().
func Init(lang Lang) {
	defaultLang = lang
}

// T returns the translated string for the language in context.
// Falls back to defaultLang if no language is set in context.
func T(ctx context.Context, key MsgKey, args ...any) string {
	lang := LangFromContext(ctx)
	msg := messages[lang][key]
	if msg == "" {
		msg = messages[defaultLang][key]
	}
	if msg == "" {
		return fmt.Sprintf("[missing:%d]", key)
	}
	if len(args) > 0 {
		return fmt.Sprintf(msg, args...)
	}
	return msg
}

// DefaultLang returns the configured default language.
func DefaultLang() Lang {
	return defaultLang
}

// DetectLang returns the Lang for a Slack locale string.
// Returns empty string if no match (caller should fall back to defaultLang).
func DetectLang(slackLocale string) Lang {
	if strings.HasPrefix(slackLocale, "ja") {
		return LangJA
	}
	if strings.HasPrefix(slackLocale, "en") {
		return LangEN
	}
	return ""
}

// LanguageLabel resolves the active language from ctx (falling back to the
// configured default) and returns its English name, which is what an agent
// runtime embeds in the "write user-facing copy in X" directive of a prompt.
// Returns empty when neither ctx nor the default names a supported language,
// which the prompts read as "no directive".
//
// It lives here rather than in each agent host because a host that forgets to
// fill it has no symptom other than replies arriving in the wrong language: the
// run succeeds, nothing logs, and only a reader of the thread notices. Three
// hosts fill the same field, so the mapping is shared to keep them from
// diverging.
func LanguageLabel(ctx context.Context) string {
	lang := LangFromContext(ctx)
	if lang == "" {
		lang = DefaultLang()
	}
	return LanguageLabelOf(lang)
}

// LanguageLabelOf returns the English name of lang, the form a prompt's
// language directive is written with, or an empty string for a language this
// package does not support. Unlike LanguageLabel it applies no fallback: a
// host passing a workspace's configured language gets an empty result when the
// workspace configures none.
func LanguageLabelOf(lang Lang) string {
	switch lang {
	case LangJA:
		return "Japanese"
	case LangEN:
		return "English"
	default:
		return ""
	}
}

// ParseLang validates and returns a Lang from a string (for CLI flags).
func ParseLang(s string) (Lang, error) {
	switch Lang(s) {
	case LangEN:
		return LangEN, nil
	case LangJA:
		return LangJA, nil
	default:
		return "", goerr.New("unsupported language", goerr.V("lang", s))
	}
}
