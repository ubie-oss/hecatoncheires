package usecase

import (
	"context"
	"time"

	"github.com/gollem-dev/agentkit"
	githubsvc "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/github"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	slackmodel "github.com/m-mizutani/hecatoncheires/pkg/domain/model/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/planexec"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/proposal"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/threadcase"
)

// SetMentionProposalWorkspaceAccessForTest replaces the authorizer a directly
// constructed MentionProposalUseCase holds (New wires it in production).
func SetMentionProposalWorkspaceAccessForTest(uc *MentionProposalUseCase, a interfaces.WorkspaceAuthorizer) {
	uc.workspaceAccess = a
}

// MentionProposalWorkspaceAccessForTest returns the authorizer the usecase
// holds, so a test can hand the durable host the same one.
func MentionProposalWorkspaceAccessForTest(uc *MentionProposalUseCase) interfaces.WorkspaceAuthorizer {
	return uc.workspaceAccess
}

// SetSlackWorkspaceAccessForTest replaces the authorizer a directly
// constructed SlackUseCases holds.
func SetSlackWorkspaceAccessForTest(uc *SlackUseCases, a interfaces.WorkspaceAuthorizer) {
	uc.workspaceAccess = a
}

// ThreadcaseAskQuestionForTest drives the thread-mode host's question delivery —
// the completion-handler path a durable run takes when its planner asks the user.
func ThreadcaseAskQuestionForTest(uc *AgentUseCase, ctx context.Context,
	target threadcase.Target, q threadcase.QuestionPayload,
) error {
	return threadcaseHost{uc: uc}.AskQuestion(ctx, target, q)
}

// ProposalAskForTest drives the case-draft host's question delivery.
func ProposalAskForTest(uc *MentionProposalUseCase, ctx context.Context,
	target proposal.Target, q proposal.QuestionPayload,
) error {
	return proposalHost{uc: uc}.Ask(ctx, target, q)
}

// CreateThreadBoundCaseForTest exposes the unexported createThreadBoundCase to
// external tests (package usecase_test). It replaces the former exported
// CreateThreadCase (deleted; test-only) and CreateThreadCaseWithFields
// (unexported). Callers that used the fields-less CreateThreadCase pass a nil
// fieldValues and an empty requestKey.
func (uc *CaseUseCase) CreateThreadBoundCaseForTest(ctx context.Context, workspaceID, channelID, threadTS, reporterID, title, description string, fieldValues map[string]model.FieldValue, requestKey string) (*model.Case, error) {
	return uc.createThreadBoundCase(ctx, workspaceID, channelID, threadTS, reporterID, title, description, fieldValues, requestKey)
}

// ProgressBlockForTest exposes progressBlock, the single context block a run's
// progress message is made of.
var ProgressBlockForTest = progressBlock

// NewAgentProgressForTest builds the planexec.Progress implementation the durable
// agents draw through, wired to the given usecase.
func NewAgentProgressForTest(uc *AgentUseCase) planexec.Progress {
	return agentProgress{uc: uc}
}

// PostProgressAckForTest invokes the unexported postProgressAck, which posts the
// create turn's progress message before the run is spawned.
func PostProgressAckForTest(uc *AgentUseCase, ctx context.Context, channelID, threadTS string) string {
	return uc.postProgressAck(ctx, channelID, threadTS)
}

// BuildCaseCreatedTailBlocksForTest is exported for testing
var BuildCaseCreatedTailBlocksForTest = buildCaseCreatedTailBlocks

// BuildPreviewBlocksForTest exposes buildPreviewBlocks so tests can assert
// on the localized notification fallback of the draft preview.
var BuildPreviewBlocksForTest = buildPreviewBlocks

// BuildProposalQuestionBlocksForTest exposes buildProposalQuestionBlocks so
// tests can assert on the localized notification fallback of the question form.
var BuildProposalQuestionBlocksForTest = buildProposalQuestionBlocks

// BuildThreadCreateQuestionBlocksForTest exposes buildThreadCreateQuestionBlocks
// so tests can assert on the localized notification fallback of the
// thread-mode question form.
var BuildThreadCreateQuestionBlocksForTest = buildThreadCreateQuestionBlocks

// BuildProposalUserInputForTest exposes the unexported buildProposalUserInput
// so tests in the external usecase_test package can assert on the
// planner's first-turn prompt content.
var BuildProposalUserInputForTest = buildProposalUserInput

// FirstSlackUserMentionForTest exposes firstSlackUserMention so tests can
// verify reporter extraction from a bot-relayed intake post.
var FirstSlackUserMentionForTest = firstSlackUserMention

// RenderReactionCreateInstructionForTest exposes renderReactionCreateInstruction
// so the reaction create-context prompt template can be golden-tested.
var RenderReactionCreateInstructionForTest = renderReactionCreateInstruction

// NormalizeReactionNameForTest exposes normalizeReactionName for skin-tone tests.
var NormalizeReactionNameForTest = normalizeReactionName

// EncodeCaseThreadValueForTest / ParseCaseThreadValueForTest expose the question
// Submit button value codec so its contract (and the legacy fallback) can be
// tested directly.
var EncodeCaseThreadValueForTest = encodeCaseThreadValue
var ParseCaseThreadValueForTest = parseCaseThreadValue

// BuildAssistSystemPrompt is exported for testing
var BuildAssistSystemPrompt = (*AssistUseCase).buildAssistSystemPrompt

// ProcessAssistCaseForTest spawns the assist run for one case and returns its
// Process id, so a test can read back what the run was spawned with.
var ProcessAssistCaseForTest = (*AssistUseCase).processCase

// SetAssistServeOptionsForTest overrides the Serve options the assist drain
// uses, so a test that drives a deliberately failing run does not have to wait
// out agentkit's default retry schedule.
func SetAssistServeOptionsForTest(uc *AssistUseCase, opts ...agentkit.ServeOption) {
	uc.serveOpts = opts
}

// AssistPromptData is exported for testing template rendering
type AssistPromptData = assistPromptData
type AssistPromptAction = assistPromptAction
type AssistPromptMessage = assistPromptMessage
type AssistPromptAssistLog = assistPromptAssistLog

// TestErrAccessDenied is exported for testing
var TestErrAccessDenied = ErrAccessDenied

// Case write access-control helpers exposed for testing (see case_access.go).
var (
	AssertCaseWriteAccessForTest = assertCaseWriteAccess
	TokenActorForTest            = tokenActor
	LoadCaseForWriteForTest      = loadCaseForWrite
	ActorForAccessForTest        = actorForAccess
)

// NewWelcomeRendererForTest is exported for testing
var NewWelcomeRendererForTest = newWelcomeRenderer

// BuildWelcomeFieldsForTest is exported for testing
var BuildWelcomeFieldsForTest = buildWelcomeFields

// RenderSummaryFieldsForTest exposes the thread-mode create summary's field
// lines.
var RenderSummaryFieldsForTest = renderSummaryFields

// BuildThreadCaseSummaryBlocksForTest exposes the thread-mode create summary
// builder.
var BuildThreadCaseSummaryBlocksForTest = buildThreadCaseSummaryBlocks

// BuildFieldPairSectionsForTest exposes the mention draft preview's field
// section builder.
var BuildFieldPairSectionsForTest = buildFieldPairSections

// WelcomeContextForTest is exported for testing
type WelcomeContextForTest = welcomeContext

// WelcomeRendererRenderForTest invokes the (unexported) Render method on a
// welcomeRenderer for tests in the external package.
func WelcomeRendererRenderForTest(r *welcomeRenderer, ctx welcomeContext) ([]string, error) {
	return r.Render(ctx)
}

// ShouldBroadcastActionEventForTest exposes shouldBroadcastActionEvent
// so external tests can verify the broadcast set membership.
var ShouldBroadcastActionEventForTest = shouldBroadcastActionEvent

// ShouldBroadcastAnyActionEventForTest exposes shouldBroadcastAnyActionEvent
// so external tests can verify multi-kind broadcast judgement.
var ShouldBroadcastAnyActionEventForTest = shouldBroadcastAnyActionEvent

// ClampPlainTextForTest exposes the unexported clampPlainText helper so
// external tests can verify the Slack input-length contract.
var ClampPlainTextForTest = clampPlainText

// CommentExcerptForTest exposes the unexported commentExcerpt helper so
// external tests can verify the whitespace-collapse / rune-boundary
// truncation / mrkdwn-escape contract of the Slack notification preview.
var CommentExcerptForTest = commentExcerpt

// ActionCommentBroadcastsForTest exposes the channel-visibility decision for
// Action comment notifications so an external test can pin it.
const ActionCommentBroadcastsForTest = actionCommentBroadcasts

// ClampSlackOptionDescriptionForTest exposes the unexported
// clampSlackOptionDescription helper so external tests can verify the
// 75-rune option-description contract.
var ClampSlackOptionDescriptionForTest = clampSlackOptionDescription

// SlackOptionDescriptionMaxRunesForTest exposes the 75-rune ceiling for
// assertions.
const SlackOptionDescriptionMaxRunesForTest = slackOptionDescriptionMaxRunes

// BuildFieldOptionsForTest exposes buildFieldOptions so external tests can
// assert that long option descriptions are clamped before being handed to
// Slack.
var BuildFieldOptionsForTest = buildFieldOptions

// IsLikelySlackUserIDForTest exposes the unexported isLikelySlackUserID
// helper so external tests can verify the user-ID syntactic gate.
var IsLikelySlackUserIDForTest = isLikelySlackUserID

// FilterSlackUserIDsForTest exposes the unexported filterSlackUserIDs
// helper so external tests can verify the slice filtering behaviour.
var FilterSlackUserIDsForTest = filterSlackUserIDs

// SlackPlainTextMaxRunesForTest exposes the clamp ceiling for assertions.
const SlackPlainTextMaxRunesForTest = slackPlainTextMaxRunes

// ClampSuffixMultiLineForTest exposes the multiline truncation sentinel.
const ClampSuffixMultiLineForTest = clampSuffixMultiLine

// ClampSuffixSingleLineForTest exposes the single-line truncation sentinel.
const ClampSuffixSingleLineForTest = clampSuffixSingleLine

// BuildDraftEditModalForTest exposes buildDraftEditModal so external tests
// can assert on the rendered Block Kit payload.
var BuildDraftEditModalForTest = buildDraftEditModal

// BuildFieldInputBlockForTest / BuildFieldInputBlockWithValueForTest expose the
// custom-field Slack input builders so external tests can assert per-type
// element shape (e.g. markdown → multiline plain-text input).
var (
	BuildFieldInputBlockForTest          = buildFieldInputBlock
	BuildFieldInputBlockWithValueForTest = buildFieldInputBlockWithValue
)

// Mention-draft Edit modal block / action IDs exposed for tests that
// build the view_submission callback directly (e.g. the reporter
// recording test for HandleEditSubmit).
const (
	BlockIDDraftEditTitleForTest        = blockIDDraftEditTitle
	ActionIDDraftEditTitleForTest       = actionIDDraftEditTitle
	BlockIDDraftEditDescriptionForTest  = blockIDDraftEditDescription
	ActionIDDraftEditDescriptionForTest = actionIDDraftEditDescription
	BlockIDDraftEditTestForTest         = blockIDDraftEditTest
	ActionIDDraftEditTestForTest        = actionIDDraftEditTest
	CaseOptionValueTestForTest          = caseOptionValueTest
)

// NotificationSlotCoordinatorForTest is the test-only alias for the
// unexported notificationSlotCoordinator. External tests treat values of
// this type as opaque and exercise behaviour through the *ForTest helpers
// below.
type NotificationSlotCoordinatorForTest = notificationSlotCoordinator

// NewNotificationSlotCoordinatorForTest constructs a coordinator with an
// injectable clock so the test can drive slot expiry deterministically.
func NewNotificationSlotCoordinatorForTest(
	repo interfaces.NotificationSlotRepository,
	slackService slack.Service,
	slotDuration time.Duration,
	now func() time.Time,
) *NotificationSlotCoordinatorForTest {
	return newNotificationSlotCoordinator(repo, slackService, slotDuration, now)
}

// SlotEntryForTest is the test-facing alias for the coordinator's input
// struct so external tests can describe events without reaching into the
// unexported notification_slot.go internals.
type SlotEntryForTest = slotEntry

// EnqueueChannelLineForTest invokes the unexported enqueueChannelLine.
func EnqueueChannelLineForTest(c *NotificationSlotCoordinatorForTest, ctx context.Context, channelID string, entry SlotEntryForTest) {
	c.enqueueChannelLine(ctx, channelID, entry)
}

// NotificationSlotCoordinatorEnabledForTest exposes the enabled() probe.
func NotificationSlotCoordinatorEnabledForTest(c *NotificationSlotCoordinatorForTest) bool {
	return c.enabled()
}

// BuildSlotBlocksForTest exposes the Block Kit renderer for unit tests.
var BuildSlotBlocksForTest = buildSlotBlocks

// User-facing error classification / rendering seams (see uierr.go).
var (
	ClassifyUserErrorForTest    = classifyUserError
	UnexpectedUserFacingForTest = unexpectedUserFacing
	PrepareUserErrorForTest     = prepareUserError
	FallbackReasonErrorForTest  = fallbackReasonError
)

// Home dashboard prompt seams (see dashboard.go) exposed so the greeting
// template can be golden-tested and the language allow-list verified.
type HomeMessagePromptInputForTest = homeMessagePromptInput

var RenderHomeMessagePromptForTest = renderHomeMessagePrompt
var NormalizeHomeMessageLangForTest = normalizeHomeMessageLang

// Type aliases for testing
type GitHubPullRequest = githubsvc.PullRequest
type GitHubIssue = githubsvc.Issue
type GitHubIssueWithComments = githubsvc.IssueWithComments
type GitHubComment = githubsvc.Comment
type GitHubReview = githubsvc.Review
type SlackMessage = slackmodel.Message
type SlackChannel = model.SlackChannel
type ConversationMessage = slack.ConversationMessage
