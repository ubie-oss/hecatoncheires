package usecase

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	goslack "github.com/slack-go/slack"
)

// Slack interaction action / block IDs for Action Block Kit messages.
const (
	SlackActionIDStatusSelect   = "hc_action_status"
	SlackActionIDAssigneeSelect = "hc_action_assignee"
	// slackActionAssigneeBlockIDPrefix is followed by ":{workspaceID}:{actionID}".
	// The status_select and users_select share a single actions block whose
	// block_id encodes (workspaceID, actionID), since users_select carries no
	// `value` for the handler to recover them from.
	slackActionAssigneeBlockIDPrefix = "hc_action_assignee_block"

	// actionCardTitleEmoji is the fixed prefix emoji that signals "this row
	// is an action card" in the parent Slack channel feed. The per-status
	// emoji was removed from the title — status is communicated by the
	// attachment side-bar color and by the Status select element instead.
	actionCardTitleEmoji = "📌"
)

// slackTextEscaper escapes user-supplied strings before embedding them in
// Slack mrkdwn / `text` fields. `&`, `<`, `>` are mrkdwn control characters
// (require HTML entity form); `|` is the `<URL|label>` separator and Slack
// does not honor backslash-escapes inside the label, so substitute a full-
// width pipe instead. Reused for any field that flows into the Action card
// title — kept private to this package until another caller needs it.
var slackTextEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	"|", "｜",
)

// SlackSyncMode controls how UpdateAction interacts with Slack.
type SlackSyncMode int

const (
	// SlackSyncFull updates the existing Slack message and posts a thread
	// notification for visible field changes (default).
	SlackSyncFull SlackSyncMode = iota
	// SlackSyncMessageOnly only refreshes the existing Slack message; no
	// thread notification is posted.
	SlackSyncMessageOnly
	// SlackSyncSkip leaves Slack untouched.
	SlackSyncSkip
)

// ActorKind describes who triggered an UpdateAction call.
type ActorKind int

const (
	ActorKindSystem ActorKind = iota
	ActorKindSlackUser
)

// ActorRef identifies the actor that triggered an update, for change-notification rendering.
type ActorRef struct {
	Kind ActorKind
	ID   string // Slack user ID when Kind == ActorKindSlackUser
}

// actorForAccess resolves the acting user for Action write access control,
// preferring the context auth token (GraphQL / WebUI path) and falling back to
// the Slack ActorRef (async Slack callback path, whose dispatched context
// carries no token — a token-only check would silently bypass access control on
// Slack-initiated writes). checkAccess is returned separately from actorID so a
// user-initiated call with an empty ID (malformed token, or a Slack actor
// missing its user ID) still denies for private cases rather than bypassing;
// only ActorKindSystem — which has no identified user by design — skips the
// check. The resolved pair feeds assertCaseWriteAccess.
func actorForAccess(ctx context.Context, actor ActorRef) (actorID string, checkAccess bool) {
	if token, err := auth.TokenFromContext(ctx); err == nil {
		return token.Sub, true
	}
	if actor.Kind == ActorKindSlackUser {
		return actor.ID, true
	}
	return "", false
}

// UpdateActionInput is the unified input for ActionUseCase.UpdateAction.
type UpdateActionInput struct {
	ID             int64
	CaseID         *int64
	Title          *string
	Description    *string
	AssigneeID     *string // nil = no change; "" is not a valid clear, use ClearAssignee.
	Status         *types.ActionStatus
	DueDate        *time.Time
	ClearDueDate   bool
	ClearAssignee  bool
	SlackMessageTS *string

	SlackSync SlackSyncMode
	Actor     ActorRef

	// RejectNonHumanAssignee, when true, drops AssigneeID changes whose
	// target user is missing from the SlackUser DB. The DB only stores
	// non-bot users, so this is a guard against picks coming from the
	// Slack users_select element (which has no built-in bot filter).
	// GraphQL/WebUI callers leave this false: their pickers already show
	// only synced humans, and silently dropping the change would just
	// look like a broken UI.
	RejectNonHumanAssignee bool
}

type ActionUseCase struct {
	repo            interfaces.Repository
	registry        *model.WorkspaceRegistry
	slackService    slack.Service
	baseURL         string
	slotCoord       *notificationSlotCoordinator
	workspaceAccess interfaces.WorkspaceAuthorizer
}

// NewActionUseCase constructs the ActionUseCase. slotCoord may be nil; when
// nil (or when its slotDuration is non-positive), channel-side change
// notifications fall back to the legacy reply_broadcast path on each post.
func NewActionUseCase(repo interfaces.Repository, registry *model.WorkspaceRegistry, slackService slack.Service, baseURL string, slotCoord *notificationSlotCoordinator) *ActionUseCase {
	return &ActionUseCase{
		repo:            repo,
		registry:        registry,
		slackService:    slackService,
		baseURL:         baseURL,
		slotCoord:       slotCoord,
		workspaceAccess: allowAllWorkspaces(),
	}
}

// statusSet returns the configured ActionStatusSet for the given workspace,
// falling back to the default set when the workspace is unknown or has no
// custom configuration.
func (uc *ActionUseCase) statusSet(workspaceID string) *model.ActionStatusSet {
	return resolveActionStatusSet(uc.registry, workspaceID)
}

// resolveActionStatusSet is shared helper for any code path that needs the
// workspace's ActionStatusSet but only has a registry handle.
func resolveActionStatusSet(registry *model.WorkspaceRegistry, workspaceID string) *model.ActionStatusSet {
	if registry == nil {
		return model.DefaultActionStatusSet()
	}
	entry, err := registry.Get(workspaceID)
	if err != nil || entry == nil || entry.ActionStatusSet == nil {
		return model.DefaultActionStatusSet()
	}
	return entry.ActionStatusSet
}

// ensureCaseAcceptsActions rejects putting an Action on c when c's workspace
// does not have Actions: a thread-mode workspace has them only when [case]
// actions opts in (model.WorkspaceEntry.ActionsEnabled). The workspace's mode
// decides it. When the workspace is not registered (no registry wired, or an
// unknown id) the case's own binding stands in for the mode, and a thread-bound
// case is refused — the default of the option.
func (uc *ActionUseCase) ensureCaseAcceptsActions(workspaceID string, c *model.Case) error {
	enabled := !c.IsThreadBound()
	if uc.registry != nil {
		entry, err := uc.registry.Get(workspaceID)
		switch {
		case err == nil:
			enabled = entry.ActionsEnabled()
		case !errors.Is(err, model.ErrWorkspaceNotFound):
			return goerr.Wrap(err, "resolve the workspace of the action's case",
				goerr.V(CaseIDKey, c.ID))
		}
	}
	if !enabled {
		return goerr.Wrap(ErrCaseActionsDisabled, "this workspace does not enable actions on thread-mode cases",
			goerr.V(CaseIDKey, c.ID),
			goerr.V("workspace_id", workspaceID))
	}
	return nil
}

// ActionsEnabled reports whether the Cases of the workspace have Actions,
// which is what the Web UI reads to show or hide the Related Actions section.
func (uc *ActionUseCase) ActionsEnabled(_ context.Context, workspaceID string) (bool, error) {
	if uc.registry == nil {
		// No workspace configuration to consult: the channel-mode default.
		return true, nil
	}
	entry, err := uc.registry.Get(workspaceID)
	if err != nil {
		return false, goerr.Wrap(err, "resolve the workspace", goerr.V("workspace_id", workspaceID))
	}
	return entry.ActionsEnabled(), nil
}

// actionNotifyTarget is where an Action's Slack notifications (change
// summaries, step / comment / archive notices) are posted.
type actionNotifyTarget struct {
	channelID string
	threadTS  string
	// inCaseThread is true for an Action of a thread-mode Case. There is no
	// per-Action card to hang a thread off, so every notification is a reply in
	// the Case's own thread. That thread is also the case conversation, so the
	// notifications there stay quiet: each names the Action it is about (several
	// Actions share the thread), none is broadcast to the channel, and none joins
	// the channel notification slot (the monitored channel is shared by every
	// case of the workspace, so a channel-level line would be noise for all of
	// them).
	inCaseThread bool
}

// resolveActionNotifyTarget decides where an Action's notifications go, or
// reports false when there is nowhere to post: the Case has no Slack channel,
// or — for a channel-mode Case — the Action has no Slack card yet.
func resolveActionNotifyTarget(action *model.Action, c *model.Case) (actionNotifyTarget, bool) {
	if action == nil || c == nil || c.SlackChannelID == "" {
		return actionNotifyTarget{}, false
	}
	if c.IsThreadBound() {
		return actionNotifyTarget{channelID: c.SlackChannelID, threadTS: c.SlackThreadTS, inCaseThread: true}, true
	}
	if action.SlackMessageTS == "" {
		return actionNotifyTarget{}, false
	}
	return actionNotifyTarget{channelID: c.SlackChannelID, threadTS: action.SlackMessageTS}, true
}

// actionLabel renders the compact reference to an Action used by the
// notifications posted into a thread-mode Case's thread: the action-card emoji
// followed by the bold title, linked to the WebUI when actionURL is known.
func actionLabel(action *model.Action, actionURL string) string {
	escapedTitle := slackTextEscaper.Replace(action.Title)
	if actionURL != "" {
		return fmt.Sprintf("%s *<%s|%s>*", actionCardTitleEmoji, actionURL, escapedTitle)
	}
	return fmt.Sprintf("%s *%s*", actionCardTitleEmoji, escapedTitle)
}

// actionNotice is one notification about an Action.
type actionNotice struct {
	// body is the mrkdwn text of the notification.
	body string
	// broadcast asks for the notification to also surface in the channel
	// (reply_broadcast, or the notification slot when one is configured). It is
	// ignored for a thread-mode Case, whose notifications are never broadcast.
	broadcast bool
	// nameAction prefixes the body with actionLabel in a thread-mode Case,
	// where the thread is shared by every Action of the Case. A body that
	// already names the Action (archive notices) leaves it false.
	nameAction bool
	// actionURL is the WebUI link actionLabel uses; empty renders no link.
	actionURL string
}

// postActionNotice posts one context-block notification about an Action to
// target, and folds the channel-side line into the notification slot when the
// notice is broadcast and a slot is configured. It is the single place that
// decides how an Action notification looks in each Case mode, shared by the
// action, step and comment usecases.
func postActionNotice(ctx context.Context, svc slack.Service, slotCoord *notificationSlotCoordinator, target actionNotifyTarget, action *model.Action, n actionNotice) error {
	body := n.body
	broadcast := n.broadcast
	if target.inCaseThread {
		if n.nameAction {
			body = actionLabel(action, n.actionURL) + "\n" + body
		}
		broadcast = false
	}

	blocks := []goslack.Block{
		goslack.NewContextBlock("",
			goslack.NewTextBlockObject(goslack.MarkdownType, body, false, false),
		),
	}
	aggregate := broadcast && slotCoord.enabled()

	var opts []slack.PostThreadOption
	if broadcast && !aggregate {
		opts = append(opts, slack.WithBroadcastToChannel())
	}
	if _, err := svc.PostThreadMessage(ctx, target.channelID, target.threadTS, blocks, body, opts...); err != nil {
		return goerr.Wrap(err, "failed to post action notification",
			goerr.V(ActionIDKey, action.ID),
			goerr.V(CaseIDKey, action.CaseID))
	}
	if aggregate {
		slotCoord.enqueueChannelLine(ctx, target.channelID, slotEntry{
			ActionMessageTS: action.SlackMessageTS,
			ActionTitle:     action.Title,
			Body:            n.body,
		})
	}
	return nil
}

func (uc *ActionUseCase) CreateAction(ctx context.Context, workspaceID string, caseID int64, title, description string, assigneeID string, slackMessageTS string, status types.ActionStatus, dueDate *time.Time) (*model.Action, error) {
	if title == "" {
		return nil, goerr.New("action title is required")
	}

	caseModel, err := uc.repo.Case().Get(ctx, workspaceID, caseID)
	if err != nil {
		return nil, goerr.Wrap(ErrCaseNotFound, "case not found", goerr.V(CaseIDKey, caseID))
	}

	actorID, checkAccess := tokenActor(ctx)
	if err := assertCaseWriteAccess(caseModel, actorID, checkAccess); err != nil {
		return nil, err
	}

	if err := uc.ensureCaseAcceptsActions(workspaceID, caseModel); err != nil {
		return nil, err
	}

	statusSet := uc.statusSet(workspaceID)
	if status == "" {
		status = types.ActionStatus(statusSet.InitialID())
	}
	if !statusSet.IsValid(string(status)) {
		return nil, goerr.New("invalid action status",
			goerr.V("status", status),
			goerr.V("workspace_id", workspaceID))
	}

	now := time.Now().UTC()
	action := &model.Action{
		CaseID:         caseID,
		Title:          title,
		Description:    description,
		AssigneeID:     assigneeID,
		SlackMessageTS: slackMessageTS,
		Status:         status,
		DueDate:        dueDate,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	created, err := uc.repo.Action().Create(ctx, workspaceID, action)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to create action",
			goerr.V(CaseIDKey, caseID))
	}

	// Record the creation event so the WebUI activity feed can show
	// "X created this action" alongside subsequent edits.
	creator := tokenUserID(ctx)
	if err := uc.repo.ActionEvent().Put(ctx, workspaceID, created.ID, &model.ActionEvent{
		ID:        uuid.NewString(),
		ActionID:  created.ID,
		Kind:      types.ActionEventCreated,
		ActorID:   creator,
		NewValue:  created.Title,
		CreatedAt: created.CreatedAt,
	}); err != nil {
		errutil.Handle(ctx, err, "failed to record action created event")
	}

	if uc.slackService != nil && caseModel.SlackChannelID != "" {
		posted, postErr := uc.postSlackMessageForAction(ctx, workspaceID, created, caseModel, tokenActorRef(ctx))
		if postErr != nil {
			errutil.Handle(ctx, postErr, "failed to post Slack notification for action")
		} else {
			created = posted
		}
	}

	return created, nil
}

// PostSlackMessageToAction posts the Action's primary Slack message and
// persists the resulting timestamp on the Action. It exists for two callers
// that operate on actions whose initial CreateAction post never reached
// Slack:
//   - The WebUI "Post to Slack" button on actions whose SlackMessageTS is
//     empty (e.g. the action was created before the tool path was unified
//     with the usecase).
//   - The diagnosis fix-unsent-action job, which sweeps such actions in
//     bulk.
//
// Unlike CreateAction's best-effort tail, this method is strict: a missing
// Slack channel, an already-posted action, or a Slack/PostMessage error
// surface as typed errors so the caller can react. Callers that want
// CreateAction-style "log and move on" behaviour should call CreateAction
// instead.
func (uc *ActionUseCase) PostSlackMessageToAction(ctx context.Context, workspaceID string, actionID int64) (*model.Action, error) {
	if uc.slackService == nil {
		return nil, goerr.New("slack service is not configured", goerr.V(ActionIDKey, actionID))
	}

	action, err := uc.repo.Action().Get(ctx, workspaceID, actionID)
	if err != nil {
		return nil, goerr.Wrap(ErrActionNotFound, "action not found", goerr.V(ActionIDKey, actionID))
	}

	parentCase, err := uc.repo.Case().Get(ctx, workspaceID, action.CaseID)
	if err != nil {
		return nil, goerr.Wrap(ErrCaseNotFound, "parent case not found",
			goerr.V(ActionIDKey, actionID),
			goerr.V(CaseIDKey, action.CaseID))
	}

	actorID, checkAccess := tokenActor(ctx)
	if err := assertCaseWriteAccess(parentCase, actorID, checkAccess); err != nil {
		return nil, err
	}

	if action.SlackMessageTS != "" {
		return nil, goerr.Wrap(ErrSlackMessageAlreadyPosted, "action already has a Slack message",
			goerr.V(ActionIDKey, actionID),
			goerr.V("slack_message_ts", action.SlackMessageTS))
	}
	if parentCase.SlackChannelID == "" {
		return nil, goerr.Wrap(ErrCaseHasNoSlackChannel, "parent case has no Slack channel",
			goerr.V(ActionIDKey, actionID),
			goerr.V(CaseIDKey, action.CaseID))
	}

	updated, err := uc.postSlackMessageForAction(ctx, workspaceID, action, parentCase, tokenActorRef(ctx))
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// postSlackMessageForAction posts the Action's primary Slack message and
// persists the resulting message timestamp on the Action. It is the single
// shared implementation used by:
//   - CreateAction (initial post during action creation; best-effort)
//   - PostSlackMessageToAction (user-driven re-post for actions that
//     missed the initial post; strict — error propagated to caller)
//   - diagnosis.UseCase.FixUnsentActions (batch repair; per-action error
//     bucketed into the report)
//
// The message depends on the Case mode:
//   - channel-mode: the Action card (title, description, status / assignee
//     selects) at the root of the Case channel. Its timestamp anchors the
//     Action's own thread, where every later notification is posted.
//   - thread-mode: a one-line context-block notice ("X created action ...")
//     replying in the Case thread — the same quiet format as the change
//     notifications, so creating Actions does not flood the case conversation.
//     Its timestamp is recorded so the Action counts as posted (the
//     fix-unsent-action sweep and the WebUI "Post to Slack" button key on it);
//     it is never refreshed into a card.
//
// actor is who created the Action; it is named only in the thread-mode notice.
//
// Preconditions: uc.slackService != nil and caseModel.SlackChannelID != "".
// Callers MUST verify these and decide their own policy (skip / error /
// degrade) for the missing-channel case.
func (uc *ActionUseCase) postSlackMessageForAction(ctx context.Context, workspaceID string, action *model.Action, caseModel *model.Case, actor ActorRef) (*model.Action, error) {
	actionURL := uc.actionWebURL(workspaceID, action.CaseID, action.ID)

	var ts string
	var postErr error
	if caseModel.IsThreadBound() {
		ctx = i18n.ContextWithWorkspaceLang(ctx, uc.registry.Language(workspaceID))
		body := i18n.T(ctx, i18n.MsgActionCreated, renderActor(ctx, actor), actionLabel(action, actionURL))
		blocks := []goslack.Block{
			goslack.NewContextBlock("",
				goslack.NewTextBlockObject(goslack.MarkdownType, body, false, false),
			),
		}
		ts, postErr = uc.slackService.PostThreadMessage(ctx, caseModel.SlackChannelID, caseModel.SlackThreadTS, blocks, body)
	} else {
		text, att := uc.buildActionMessagePayload(ctx, workspaceID, action, actionURL)
		ts, postErr = uc.slackService.PostMessageWithAttachment(ctx, caseModel.SlackChannelID, text, att)
	}
	if postErr != nil {
		return action, goerr.Wrap(postErr, "failed to post Slack message for action",
			goerr.V(ActionIDKey, action.ID),
			goerr.V(CaseIDKey, action.CaseID))
	}
	if ts == "" {
		return action, nil
	}
	action.SlackMessageTS = ts
	action.UpdatedAt = time.Now().UTC()
	updated, updateErr := uc.repo.Action().Update(ctx, workspaceID, action)
	if updateErr != nil {
		return action, goerr.Wrap(updateErr, "failed to persist Slack message timestamp",
			goerr.V(ActionIDKey, action.ID))
	}
	return updated, nil
}

// tokenActorRef names the caller of a token-driven write (GraphQL / WebUI) as
// a Slack-user actor, or the system when the context carries no token (agent
// tools, internal callers).
func tokenActorRef(ctx context.Context) ActorRef {
	if token, err := auth.TokenFromContext(ctx); err == nil && token.Sub != "" {
		return ActorRef{Kind: ActorKindSlackUser, ID: token.Sub}
	}
	return ActorRef{Kind: ActorKindSystem}
}

// UpdateAction is the single entry point for mutating an Action. All transports
// (GraphQL/WebUI, Slack interactivity, internal callers) funnel through this
// method; Slack side-effects are controlled by in.SlackSync and in.Actor.
func (uc *ActionUseCase) UpdateAction(ctx context.Context, workspaceID string, in UpdateActionInput) (*model.Action, error) {
	// Resolve the acting user from the auth token or the Slack Actor. The
	// workspace gate runs before anything is loaded, so a denied actor learns
	// nothing about which actions exist; the Slack select/assignee controls
	// reach here without passing the GraphQL field middleware.
	actorID, checkAccess := actorForAccess(ctx, in.Actor)
	if checkAccess {
		if err := uc.workspaceAccess.Authorize(ctx, workspaceID, actorID); err != nil {
			return nil, goerr.Wrap(err, "cannot update action", goerr.V(ActionIDKey, in.ID))
		}
	}

	existing, err := uc.repo.Action().Get(ctx, workspaceID, in.ID)
	if err != nil {
		return nil, goerr.Wrap(ErrActionNotFound, "action not found", goerr.V(ActionIDKey, in.ID))
	}

	parentCase, err := uc.repo.Case().Get(ctx, workspaceID, existing.CaseID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get parent case", goerr.V(CaseIDKey, existing.CaseID))
	}
	// Run the shared Case write access gate (see actorForAccess / assertCaseWriteAccess).
	if err := assertCaseWriteAccess(parentCase, actorID, checkAccess); err != nil {
		return nil, goerr.Wrap(err, "cannot update action", goerr.V(ActionIDKey, in.ID))
	}

	if in.CaseID != nil && *in.CaseID != existing.CaseID {
		newCase, err := uc.repo.Case().Get(ctx, workspaceID, *in.CaseID)
		if err != nil {
			return nil, goerr.Wrap(ErrCaseNotFound, "new case not found",
				goerr.V(CaseIDKey, *in.CaseID),
				goerr.V(ActionIDKey, in.ID))
		}
		// Reparenting must not move an action into a case whose workspace has
		// no Actions. Same boundary invariant as CreateAction.
		if err := uc.ensureCaseAcceptsActions(workspaceID, newCase); err != nil {
			return nil, err
		}
	}

	// Copy the whole struct so every field — including ArchivedAt and any field
	// added to model.Action later — is carried over verbatim. A field-by-field
	// literal silently dropped ArchivedAt, and because repo.Update is a full Set
	// overwrite, a single UpdateAction on an archived action un-archived it
	// (issue #188; same class as the SlackThreadTS loss fixed in #185 for Case).
	//
	// Copy — do NOT mutate `existing` in place. recordActionEvents below diffs
	// existing (before) against updated (after); mutating existing would collapse
	// that diff and silently drop the change history. (This is why UpdateAction
	// differs from UpdateCase, which mutates its loaded pointer directly but
	// records no such diff.) The patch below only reassigns fields on the copy;
	// the shared DueDate/ArchivedAt pointers are never mutated through, so
	// `existing` stays an untouched pre-update snapshot.
	copied := *existing
	action := &copied

	if in.CaseID != nil {
		action.CaseID = *in.CaseID
	}
	if in.Title != nil {
		if *in.Title == "" {
			return nil, goerr.New("action title cannot be empty", goerr.V(ActionIDKey, in.ID))
		}
		action.Title = *in.Title
	}
	if in.Description != nil {
		action.Description = *in.Description
	}
	if in.ClearAssignee {
		action.AssigneeID = ""
	} else if in.AssigneeID != nil {
		candidate := *in.AssigneeID
		switch {
		case candidate == "":
			action.AssigneeID = ""
		case in.RejectNonHumanAssignee:
			// Slack-sourced picks: refuse if the target is not a known
			// human (the SlackUser DB excludes bots at sync time). Keep
			// the prior assignee so the message re-render restores the
			// original initial_user.
			if u, lookupErr := uc.repo.SlackUser().GetByID(ctx, model.SlackUserID(candidate)); lookupErr != nil || u == nil {
				if lookupErr != nil {
					errutil.Handle(ctx, lookupErr, "rejected non-human or unknown assignee")
				}
			} else {
				action.AssigneeID = candidate
			}
		default:
			action.AssigneeID = candidate
		}
	}
	if in.SlackMessageTS != nil {
		action.SlackMessageTS = *in.SlackMessageTS
	}
	if in.Status != nil {
		if !uc.statusSet(workspaceID).IsValid(string(*in.Status)) {
			return nil, goerr.New("invalid action status",
				goerr.V("status", *in.Status),
				goerr.V("workspace_id", workspaceID),
				goerr.V(ActionIDKey, in.ID))
		}
		action.Status = *in.Status
	}
	if in.ClearDueDate {
		action.DueDate = nil
	} else if in.DueDate != nil {
		action.DueDate = in.DueDate
	}

	action.UpdatedAt = time.Now().UTC()
	updated, err := uc.repo.Action().Update(ctx, workspaceID, action)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to update action", goerr.V(ActionIDKey, in.ID))
	}

	// Record change history regardless of Slack sync mode. The activity feed
	// in the WebUI reads ActionEvent records as the source of truth for
	// "what changed when, by whom".
	uc.recordActionEvents(ctx, workspaceID, existing, updated, in.Actor)

	switch in.SlackSync {
	case SlackSyncSkip:
		// no Slack side effects
	case SlackSyncMessageOnly:
		uc.refreshSlackMessage(ctx, workspaceID, updated)
	case SlackSyncFull:
		uc.refreshSlackMessage(ctx, workspaceID, updated)
		// Slack thread also gets a human-readable context-block summary so
		// channel watchers see the change without opening the WebUI. The
		// ingest path drops these on the floor (HandleSlackMessage skips
		// our own bot ID) so they never enter the activity feed twice.
		uc.postActionChangeNotification(ctx, workspaceID, existing, updated, in.Actor)
	}

	return updated, nil
}

// ArchiveAction marks an action as archived so it disappears from default
// listings (Kanban, Case detail). Archived actions can be unarchived later
// via UnarchiveAction. The action document itself is preserved; only the
// public lifecycle hides it. Idempotency: archiving an already-archived
// action returns ErrActionAlreadyArchived rather than silently succeeding,
// because callers should distinguish "already done" from "now done" for
// UI feedback.
func (uc *ActionUseCase) ArchiveAction(ctx context.Context, workspaceID string, id int64, actor ActorRef) (*model.Action, error) {
	existing, parentCase, err := uc.loadActionForArchive(ctx, workspaceID, id, actor)
	if err != nil {
		return nil, err
	}
	if existing.IsArchived() {
		return nil, goerr.Wrap(ErrActionAlreadyArchived, "action is already archived", goerr.V(ActionIDKey, id))
	}

	now := time.Now().UTC()
	existing.ArchivedAt = &now
	existing.UpdatedAt = now
	updated, err := uc.repo.Action().Update(ctx, workspaceID, existing)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to archive action", goerr.V(ActionIDKey, id))
	}

	uc.recordArchiveEvent(ctx, workspaceID, updated, types.ActionEventArchived, actor)
	uc.notifyArchiveOnSlack(ctx, workspaceID, updated, parentCase, types.ActionEventArchived, actor)
	return updated, nil
}

// BulkArchiveActions archives the given actions by delegating each id to
// ArchiveAction, so bulk and single archive share the exact same path
// (access control, idempotency, event recording, Slack notification). The
// archive body lives only in ArchiveAction and is never duplicated here, so
// any future change to single-archive behaviour automatically applies to bulk.
//
// Idempotency: ids that are already archived are skipped rather than failing
// the whole batch, since "archive everything in this column" should tolerate
// rows that someone else already archived. Any other error (access denied,
// not found, persistence failure) is propagated — bulk archive must not
// silently swallow a real failure. The returned slice contains only the
// actions that were newly archived.
func (uc *ActionUseCase) BulkArchiveActions(ctx context.Context, workspaceID string, ids []int64, actor ActorRef) ([]*model.Action, error) {
	archived := make([]*model.Action, 0, len(ids))
	for _, id := range ids {
		updated, err := uc.ArchiveAction(ctx, workspaceID, id, actor)
		if err != nil {
			if errors.Is(err, ErrActionAlreadyArchived) {
				continue
			}
			return nil, goerr.Wrap(err, "failed to bulk archive action", goerr.V(ActionIDKey, id))
		}
		archived = append(archived, updated)
	}
	return archived, nil
}

// BulkArchiveActionsAsync runs BulkArchiveActions in the background via
// async.Dispatch so the operation outlives the HTTP request that triggered
// it. Clearing a completed column issues one ArchiveAction (DB write + event
// + Slack post) per action; on a large column that can take longer than the
// client is willing to wait, and a mid-flight disconnect would otherwise
// cancel the request context and leave the column half-archived. async.Dispatch
// hands the work a context with the auth token / logger intact but the
// cancellation severed, so the whole column is archived even after the
// GraphQL response (the accepted ids) has been returned. Per-action failures
// surface through errutil.Handle inside the synchronous core rather than to
// the caller, which has already returned.
func (uc *ActionUseCase) BulkArchiveActionsAsync(ctx context.Context, workspaceID string, ids []int64, actor ActorRef) {
	async.Dispatch(ctx, func(bgCtx context.Context) error {
		_, err := uc.BulkArchiveActions(bgCtx, workspaceID, ids, actor)
		return err
	})
}

// UnarchiveAction restores a previously archived action back to active state.
func (uc *ActionUseCase) UnarchiveAction(ctx context.Context, workspaceID string, id int64, actor ActorRef) (*model.Action, error) {
	existing, parentCase, err := uc.loadActionForArchive(ctx, workspaceID, id, actor)
	if err != nil {
		return nil, err
	}
	if !existing.IsArchived() {
		return nil, goerr.Wrap(ErrActionNotArchived, "action is not archived", goerr.V(ActionIDKey, id))
	}

	existing.ArchivedAt = nil
	existing.UpdatedAt = time.Now().UTC()
	updated, err := uc.repo.Action().Update(ctx, workspaceID, existing)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to unarchive action", goerr.V(ActionIDKey, id))
	}

	uc.recordArchiveEvent(ctx, workspaceID, updated, types.ActionEventUnarchived, actor)
	uc.notifyArchiveOnSlack(ctx, workspaceID, updated, parentCase, types.ActionEventUnarchived, actor)
	return updated, nil
}

// loadActionForArchive fetches the action and parent case, and verifies that
// the actor is allowed to mutate it. Mirrors UpdateAction's access-control
// pattern so Slack-initiated archive/unarchive (no auth token in context)
// still gets checked through ActorRef.
func (uc *ActionUseCase) loadActionForArchive(ctx context.Context, workspaceID string, id int64, actor ActorRef) (*model.Action, *model.Case, error) {
	existing, err := uc.repo.Action().Get(ctx, workspaceID, id)
	if err != nil {
		return nil, nil, goerr.Wrap(ErrActionNotFound, "action not found", goerr.V(ActionIDKey, id))
	}

	parentCase, err := uc.repo.Case().Get(ctx, workspaceID, existing.CaseID)
	if err != nil {
		return nil, nil, goerr.Wrap(err, "failed to get parent case", goerr.V(CaseIDKey, existing.CaseID))
	}

	actorID, checkAccess := actorForAccess(ctx, actor)
	if err := assertCaseWriteAccess(parentCase, actorID, checkAccess); err != nil {
		return nil, nil, goerr.Wrap(err, "cannot mutate action", goerr.V(ActionIDKey, id))
	}

	return existing, parentCase, nil
}

// recordArchiveEvent appends an ActionEvent for ARCHIVED / UNARCHIVED kind.
// Best-effort: failures are reported via errutil.Handle so the archive
// itself still succeeds.
func (uc *ActionUseCase) recordArchiveEvent(ctx context.Context, workspaceID string, action *model.Action, kind types.ActionEventKind, actor ActorRef) {
	actorID := ""
	if actor.Kind == ActorKindSlackUser {
		actorID = actor.ID
	}
	event := &model.ActionEvent{
		ID:        uuid.NewString(),
		ActionID:  action.ID,
		Kind:      kind,
		ActorID:   actorID,
		NewValue:  action.Title,
		CreatedAt: time.Now().UTC(),
	}
	if err := uc.repo.ActionEvent().Put(ctx, workspaceID, action.ID, event); err != nil {
		errutil.Handle(ctx, err, "failed to record action archive event")
	}
}

// notifyArchiveOnSlack refreshes the Slack message (so an archived badge can
// render) and posts a context-block thread reply describing who archived
// what. Best-effort throughout.
func (uc *ActionUseCase) notifyArchiveOnSlack(ctx context.Context, workspaceID string, action *model.Action, caseModel *model.Case, kind types.ActionEventKind, actor ActorRef) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.registry.Language(workspaceID))
	if uc.slackService == nil {
		return
	}
	target, ok := resolveActionNotifyTarget(action, caseModel)
	if !ok {
		return
	}

	uc.refreshSlackMessage(ctx, workspaceID, action)

	actorMention := renderActor(ctx, actor)
	var msg string
	switch kind {
	case types.ActionEventArchived:
		msg = i18n.T(ctx, i18n.MsgActionChangeArchived, actorMention, action.Title)
	case types.ActionEventUnarchived:
		msg = i18n.T(ctx, i18n.MsgActionChangeUnarchived, actorMention, action.Title)
	default:
		return
	}
	// The archive notice already names the Action, so it needs no label in a
	// thread-mode Case either.
	if err := postActionNotice(ctx, uc.slackService, nil, target, action, actionNotice{body: msg}); err != nil {
		errutil.Handle(ctx, err, "failed to post action archive notification")
	}
}

// GetAction returns the action by ID. opts is variadic and optional: existing
// callers pass none and get the action regardless of its parent Case's
// privacy (archive scope is irrelevant to a single Get). When the first opts
// element has ExcludePrivateCaseActions set, the parent Case is loaded and an
// action belonging to a private Case is reported as ErrAccessDenied — the MCP
// endpoint relies on this so private-case actions are never returned.
func (uc *ActionUseCase) GetAction(ctx context.Context, workspaceID string, id int64, opts ...interfaces.ActionListOptions) (*model.Action, error) {
	action, err := uc.repo.Action().Get(ctx, workspaceID, id)
	if err != nil {
		return nil, goerr.Wrap(ErrActionNotFound, "action not found", goerr.V(ActionIDKey, id))
	}

	var opt interfaces.ActionListOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.ExcludePrivateCaseActions {
		parentCase, caseErr := uc.repo.Case().Get(ctx, workspaceID, action.CaseID)
		if caseErr != nil {
			return nil, goerr.Wrap(ErrCaseNotFound, "parent case not found",
				goerr.V(ActionIDKey, id), goerr.V(CaseIDKey, action.CaseID))
		}
		if parentCase.IsPrivate {
			return nil, goerr.Wrap(ErrAccessDenied, "action belongs to a private case",
				goerr.V(ActionIDKey, id), goerr.V(CaseIDKey, action.CaseID))
		}
	}

	return action, nil
}

func (uc *ActionUseCase) ListActions(ctx context.Context, workspaceID string, opts interfaces.ActionListOptions) ([]*model.Action, error) {
	actions, err := uc.repo.Action().List(ctx, workspaceID, opts)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list actions")
	}
	return uc.filterActionsByParentAccess(ctx, workspaceID, actions, opts)
}

// GetActions retrieves multiple actions by ID in a single repository batch
// (GetByIDs) and applies the same parent-Case access control as the listing
// methods, avoiding the N+1 round-trips a per-ID loop would incur. Results
// preserve the order of ids; ids that do not resolve — and actions whose
// parent Case is excluded (private under opts.ExcludePrivateCaseActions, or
// inaccessible to the caller) — are omitted.
func (uc *ActionUseCase) GetActions(ctx context.Context, workspaceID string, ids []int64, opts interfaces.ActionListOptions) ([]*model.Action, error) {
	if len(ids) == 0 {
		return []*model.Action{}, nil
	}
	found, err := uc.repo.Action().GetByIDs(ctx, workspaceID, ids)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to batch get actions")
	}
	ordered := make([]*model.Action, 0, len(ids))
	for _, id := range ids {
		if a, ok := found[id]; ok {
			ordered = append(ordered, a)
		}
	}
	return uc.filterActionsByParentAccess(ctx, workspaceID, ordered, opts)
}

// filterActionsByParentAccess drops actions the caller may not see based on
// their parent Case. Membership-based access control applies when an auth
// token is present; opts.ExcludePrivateCaseActions additionally drops every
// private-case action regardless of membership (the MCP policy). Parent Cases
// are fetched in one batch (repo.Case().GetByIDs) rather than per-action, and
// an action whose parent cannot be resolved is dropped.
func (uc *ActionUseCase) filterActionsByParentAccess(ctx context.Context, workspaceID string, actions []*model.Action, opts interfaces.ActionListOptions) ([]*model.Action, error) {
	token, tokenErr := auth.TokenFromContext(ctx)
	// With no caller identity and no private-exclusion there is nothing to
	// filter (system/bot context reading public data).
	if tokenErr != nil && !opts.ExcludePrivateCaseActions {
		return actions, nil
	}

	caseIDSet := make(map[int64]struct{}, len(actions))
	for _, action := range actions {
		caseIDSet[action.CaseID] = struct{}{}
	}
	caseIDs := make([]int64, 0, len(caseIDSet))
	for caseID := range caseIDSet {
		caseIDs = append(caseIDs, caseID)
	}
	caseInfo, err := uc.repo.Case().GetByIDs(ctx, workspaceID, caseIDs)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to batch get parent cases")
	}

	filtered := make([]*model.Action, 0, len(actions))
	for _, action := range actions {
		parentCase, ok := caseInfo[action.CaseID]
		// A missing parent Case means we cannot vouch for the action's
		// visibility, so drop it.
		if !ok {
			continue
		}
		if opts.ExcludePrivateCaseActions && parentCase.IsPrivate {
			continue
		}
		if tokenErr == nil && !model.IsCaseAccessible(parentCase, token.Sub) {
			continue
		}
		filtered = append(filtered, action)
	}
	return filtered, nil
}

func (uc *ActionUseCase) GetActionsByCase(ctx context.Context, workspaceID string, caseID int64, opts interfaces.ActionListOptions) ([]*model.Action, error) {
	parentCase, err := uc.repo.Case().Get(ctx, workspaceID, caseID)
	if err != nil {
		return []*model.Action{}, nil
	}
	// MCP and similar entry points must never expose actions of a private
	// Case, even to a member: an empty list reveals nothing about the case.
	if opts.ExcludePrivateCaseActions && parentCase.IsPrivate {
		return []*model.Action{}, nil
	}
	token, tokenErr := auth.TokenFromContext(ctx)
	if tokenErr == nil && !model.IsCaseAccessible(parentCase, token.Sub) {
		return []*model.Action{}, nil
	}

	actions, err := uc.repo.Action().GetByCase(ctx, workspaceID, caseID, opts)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get actions by case", goerr.V(CaseIDKey, caseID))
	}
	return actions, nil
}

// refreshSlackMessage rebuilds and updates the Action's primary Slack message.
// It is best-effort: failures are logged but do not abort the caller.
func (uc *ActionUseCase) refreshSlackMessage(ctx context.Context, workspaceID string, action *model.Action) {
	if uc.slackService == nil || action.SlackMessageTS == "" {
		return
	}

	caseModel, err := uc.repo.Case().Get(ctx, workspaceID, action.CaseID)
	if err != nil {
		errutil.Handle(ctx, err, "failed to get case for Slack message update")
		return
	}
	// A thread-mode Case has no Action card: the message recorded on the Action
	// is the one-line creation notice in the Case thread, which must not be
	// turned into a card.
	if caseModel.SlackChannelID == "" || caseModel.IsThreadBound() {
		return
	}

	actionURL := uc.actionWebURL(workspaceID, action.CaseID, action.ID)
	text, att := uc.buildActionMessagePayload(ctx, workspaceID, action, actionURL)
	if updateErr := uc.slackService.UpdateMessageWithAttachment(ctx, caseModel.SlackChannelID, action.SlackMessageTS, text, att); updateErr != nil {
		errutil.Handle(ctx, updateErr, "failed to update Slack message for action")
	}
}

// postActionChangeNotification posts a context-block thread reply summarising
// changes to title / status / assignee. Slack channel watchers rely on this
// to see history without opening the WebUI; the ingest path drops these
// posts so they don't double-count in the ActionEvent feed.
func (uc *ActionUseCase) postActionChangeNotification(ctx context.Context, workspaceID string, before, after *model.Action, actor ActorRef) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.registry.Language(workspaceID))
	if uc.slackService == nil {
		return
	}

	caseModel, err := uc.repo.Case().Get(ctx, workspaceID, after.CaseID)
	if err != nil {
		errutil.Handle(ctx, err, "failed to get case for Slack change notification")
		return
	}
	target, ok := resolveActionNotifyTarget(after, caseModel)
	if !ok {
		return
	}

	actorMention := renderActor(ctx, actor)

	var lines []string
	if before.Title != after.Title {
		lines = append(lines, i18n.T(ctx, i18n.MsgActionChangeTitle, actorMention, before.Title, after.Title))
	}
	if before.Status != after.Status {
		lines = append(lines, i18n.T(ctx, i18n.MsgActionChangeStatus, actorMention, before.Status.String(), after.Status.String()))
	}
	if before.AssigneeID != after.AssigneeID {
		switch {
		case before.AssigneeID == "" && after.AssigneeID != "":
			lines = append(lines, i18n.T(ctx, i18n.MsgActionChangeAssigneeAssigned, actorMention, mentionUser(after.AssigneeID)))
		case before.AssigneeID != "" && after.AssigneeID == "":
			lines = append(lines, i18n.T(ctx, i18n.MsgActionChangeAssigneeUnassigned, actorMention, mentionUser(before.AssigneeID)))
		default:
			lines = append(lines, i18n.T(ctx, i18n.MsgActionChangeAssigneeReplaced, actorMention, mentionUser(before.AssigneeID), mentionUser(after.AssigneeID)))
		}
	}
	if len(lines) == 0 {
		return
	}

	body := strings.Join(lines, "\n")

	var kinds []types.ActionEventKind
	if before.Title != after.Title {
		kinds = append(kinds, types.ActionEventTitleChanged)
	}
	if before.Status != after.Status {
		kinds = append(kinds, types.ActionEventStatusChanged)
	}
	if before.AssigneeID != after.AssigneeID {
		kinds = append(kinds, types.ActionEventAssigneeChanged)
	}

	notice := actionNotice{
		body:       body,
		broadcast:  shouldBroadcastAnyActionEvent(kinds...),
		nameAction: true,
		actionURL:  uc.actionWebURL(workspaceID, after.CaseID, after.ID),
	}
	if err := postActionNotice(ctx, uc.slackService, uc.slotCoord, target, after, notice); err != nil {
		errutil.Handle(ctx, err, "failed to post action change notification")
	}
}

func renderActor(ctx context.Context, actor ActorRef) string {
	if actor.Kind == ActorKindSlackUser && actor.ID != "" {
		return mentionUser(actor.ID)
	}
	return i18n.T(ctx, i18n.MsgChangeActorSystem)
}

func mentionUser(slackUserID string) string {
	return fmt.Sprintf("<@%s>", slackUserID)
}

// recordActionEvents emits one ActionEvent per observable field diff
// (title / status / assignee). The activity feed reads this stream to
// render the change history. Best-effort: failures are logged but do
// not abort the caller.
func (uc *ActionUseCase) recordActionEvents(ctx context.Context, workspaceID string, before, after *model.Action, actor ActorRef) {
	actorID := ""
	if actor.Kind == ActorKindSlackUser {
		actorID = actor.ID
	}

	// Capture a single timestamp so all events emitted from one UpdateAction
	// call share an identical CreatedAt; otherwise the activity feed sees
	// sub-microsecond drift between sibling diffs and the sort becomes
	// dependent on map iteration order.
	now := time.Now().UTC()
	put := func(kind types.ActionEventKind, oldVal, newVal string) {
		event := &model.ActionEvent{
			ID:        uuid.NewString(),
			ActionID:  after.ID,
			Kind:      kind,
			ActorID:   actorID,
			OldValue:  oldVal,
			NewValue:  newVal,
			CreatedAt: now,
		}
		if err := uc.repo.ActionEvent().Put(ctx, workspaceID, after.ID, event); err != nil {
			errutil.Handle(ctx, err, "failed to record action event")
		}
	}

	if before.Title != after.Title {
		put(types.ActionEventTitleChanged, before.Title, after.Title)
	}
	if before.Status != after.Status {
		put(types.ActionEventStatusChanged, before.Status.String(), after.Status.String())
	}
	if before.AssigneeID != after.AssigneeID {
		put(types.ActionEventAssigneeChanged, before.AssigneeID, after.AssigneeID)
	}
}

func (uc *ActionUseCase) actionWebURL(workspaceID string, caseID, actionID int64) string {
	return buildActionWebURL(uc.baseURL, workspaceID, caseID, actionID)
}

// buildActionWebURL builds the WebUI deep link to an Action's detail modal.
// Package-level so ActionCommentUseCase can build the per-comment variant
// below from its own baseURL without duplicating the path shape.
func buildActionWebURL(baseURL, workspaceID string, caseID, actionID int64) string {
	if baseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/ws/%s/cases/%d/actions/%d", baseURL, workspaceID, caseID, actionID)
}

// buildActionCommentWebURL appends the query parameter the WebUI reads to
// select the Comments tab and highlight one comment.
func buildActionCommentWebURL(baseURL, workspaceID string, caseID, actionID int64, commentID string) string {
	base := buildActionWebURL(baseURL, workspaceID, caseID, actionID)
	if base == "" {
		return ""
	}
	return base + "?comment=" + url.QueryEscape(commentID)
}

// buildActionMessagePayload constructs the top-level `text` and the single
// attachment that together form the Action card. The shape (top-level text
// instead of top-level Block Kit) is what lets Slack render the parent
// excerpt when a `reply_broadcast` thread reply is shown in the channel
// view; with top-level `blocks`, Slack collapses that preview to a generic
// "a thread" link.
//
// Layout:
//   - top-level text: fixed prefix emoji + bold linked title (no "Action:"
//     literal — the fixed emoji is the visual signal). This text doubles as
//     the broadcast-preview source.
//   - attachment.color: status-derived hex (resolved via
//     ActionStatusDefinition.SlackColor); the side-bar gives status a glance-
//     level read.
//   - attachment.blocks: optional description Section, then one Actions block
//     carrying the status_select and assignee_select. Both selects share a
//     single Actions block; their block_id encodes (workspaceID, actionID).
func (uc *ActionUseCase) buildActionMessagePayload(ctx context.Context, workspaceID string, action *model.Action, actionURL string) (string, goslack.Attachment) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.registry.Language(workspaceID))
	statusSet := uc.statusSet(workspaceID)
	// `&`, `<`, `>` are mrkdwn control characters and must be HTML-escaped
	// or Slack swallows the title (or worse, treats user input as markup).
	// `|` inside a `<URL|label>` is the label terminator; Slack does not
	// support backslash-escaping it, so substitute the visually similar
	// full-width pipe. This same escaped form is reused outside the link
	// case for consistency.
	escapedTitle := slackTextEscaper.Replace(action.Title)
	titleText := fmt.Sprintf("%s *%s*", actionCardTitleEmoji, escapedTitle)
	if actionURL != "" {
		titleText = fmt.Sprintf("%s *<%s|%s>*", actionCardTitleEmoji, actionURL, escapedTitle)
	}

	var attBlocks []goslack.Block
	if action.Description != "" {
		attBlocks = append(attBlocks, goslack.NewSectionBlock(
			goslack.NewTextBlockObject(goslack.MarkdownType, action.Description, false, false),
			nil, nil,
		))
	}
	statusSelect := buildStatusSelect(ctx, workspaceID, action, statusSet)
	assigneeSelect := buildAssigneeSelect(ctx, action)
	attBlocks = append(attBlocks, goslack.NewActionBlock(
		SlackActionAssigneeBlockID(workspaceID, action.ID),
		statusSelect,
		assigneeSelect,
	))

	var color string
	if def, ok := statusSet.Get(string(action.Status)); ok {
		color = def.SlackColor()
	}

	return titleText, goslack.Attachment{
		Color:  color,
		Blocks: goslack.Blocks{BlockSet: attBlocks},
	}
}

func buildStatusSelect(ctx context.Context, workspaceID string, action *model.Action, statusSet *model.ActionStatusSet) *goslack.SelectBlockElement {
	defs := statusSet.Statuses()
	options := make([]*goslack.OptionBlockObject, 0, len(defs))
	var initial *goslack.OptionBlockObject
	for _, def := range defs {
		label := goslack.NewTextBlockObject(goslack.PlainTextType, statusLabel(ctx, def), true, false)
		opt := goslack.NewOptionBlockObject(
			fmt.Sprintf("%s:%d:%s", workspaceID, action.ID, def.ID),
			label,
			nil,
		)
		options = append(options, opt)
		if def.ID == string(action.Status) {
			initial = opt
		}
	}
	placeholder := goslack.NewTextBlockObject(goslack.PlainTextType, i18n.T(ctx, i18n.MsgActionStatusPlaceholder), true, false)
	sel := goslack.NewOptionsSelectBlockElement(goslack.OptTypeStatic, placeholder, SlackActionIDStatusSelect, options...)
	if initial != nil {
		sel.InitialOption = initial
	}
	return sel
}

// buildAssigneeSelect renders a users_select. Slack offers no native
// "exclude bots" toggle, so the handler treats a bot selection as a
// reject + re-render rather than filtering at render time.
func buildAssigneeSelect(ctx context.Context, action *model.Action) *goslack.SelectBlockElement {
	placeholder := goslack.NewTextBlockObject(goslack.PlainTextType, i18n.T(ctx, i18n.MsgActionAssigneePlaceholder), true, false)
	sel := goslack.NewOptionsSelectBlockElement(goslack.OptTypeUser, placeholder, SlackActionIDAssigneeSelect)
	if action.AssigneeID != "" {
		sel.InitialUser = action.AssigneeID
	}
	return sel
}

// SlackActionAssigneeBlockID returns the block_id that wraps the users_select
// element. We encode (workspaceID, actionID) into the block_id because
// users_select callbacks carry no `value` field.
func SlackActionAssigneeBlockID(workspaceID string, actionID int64) string {
	return fmt.Sprintf("%s:%s:%d", slackActionAssigneeBlockIDPrefix, workspaceID, actionID)
}

// ParseSlackAssigneeBlockID parses a block_id of the form
// "{prefix}:{workspaceID}:{actionID}" into its components.
func ParseSlackAssigneeBlockID(blockID string) (workspaceID string, actionID int64, err error) {
	prefix := slackActionAssigneeBlockIDPrefix + ":"
	if !strings.HasPrefix(blockID, prefix) {
		return "", 0, goerr.New("block_id missing assignee prefix", goerr.V("block_id", blockID))
	}
	rest := strings.TrimPrefix(blockID, prefix)
	lastColon := strings.LastIndex(rest, ":")
	if lastColon < 0 {
		return "", 0, goerr.New("invalid assignee block_id", goerr.V("block_id", blockID))
	}
	workspaceID = rest[:lastColon]
	idPart := rest[lastColon+1:]
	if _, parseErr := fmt.Sscanf(idPart, "%d", &actionID); parseErr != nil {
		return "", 0, goerr.Wrap(parseErr, "failed to parse action_id from block_id", goerr.V("block_id", blockID))
	}
	return workspaceID, actionID, nil
}

// statusLabel renders the user-facing label for an Action status definition.
// The workspace operator picks the language by writing `name` in their
// preferred locale; we just fall back to the id when name is absent.
func statusLabel(_ context.Context, def model.ActionStatusDefinition) string {
	if def.Name != "" {
		return def.Name
	}
	return def.ID
}

// ParseSlackStatusSelectValue parses a status_select option value of the form
// "{workspaceID}:{actionID}:{status}" and returns its components.
func ParseSlackStatusSelectValue(value string) (workspaceID string, actionID int64, status types.ActionStatus, err error) {
	// status is ALL_CAPS_WITH_UNDERSCORES; split from the right.
	lastColon := strings.LastIndex(value, ":")
	if lastColon < 0 {
		return "", 0, "", goerr.New("invalid status_select value: missing status separator", goerr.V("value", value))
	}
	statusPart := value[lastColon+1:]
	rest := value[:lastColon]

	mid := strings.LastIndex(rest, ":")
	if mid < 0 {
		return "", 0, "", goerr.New("invalid status_select value: missing action_id separator", goerr.V("value", value))
	}
	idPart := rest[mid+1:]
	wsPart := rest[:mid]

	var id int64
	if _, parseErr := fmt.Sscanf(idPart, "%d", &id); parseErr != nil {
		return "", 0, "", goerr.Wrap(parseErr, "failed to parse action_id", goerr.V("value", value))
	}
	// statusPart is validated against the workspace's ActionStatusSet by the
	// caller (the controller already knows the workspace). Here we just carry
	// the raw value forward — keeping this parser dependency-free.
	if statusPart == "" {
		return "", 0, "", goerr.New("empty status in status_select value", goerr.V("value", value))
	}
	return wsPart, id, types.ActionStatus(statusPart), nil
}
