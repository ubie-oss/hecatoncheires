package usecase

import (
	"context"
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
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

// ActionStepUseCase orchestrates ActionStep CRUD: load parent Action and
// Case, enforce private-case access control, persist the step, record an
// ActionEvent, and post a thread context-block notification to Slack.
type ActionStepUseCase struct {
	repo         interfaces.Repository
	registry     *model.WorkspaceRegistry
	slackService slack.Service
	slotCoord    *notificationSlotCoordinator
}

// NewActionStepUseCase constructs the ActionStepUseCase. registry supplies
// the workspace's output language for the step notifications and may be nil
// (the notifications then follow the context's language). slotCoord may be
// nil; when nil (or its slotDuration is non-positive), step change
// notifications fall back to the legacy reply_broadcast path.
func NewActionStepUseCase(repo interfaces.Repository, registry *model.WorkspaceRegistry, slackService slack.Service, slotCoord *notificationSlotCoordinator) *ActionStepUseCase {
	return &ActionStepUseCase{
		repo:         repo,
		registry:     registry,
		slackService: slackService,
		slotCoord:    slotCoord,
	}
}

// AddActionStepInput is the input for ActionStepUseCase.Add.
type AddActionStepInput struct {
	WorkspaceID string
	ActionID    int64
	Title       string
	Actor       ActorRef
}

// SetActionStepDoneInput is the input for ActionStepUseCase.SetDone.
type SetActionStepDoneInput struct {
	WorkspaceID string
	ActionID    int64
	StepID      string
	Done        bool
	Actor       ActorRef
}

// RenameActionStepInput is the input for ActionStepUseCase.Rename.
type RenameActionStepInput struct {
	WorkspaceID string
	ActionID    int64
	StepID      string
	Title       string
	Actor       ActorRef
}

// DeleteActionStepInput is the input for ActionStepUseCase.Delete.
type DeleteActionStepInput struct {
	WorkspaceID string
	ActionID    int64
	StepID      string
	Actor       ActorRef
}

// loadActionForStepMutation fetches the parent Action and Case and verifies
// the actor is allowed to mutate steps under it. Mirrors the access pattern
// used by ActionUseCase.loadActionForArchive: when an auth token is present
// the token's user is checked; otherwise an explicit ActorKindSlackUser is
// honoured. ActorKindSystem (LLM tool path) bypasses the check, matching
// existing Action mutations.
func (uc *ActionStepUseCase) loadActionForStepMutation(ctx context.Context, workspaceID string, actionID int64, actor ActorRef) (*model.Action, *model.Case, error) {
	action, err := uc.repo.Action().Get(ctx, workspaceID, actionID)
	if err != nil {
		return nil, nil, goerr.Wrap(ErrActionNotFound, "action not found", goerr.V(ActionIDKey, actionID))
	}

	parentCase, err := uc.repo.Case().Get(ctx, workspaceID, action.CaseID)
	if err != nil {
		return nil, nil, goerr.Wrap(err, "failed to get parent case", goerr.V(CaseIDKey, action.CaseID))
	}

	var actorID string
	var checkAccess bool
	if token, tokenErr := auth.TokenFromContext(ctx); tokenErr == nil {
		actorID = token.Sub
		checkAccess = true
	} else if actor.Kind == ActorKindSlackUser {
		actorID = actor.ID
		checkAccess = true
	}
	if checkAccess && !model.IsCaseAccessible(parentCase, actorID) {
		return nil, nil, goerr.Wrap(ErrAccessDenied, "cannot mutate action step in private case",
			goerr.V(ActionIDKey, actionID), goerr.V("user_id", actorID))
	}

	return action, parentCase, nil
}

// canRead reports whether the caller may read steps for the given Action.
// The bot/system context (no auth token) returns true so background flows
// continue to work; a token-bearing caller must be a member of a private
// parent Case.
func (uc *ActionStepUseCase) canRead(ctx context.Context, workspaceID string, actionID int64) (*model.Action, bool, error) {
	action, err := uc.repo.Action().Get(ctx, workspaceID, actionID)
	if err != nil {
		return nil, false, goerr.Wrap(ErrActionNotFound, "action not found", goerr.V(ActionIDKey, actionID))
	}

	token, tokenErr := auth.TokenFromContext(ctx)
	if tokenErr != nil {
		return action, true, nil
	}

	parentCase, err := uc.repo.Case().Get(ctx, workspaceID, action.CaseID)
	if err != nil {
		return nil, false, goerr.Wrap(err, "failed to get parent case", goerr.V(CaseIDKey, action.CaseID))
	}
	return action, model.IsCaseAccessible(parentCase, token.Sub), nil
}

// Add creates a new ActionStep under the given Action.
func (uc *ActionStepUseCase) Add(ctx context.Context, in AddActionStepInput) (*model.ActionStep, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.registry.Language(in.WorkspaceID))
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, goerr.New("action step title is required")
	}

	action, parentCase, err := uc.loadActionForStepMutation(ctx, in.WorkspaceID, in.ActionID, in.Actor)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	creator := actorIdentifier(ctx, in.Actor)

	step := &model.ActionStep{
		ID:        uuid.NewString(),
		ActionID:  action.ID,
		Title:     title,
		CreatedBy: creator,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := uc.repo.ActionStep().Put(ctx, in.WorkspaceID, step); err != nil {
		return nil, goerr.Wrap(err, "failed to create action step",
			goerr.V(ActionIDKey, in.ActionID))
	}

	uc.recordStepEvent(ctx, in.WorkspaceID, action.ID, types.ActionEventStepAdded, in.Actor, "", step.Title, now)
	uc.notifyStepEvent(ctx, action, parentCase, types.ActionEventStepAdded, in.Actor, "", step.Title)

	return step, nil
}

// SetDone toggles the step's done state. No-op (returns the existing step
// untouched) if the requested state is already in effect.
func (uc *ActionStepUseCase) SetDone(ctx context.Context, in SetActionStepDoneInput) (*model.ActionStep, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.registry.Language(in.WorkspaceID))
	action, parentCase, err := uc.loadActionForStepMutation(ctx, in.WorkspaceID, in.ActionID, in.Actor)
	if err != nil {
		return nil, err
	}

	step, err := uc.repo.ActionStep().Get(ctx, in.WorkspaceID, action.ID, in.StepID)
	if err != nil {
		return nil, goerr.Wrap(ErrActionStepNotFound, "action step not found",
			goerr.V(ActionIDKey, in.ActionID), goerr.V(ActionStepIDKey, in.StepID))
	}

	wasDone := step.IsDone()
	if wasDone == in.Done {
		return step, nil
	}

	now := time.Now().UTC()
	if in.Done {
		t := now
		step.DoneAt = &t
		step.DoneBy = actorIdentifier(ctx, in.Actor)
	} else {
		step.DoneAt = nil
		step.DoneBy = ""
	}
	step.UpdatedAt = now

	if err := uc.repo.ActionStep().Put(ctx, in.WorkspaceID, step); err != nil {
		return nil, goerr.Wrap(err, "failed to update action step",
			goerr.V(ActionIDKey, in.ActionID), goerr.V(ActionStepIDKey, in.StepID))
	}

	kind := types.ActionEventStepReopened
	if in.Done {
		kind = types.ActionEventStepDone
	}
	uc.recordStepEvent(ctx, in.WorkspaceID, action.ID, kind, in.Actor, "", step.Title, now)
	uc.notifyStepEvent(ctx, action, parentCase, kind, in.Actor, "", step.Title)

	return step, nil
}

// Rename updates the step's title. No-op (returns the existing step
// untouched) when the trimmed title equals the current one — no
// ActionEvent is recorded and no Slack notification is sent in that case.
func (uc *ActionStepUseCase) Rename(ctx context.Context, in RenameActionStepInput) (*model.ActionStep, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.registry.Language(in.WorkspaceID))
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, goerr.New("action step title is required")
	}

	action, parentCase, err := uc.loadActionForStepMutation(ctx, in.WorkspaceID, in.ActionID, in.Actor)
	if err != nil {
		return nil, err
	}

	step, err := uc.repo.ActionStep().Get(ctx, in.WorkspaceID, action.ID, in.StepID)
	if err != nil {
		return nil, goerr.Wrap(ErrActionStepNotFound, "action step not found",
			goerr.V(ActionIDKey, in.ActionID), goerr.V(ActionStepIDKey, in.StepID))
	}

	if step.Title == title {
		return step, nil
	}

	oldTitle := step.Title
	now := time.Now().UTC()
	step.Title = title
	step.UpdatedAt = now

	if err := uc.repo.ActionStep().Put(ctx, in.WorkspaceID, step); err != nil {
		return nil, goerr.Wrap(err, "failed to rename action step",
			goerr.V(ActionIDKey, in.ActionID), goerr.V(ActionStepIDKey, in.StepID))
	}

	uc.recordStepEvent(ctx, in.WorkspaceID, action.ID, types.ActionEventStepTitleChanged, in.Actor, oldTitle, title, now)
	uc.notifyStepEvent(ctx, action, parentCase, types.ActionEventStepTitleChanged, in.Actor, oldTitle, title)

	return step, nil
}

// Delete removes a step. Returns nil even if the step did not exist
// (idempotent) but only after access control is satisfied.
func (uc *ActionStepUseCase) Delete(ctx context.Context, in DeleteActionStepInput) error {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.registry.Language(in.WorkspaceID))
	action, parentCase, err := uc.loadActionForStepMutation(ctx, in.WorkspaceID, in.ActionID, in.Actor)
	if err != nil {
		return err
	}

	step, err := uc.repo.ActionStep().Get(ctx, in.WorkspaceID, action.ID, in.StepID)
	if err != nil {
		return goerr.Wrap(ErrActionStepNotFound, "action step not found",
			goerr.V(ActionIDKey, in.ActionID), goerr.V(ActionStepIDKey, in.StepID))
	}

	if err := uc.repo.ActionStep().Delete(ctx, in.WorkspaceID, action.ID, in.StepID); err != nil {
		return goerr.Wrap(err, "failed to delete action step",
			goerr.V(ActionIDKey, in.ActionID), goerr.V(ActionStepIDKey, in.StepID))
	}

	now := time.Now().UTC()
	uc.recordStepEvent(ctx, in.WorkspaceID, action.ID, types.ActionEventStepRemoved, in.Actor, step.Title, "", now)
	uc.notifyStepEvent(ctx, action, parentCase, types.ActionEventStepRemoved, in.Actor, step.Title, "")

	return nil
}

// List returns all steps for the given Action, ordered oldest first.
// Returns an empty list when the caller cannot access the parent Case.
func (uc *ActionStepUseCase) List(ctx context.Context, workspaceID string, actionID int64) ([]*model.ActionStep, error) {
	_, ok, err := uc.canRead(ctx, workspaceID, actionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []*model.ActionStep{}, nil
	}
	steps, err := uc.repo.ActionStep().List(ctx, workspaceID, actionID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list action steps",
			goerr.V(ActionIDKey, actionID))
	}
	return steps, nil
}

// Progress returns (done, total) for the given Action's steps. Both are 0
// when the parent Case is inaccessible to the caller.
func (uc *ActionStepUseCase) Progress(ctx context.Context, workspaceID string, actionID int64) (int, int, error) {
	steps, err := uc.List(ctx, workspaceID, actionID)
	if err != nil {
		return 0, 0, err
	}
	done := 0
	for _, s := range steps {
		if s.IsDone() {
			done++
		}
	}
	return done, len(steps), nil
}

// recordStepEvent appends an ActionEvent for STEP_* kinds. Best-effort.
func (uc *ActionStepUseCase) recordStepEvent(ctx context.Context, workspaceID string, actionID int64, kind types.ActionEventKind, actor ActorRef, oldValue, newValue string, at time.Time) {
	actorID := ""
	if actor.Kind == ActorKindSlackUser {
		actorID = actor.ID
	}
	event := &model.ActionEvent{
		ID:        uuid.NewString(),
		ActionID:  actionID,
		Kind:      kind,
		ActorID:   actorID,
		OldValue:  oldValue,
		NewValue:  newValue,
		CreatedAt: at,
	}
	if err := uc.repo.ActionEvent().Put(ctx, workspaceID, actionID, event); err != nil {
		errutil.Handle(ctx, err, "failed to record action step event")
	}
}

// notifyStepEvent posts a thread context-block to Slack describing the
// step change — in the Action card's thread for a channel-mode Case, in the
// Case thread for a thread-mode one (see resolveActionNotifyTarget).
// Best-effort: silently no-ops when Slack is not configured or there is no
// Slack thread to attach to.
func (uc *ActionStepUseCase) notifyStepEvent(ctx context.Context, action *model.Action, caseModel *model.Case, kind types.ActionEventKind, actor ActorRef, oldValue, newValue string) {
	if uc.slackService == nil {
		return
	}
	target, ok := resolveActionNotifyTarget(action, caseModel)
	if !ok {
		return
	}

	actorMention := renderActor(ctx, actor)
	var msg string
	switch kind {
	case types.ActionEventStepAdded:
		msg = i18n.T(ctx, i18n.MsgActionStepAdded, actorMention, newValue)
	case types.ActionEventStepRemoved:
		msg = i18n.T(ctx, i18n.MsgActionStepRemoved, actorMention, oldValue)
	case types.ActionEventStepDone:
		msg = i18n.T(ctx, i18n.MsgActionStepDone, actorMention, newValue)
	case types.ActionEventStepReopened:
		msg = i18n.T(ctx, i18n.MsgActionStepReopened, actorMention, newValue)
	case types.ActionEventStepTitleChanged:
		msg = i18n.T(ctx, i18n.MsgActionStepRenamed, actorMention, oldValue, newValue)
	default:
		return
	}

	// The step usecase has no base URL, so in a thread-mode Case the Action is
	// named by its title without a WebUI link.
	notice := actionNotice{
		body:       msg,
		broadcast:  shouldBroadcastActionEvent(kind),
		nameAction: true,
	}
	if err := postActionNotice(ctx, uc.slackService, uc.slotCoord, target, action, notice); err != nil {
		errutil.Handle(ctx, err, "failed to post action step notification")
	}
}

// actorIdentifier returns the Slack user id to record as Created/Done by.
// When the caller has an auth token (WebUI / GraphQL flow), prefer it;
// otherwise honour the explicit ActorRef. System-actor mutations write
// "" so the audit trail clearly distinguishes them from human actions.
func actorIdentifier(ctx context.Context, actor ActorRef) string {
	if token, tokenErr := auth.TokenFromContext(ctx); tokenErr == nil {
		return token.Sub
	}
	if actor.Kind == ActorKindSlackUser {
		return actor.ID
	}
	return ""
}
