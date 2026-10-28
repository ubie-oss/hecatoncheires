package usecase

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
)

// CreateCaseOption customises CaseUseCase.CreateCase.
type CreateCaseOption func(*createCaseOptions)

type createCaseOptions struct {
	existingSlackChannelID string
	// existingSlackChannelSet distinguishes WithExistingSlackChannel("") —
	// an invalid request — from no option at all.
	existingSlackChannelSet bool
}

// WithExistingSlackChannel makes CreateCase link the new case to an existing
// Slack channel instead of creating one. The case's privacy then follows the
// channel, so the caller must pass isPrivate=false.
func WithExistingSlackChannel(channelID string) CreateCaseOption {
	return func(o *createCaseOptions) {
		o.existingSlackChannelID = channelID
		o.existingSlackChannelSet = true
	}
}

// SlackChannelCandidate is one channel the acting user may link a new case to.
type SlackChannelCandidate struct {
	ID        string
	Name      string
	IsPrivate bool
}

// ExistingSlackChannelAllowed reports whether the workspace accepts an
// existing channel on case creation: allow_existing_channel is set, the
// workspace is in channel mode, and a Slack service is wired. An unknown
// workspace or a nil registry returns false.
func (uc *CaseUseCase) ExistingSlackChannelAllowed(workspaceID string) bool {
	if uc.workspaceRegistry == nil || uc.slackService == nil {
		return false
	}
	entry, err := uc.workspaceRegistry.Get(workspaceID)
	if err != nil {
		return false
	}
	return entry.AllowExistingSlackChannel && !entry.IsThreadMode()
}

// createCaseInExistingSlackChannel is CreateCase with WithExistingSlackChannel.
// Every check that can fail without a side effect runs before the bot joins
// the channel, and the case is persisted only after the channel passed all of
// them, so a rejected request leaves no case behind.
func (uc *CaseUseCase) createCaseInExistingSlackChannel(ctx context.Context, workspaceID string, title, description string, assigneeIDs []string, fieldValues map[string]model.FieldValue, isPrivate bool, isTest bool, sourceTeamID string, requestKey string, channelID string) (*model.Case, error) {
	if !uc.ExistingSlackChannelAllowed(workspaceID) {
		return nil, goerr.Wrap(ErrExistingSlackChannelDisabled, "existing slack channel requested",
			goerr.V("workspace_id", workspaceID))
	}
	if channelID == "" {
		return nil, goerr.Wrap(ErrInvalidArgument, "existing slack channel id is empty",
			goerr.V("workspace_id", workspaceID))
	}
	// The case's privacy is the channel's; a caller asking for a specific
	// value would get a case that silently differs from its request.
	if isPrivate {
		return nil, goerr.Wrap(ErrInvalidArgument, "isPrivate cannot be set when linking an existing slack channel",
			goerr.V("workspace_id", workspaceID), goerr.V("channel_id", channelID))
	}
	tok, err := auth.TokenFromContext(ctx)
	if err != nil || tok.Sub == "" {
		return nil, goerr.Wrap(ErrAccessDenied, "linking an existing slack channel requires an authenticated user",
			goerr.V("workspace_id", workspaceID), goerr.V("channel_id", channelID))
	}

	info, err := uc.validateExistingSlackChannel(ctx, channelID, tok.Sub)
	if err != nil {
		return nil, err
	}

	entry, err := uc.resolveCaseMode(workspaceID)
	if err != nil {
		return nil, err
	}
	return uc.createChannelModeCase(ctx, workspaceID, entry, title, description, assigneeIDs, fieldValues, info.IsPrivate, isTest, sourceTeamID, requestKey, channelID)
}

// validateExistingSlackChannel checks that channelID can become a case channel
// for actorID and makes the bot a member of it when it is a public channel the
// bot has not joined. The checks without side effects (configured channel,
// existence and archive state, already linked) run first; the join comes
// before the membership check because conversations.members is only known to
// work on a channel the bot belongs to.
func (uc *CaseUseCase) validateExistingSlackChannel(ctx context.Context, channelID, actorID string) (*slack.ChannelInfo, error) {
	if uc.workspaceRegistry.IsConfiguredChannel(channelID) {
		return nil, goerr.Wrap(ErrSlackChannelNotLinkable, "channel is a configured monitor or workspace channel",
			goerr.V("channel_id", channelID))
	}

	info, err := uc.slackService.GetChannelInfo(ctx, channelID)
	if err != nil {
		if errors.Is(err, slack.ErrChannelUnavailable) {
			return nil, goerr.Wrap(ErrSlackChannelNotLinkable, "channel is not visible to the bot",
				goerr.V("channel_id", channelID), goerr.V("slack_error", err.Error()))
		}
		return nil, goerr.Wrap(err, "failed to get slack channel info", goerr.V("channel_id", channelID))
	}
	if info.IsArchived {
		return nil, goerr.Wrap(ErrSlackChannelNotLinkable, "channel is archived",
			goerr.V("channel_id", channelID))
	}

	linked, linkedWorkspaceID, err := uc.findCaseBySlackChannelInAnyWorkspace(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if linked != nil {
		return nil, goerr.Wrap(ErrSlackChannelAlreadyLinked, "channel is already linked to a case",
			goerr.V("channel_id", channelID),
			goerr.V("linked_workspace_id", linkedWorkspaceID),
			goerr.V(CaseIDKey, linked.ID))
	}

	if !info.IsMember {
		// conversations.join cannot add the bot to a private channel; it must
		// be invited by a member.
		if info.IsPrivate {
			return nil, goerr.Wrap(ErrSlackChannelNotLinkable, "bot is not a member of the private channel",
				goerr.V("channel_id", channelID))
		}
		if err := uc.slackService.JoinChannel(ctx, channelID); err != nil {
			return nil, goerr.Wrap(ErrSlackChannelJoinFailed, "bot failed to join the channel",
				goerr.V("channel_id", channelID), goerr.V("slack_error", err.Error()))
		}
	}

	members, err := uc.slackService.GetConversationMembers(ctx, channelID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get slack channel members", goerr.V("channel_id", channelID))
	}
	if !slices.Contains(members, actorID) {
		return nil, goerr.Wrap(ErrSlackChannelNotLinkable, "acting user is not a member of the channel",
			goerr.V("channel_id", channelID), goerr.V("user_id", actorID))
	}

	return info, nil
}

// findCaseBySlackChannelInAnyWorkspace returns the first case in any
// registered workspace whose SlackChannelID is channelID, with that
// workspace's ID. A lookup failure is returned rather than skipped: treating
// it as "not linked" would let a second case take the channel.
func (uc *CaseUseCase) findCaseBySlackChannelInAnyWorkspace(ctx context.Context, channelID string) (*model.Case, string, error) {
	for _, entry := range uc.workspaceRegistry.List() {
		c, err := uc.repo.Case().GetBySlackChannelID(ctx, entry.Workspace.ID, channelID)
		if err != nil {
			return nil, "", goerr.Wrap(err, "failed to look up case by slack channel",
				goerr.V("workspace_id", entry.Workspace.ID), goerr.V("channel_id", channelID))
		}
		if c != nil {
			return c, entry.Workspace.ID, nil
		}
	}
	return nil, "", nil
}

// ListExistingSlackChannelCandidates returns the channels the acting user may
// link a new case to in workspaceID: channels the user belongs to that the
// bot can see, minus every workspace's configured channels and every channel
// already linked to a case, sorted by name. The list is for display only;
// CreateCase validates the chosen channel again.
func (uc *CaseUseCase) ListExistingSlackChannelCandidates(ctx context.Context, workspaceID string) ([]SlackChannelCandidate, error) {
	if !uc.ExistingSlackChannelAllowed(workspaceID) {
		return nil, goerr.Wrap(ErrExistingSlackChannelDisabled, "existing slack channel candidates requested",
			goerr.V("workspace_id", workspaceID))
	}
	tok, err := auth.TokenFromContext(ctx)
	if err != nil || tok.Sub == "" {
		return nil, goerr.Wrap(ErrAccessDenied, "listing existing slack channels requires an authenticated user",
			goerr.V("workspace_id", workspaceID))
	}

	channels, err := uc.slackService.ListMemberChannels(ctx, tok.Sub, uc.slackTeamIDForWorkspace(workspaceID))
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list slack channels of the user",
			goerr.V("workspace_id", workspaceID))
	}

	candidates := make([]SlackChannelCandidate, 0, len(channels))
	ids := make([]string, 0, len(channels))
	for _, ch := range channels {
		if uc.workspaceRegistry.IsConfiguredChannel(ch.ID) {
			continue
		}
		candidates = append(candidates, SlackChannelCandidate{ID: ch.ID, Name: ch.Name, IsPrivate: ch.IsPrivate})
		ids = append(ids, ch.ID)
	}

	linked := make(map[string]struct{})
	for _, entry := range uc.workspaceRegistry.List() {
		found, err := uc.repo.Case().GetBySlackChannelIDs(ctx, entry.Workspace.ID, ids)
		if err != nil {
			return nil, goerr.Wrap(err, "failed to look up cases by slack channels",
				goerr.V("workspace_id", entry.Workspace.ID))
		}
		for id := range found {
			linked[id] = struct{}{}
		}
	}
	candidates = slices.DeleteFunc(candidates, func(c SlackChannelCandidate) bool {
		_, ok := linked[c.ID]
		return ok
	})

	slices.SortFunc(candidates, func(a, b SlackChannelCandidate) int {
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return candidates, nil
}
