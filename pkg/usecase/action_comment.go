package usecase

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

// commentExcerptMaxRunes caps the one-line preview embedded in the Slack
// notification. A comment is read in the WebUI; the channel only needs enough
// text to decide whether to follow the link, and a longer excerpt would leak
// the body of a private Case's comment into the parent channel view.
const commentExcerptMaxRunes = 100

// ActionCommentUseCase orchestrates Web-UI-authored comments on an Action:
// load the parent Action and Case, enforce private-Case access control,
// enforce author-only edit / delete, persist the comment, and announce a
// creation in the Action's Slack thread.
//
// A comment is never reproduced as a Slack message. The notification carries
// the author, a deep link and a short excerpt only, so the Slack thread does
// not become a second copy of the conversation.
type ActionCommentUseCase struct {
	repo         interfaces.Repository
	registry     *model.WorkspaceRegistry
	slackService slack.Service
	slotCoord    *notificationSlotCoordinator
	baseURL      string
}

// NewActionCommentUseCase constructs the ActionCommentUseCase. slackService and
// slotCoord may be nil; when slackService is nil the notification is skipped
// entirely, and when slotCoord is nil (or its slotDuration is non-positive) the
// channel-side notification falls back to reply_broadcast on the thread post.
// baseURL may be empty, in which case the notification omits the deep link.
// registry supplies the workspace's output language for the notification and
// may be nil (the notification then follows the context's language).
func NewActionCommentUseCase(
	repo interfaces.Repository,
	registry *model.WorkspaceRegistry,
	slackService slack.Service,
	baseURL string,
	slotCoord *notificationSlotCoordinator,
) *ActionCommentUseCase {
	return &ActionCommentUseCase{
		repo:         repo,
		registry:     registry,
		slackService: slackService,
		slotCoord:    slotCoord,
		baseURL:      baseURL,
	}
}

// CreateActionCommentInput is the input for ActionCommentUseCase.Create.
type CreateActionCommentInput struct {
	WorkspaceID string
	ActionID    int64
	Body        string
}

// Validate enforces the caller-supplied invariants before any work is done.
func (in CreateActionCommentInput) Validate() error {
	if in.WorkspaceID == "" {
		return goerr.Wrap(ErrInvalidArgument, "workspace ID is required")
	}
	if in.ActionID <= 0 {
		return goerr.Wrap(ErrInvalidArgument, "action ID is required",
			goerr.V(ActionIDKey, in.ActionID))
	}
	return validateActionCommentBody(in.Body)
}

// UpdateActionCommentInput is the input for ActionCommentUseCase.Update.
type UpdateActionCommentInput struct {
	WorkspaceID string
	ActionID    int64
	CommentID   string
	Body        string
}

// Validate enforces the caller-supplied invariants before any work is done.
func (in UpdateActionCommentInput) Validate() error {
	if in.WorkspaceID == "" {
		return goerr.Wrap(ErrInvalidArgument, "workspace ID is required")
	}
	if in.ActionID <= 0 {
		return goerr.Wrap(ErrInvalidArgument, "action ID is required",
			goerr.V(ActionIDKey, in.ActionID))
	}
	if in.CommentID == "" {
		return goerr.Wrap(ErrInvalidArgument, "action comment ID is required")
	}
	return validateActionCommentBody(in.Body)
}

// DeleteActionCommentInput is the input for ActionCommentUseCase.Delete.
type DeleteActionCommentInput struct {
	WorkspaceID string
	ActionID    int64
	CommentID   string
}

// Validate enforces the caller-supplied invariants before any work is done.
func (in DeleteActionCommentInput) Validate() error {
	if in.WorkspaceID == "" {
		return goerr.Wrap(ErrInvalidArgument, "workspace ID is required")
	}
	if in.ActionID <= 0 {
		return goerr.Wrap(ErrInvalidArgument, "action ID is required",
			goerr.V(ActionIDKey, in.ActionID))
	}
	if in.CommentID == "" {
		return goerr.Wrap(ErrInvalidArgument, "action comment ID is required")
	}
	return nil
}

// validateActionCommentBody checks the body the same way for create and update.
// The trimmed form is what gets persisted, so the cap is checked against it.
func validateActionCommentBody(body string) error {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return goerr.Wrap(ErrInvalidArgument, "action comment body is required")
	}
	if len(trimmed) > model.ActionCommentBodyMaxLen {
		return goerr.Wrap(ErrInvalidArgument, "action comment body exceeds max length",
			goerr.V("len", len(trimmed)),
			goerr.V("max", model.ActionCommentBodyMaxLen))
	}
	return nil
}

// Create persists a new comment on the Action and announces it in the Action's
// Slack thread. The author is taken from the request's auth token, never from
// the caller's input.
func (uc *ActionCommentUseCase) Create(ctx context.Context, in CreateActionCommentInput) (*model.ActionComment, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	authorID, err := commentAuthor(ctx)
	if err != nil {
		return nil, err
	}
	action, caseModel, err := uc.loadActionForCommentWrite(ctx, in.WorkspaceID, in.ActionID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	comment := &model.ActionComment{
		ID:        uuid.NewString(),
		ActionID:  action.ID,
		AuthorID:  authorID,
		Body:      strings.TrimSpace(in.Body),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := uc.repo.ActionComment().Create(ctx, in.WorkspaceID, action.ID, comment); err != nil {
		return nil, goerr.Wrap(err, "failed to create action comment",
			goerr.V(ActionIDKey, in.ActionID))
	}

	// Notify only after the write landed: a link to a comment that does not
	// exist is worse than no notification at all.
	uc.notifyCommentCreated(ctx, in.WorkspaceID, action, caseModel, comment)

	return comment, nil
}

// Update rewrites the body of the caller's own comment. Slack is deliberately
// not notified: the channel was already told a comment exists, and re-posting
// on every edit is the "reproduce the conversation in Slack" behaviour this
// feature exists to avoid.
func (uc *ActionCommentUseCase) Update(ctx context.Context, in UpdateActionCommentInput) (*model.ActionComment, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	existing, err := uc.loadOwnComment(ctx, in.WorkspaceID, in.ActionID, in.CommentID)
	if err != nil {
		return nil, err
	}

	body := strings.TrimSpace(in.Body)
	if body == existing.Body {
		// No write, so UpdatedAt does not move and the row is not marked
		// edited for a no-op save.
		return existing, nil
	}

	existing.Body = body
	existing.UpdatedAt = time.Now().UTC()
	if err := uc.repo.ActionComment().Update(ctx, in.WorkspaceID, in.ActionID, existing); err != nil {
		// The comment was deleted between the read above and this write (the
		// same author editing in one tab and deleting in another). Report it as
		// not found rather than resurrecting it.
		if errors.Is(err, interfaces.ErrActionCommentNotFound) {
			return nil, goerr.Wrap(ErrActionCommentNotFound, "action comment not found",
				goerr.V(ActionIDKey, in.ActionID),
				goerr.V(ActionCommentIDKey, in.CommentID))
		}
		return nil, goerr.Wrap(err, "failed to update action comment",
			goerr.V(ActionIDKey, in.ActionID),
			goerr.V(ActionCommentIDKey, in.CommentID))
	}
	return existing, nil
}

// Delete removes the caller's own comment. Slack is not notified, for the same
// reason as Update.
func (uc *ActionCommentUseCase) Delete(ctx context.Context, in DeleteActionCommentInput) error {
	if err := in.Validate(); err != nil {
		return err
	}
	if _, err := uc.loadOwnComment(ctx, in.WorkspaceID, in.ActionID, in.CommentID); err != nil {
		return err
	}

	if err := uc.repo.ActionComment().Delete(ctx, in.WorkspaceID, in.ActionID, in.CommentID); err != nil {
		return goerr.Wrap(err, "failed to delete action comment",
			goerr.V(ActionIDKey, in.ActionID),
			goerr.V(ActionCommentIDKey, in.CommentID))
	}
	return nil
}

// List returns the Action's comments newest first. A caller that cannot access
// the parent Case gets an empty list rather than an error, matching the
// Action.messages / Action.events / Action.steps read behaviour.
func (uc *ActionCommentUseCase) List(ctx context.Context, workspaceID string, actionID int64, limit int, cursor string) ([]*model.ActionComment, string, error) {
	ok, err := uc.canRead(ctx, workspaceID, actionID)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return []*model.ActionComment{}, "", nil
	}
	comments, nextCursor, err := uc.repo.ActionComment().List(ctx, workspaceID, actionID, limit, cursor)
	if err != nil {
		return nil, "", goerr.Wrap(err, "failed to list action comments",
			goerr.V(ActionIDKey, actionID))
	}
	return comments, nextCursor, nil
}

// commentAuthor returns the Slack user id that authors the comment. Comments
// are human-authored by definition, so a context with no auth token is
// rejected instead of writing an unattributable comment. Every GraphQL request
// carries a token (the /graphql route always mounts authMiddleware, and
// no-auth mode injects a token for its configured user), so this is a
// fail-closed guard against a non-transport caller, not a reachable user error.
func commentAuthor(ctx context.Context) (string, error) {
	token, err := auth.TokenFromContext(ctx)
	if err != nil || token.Sub == "" {
		return "", goerr.Wrap(ErrInvalidArgument, "action comment requires an authenticated author")
	}
	return token.Sub, nil
}

// loadActionForCommentWrite loads the parent Action and Case and routes the
// private-Case decision through the shared gate in case_access.go, so a new
// comment write path cannot structurally miss the check.
func (uc *ActionCommentUseCase) loadActionForCommentWrite(ctx context.Context, workspaceID string, actionID int64) (*model.Action, *model.Case, error) {
	action, err := uc.repo.Action().Get(ctx, workspaceID, actionID)
	if err != nil {
		return nil, nil, goerr.Wrap(ErrActionNotFound, "action not found", goerr.V(ActionIDKey, actionID))
	}

	parentCase, err := uc.repo.Case().Get(ctx, workspaceID, action.CaseID)
	if err != nil {
		return nil, nil, goerr.Wrap(err, "failed to get parent case", goerr.V(CaseIDKey, action.CaseID))
	}

	actorID, checkAccess := tokenActor(ctx)
	if err := assertCaseWriteAccess(parentCase, actorID, checkAccess); err != nil {
		return nil, nil, err
	}
	return action, parentCase, nil
}

// loadOwnComment resolves the comment targeted by an edit / delete and rejects
// anyone but its author. It runs the private-Case gate first so a non-member
// cannot probe which comment ids exist under a private Case.
func (uc *ActionCommentUseCase) loadOwnComment(ctx context.Context, workspaceID string, actionID int64, commentID string) (*model.ActionComment, error) {
	authorID, err := commentAuthor(ctx)
	if err != nil {
		return nil, err
	}
	if _, _, err := uc.loadActionForCommentWrite(ctx, workspaceID, actionID); err != nil {
		return nil, err
	}

	// Absence and a storage failure are different answers: only the former is a
	// 404. Collapsing both would report a Firestore outage or a corrupt
	// document as "this comment does not exist", losing the cause for anyone
	// diagnosing it.
	existing, err := uc.repo.ActionComment().Get(ctx, workspaceID, actionID, commentID)
	if err != nil {
		if errors.Is(err, interfaces.ErrActionCommentNotFound) {
			return nil, goerr.Wrap(ErrActionCommentNotFound, "action comment not found",
				goerr.V(ActionIDKey, actionID),
				goerr.V(ActionCommentIDKey, commentID))
		}
		return nil, goerr.Wrap(err, "failed to load action comment",
			goerr.V(ActionIDKey, actionID),
			goerr.V(ActionCommentIDKey, commentID))
	}
	if existing.AuthorID != authorID {
		return nil, goerr.Wrap(ErrAccessDenied, "only the author can modify an action comment",
			goerr.V(ActionIDKey, actionID),
			goerr.V(ActionCommentIDKey, commentID),
			goerr.V("user_id", authorID))
	}
	return existing, nil
}

// canRead reports whether the caller may read comments for the given Action.
// A context with no auth token (system / agent / background flow) reads
// freely; a token-bearing caller must be a member of a private parent Case.
func (uc *ActionCommentUseCase) canRead(ctx context.Context, workspaceID string, actionID int64) (bool, error) {
	action, err := uc.repo.Action().Get(ctx, workspaceID, actionID)
	if err != nil {
		return false, goerr.Wrap(ErrActionNotFound, "action not found", goerr.V(ActionIDKey, actionID))
	}

	token, tokenErr := auth.TokenFromContext(ctx)
	if tokenErr != nil {
		return true, nil
	}

	parentCase, err := uc.repo.Case().Get(ctx, workspaceID, action.CaseID)
	if err != nil {
		return false, goerr.Wrap(err, "failed to get parent case", goerr.V(CaseIDKey, action.CaseID))
	}
	return model.IsCaseAccessible(parentCase, token.Sub), nil
}

// notifyCommentCreated posts the context-block thread reply announcing the new
// comment and folds the channel-side line into the active notification slot.
// Best-effort throughout: a failure here never rolls back the comment.
func (uc *ActionCommentUseCase) notifyCommentCreated(ctx context.Context, workspaceID string, action *model.Action, caseModel *model.Case, comment *model.ActionComment) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.registry.Language(workspaceID))
	if uc.slackService == nil {
		return
	}
	target, ok := resolveActionNotifyTarget(action, caseModel)
	if !ok {
		return
	}

	mention := mentionUser(comment.AuthorID)
	link := buildActionCommentWebURL(uc.baseURL, workspaceID, action.CaseID, action.ID, comment.ID)

	var body string
	if link != "" {
		body = i18n.T(ctx, i18n.MsgActionCommentAdded, mention, link)
	} else {
		body = i18n.T(ctx, i18n.MsgActionCommentAddedNoLink, mention)
	}
	if excerpt := commentExcerpt(comment.Body); excerpt != "" {
		body += "\n> " + excerpt
	}

	notice := actionNotice{
		body:       body,
		broadcast:  actionCommentBroadcasts,
		nameAction: true,
		actionURL:  buildActionWebURL(uc.baseURL, workspaceID, action.CaseID, action.ID),
	}
	if err := postActionNotice(ctx, uc.slackService, uc.slotCoord, target, action, notice); err != nil {
		errutil.Handle(ctx, err, "failed to post action comment notification")
	}
}

// commentExcerpt renders the one-line preview embedded in the Slack
// notification. Whitespace runs (newlines included) collapse to single spaces
// so the excerpt cannot break out of its blockquote line; the text is then
// truncated on a rune boundary and only escaped afterwards, because escaping
// first would let an expanded entity such as &amp; eat into the visible budget.
func commentExcerpt(body string) string {
	collapsed := strings.Join(strings.Fields(body), " ")
	if collapsed == "" {
		return ""
	}
	if utf8.RuneCountInString(collapsed) > commentExcerptMaxRunes {
		runes := []rune(collapsed)
		collapsed = string(runes[:commentExcerptMaxRunes]) + "…"
	}
	return slackTextEscaper.Replace(collapsed)
}
