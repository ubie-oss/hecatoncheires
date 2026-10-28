package usecase

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
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

const (
	// slackMemberChannelsFreshWindow is how long a stored channel list is
	// served before a background refresh is started. Joining a channel is
	// rare next to searching for one, and a user who just joined can reload
	// the list from the picker.
	slackMemberChannelsFreshWindow = 10 * time.Minute
	// slackMemberChannelsRefreshLease is how long a refresh claim keeps other
	// requests from starting another listing. It is longer than listing a
	// user with tens of thousands of channels takes at Slack's Tier 3 rate.
	slackMemberChannelsRefreshLease = 15 * time.Minute
	// existingSlackChannelCandidateLimit is how many matches one search
	// returns; the picker asks the user to type more when there are others.
	existingSlackChannelCandidateLimit = 50
	// slackChannelLinkCheckBatch is how many matches are checked for an
	// existing link per round of repository lookups (Firestore's `in` bound).
	slackChannelLinkCheckBatch = 30
)

// SlackChannelCandidateList is one search result of the existing-channel
// picker.
type SlackChannelCandidateList struct {
	// Candidates are at most existingSlackChannelCandidateLimit matches,
	// sorted by name.
	Candidates []SlackChannelCandidate
	// HasMore is true when more channels match than Candidates holds.
	HasMore bool
	// FetchedAt is when the channel list the search ran on was fetched from
	// Slack.
	FetchedAt time.Time
}

// ListExistingSlackChannelCandidates searches the channels the acting user
// may link a new case to in workspaceID: channels the user belongs to that the
// bot can see, minus every workspace's configured channels and every channel
// already linked to a case. A channel matches when its name contains query,
// ignoring case and a leading "#"; an empty query matches every channel. The
// result is for display only; CreateCase validates the chosen channel again.
//
// The user's channel list comes from the stored SlackMemberChannels. It is
// fetched from Slack when none is stored or refresh is true, and refreshed in
// the background when it is older than slackMemberChannelsFreshWindow.
func (uc *CaseUseCase) ListExistingSlackChannelCandidates(ctx context.Context, workspaceID, query string, refresh bool) (*SlackChannelCandidateList, error) {
	if !uc.ExistingSlackChannelAllowed(workspaceID) {
		return nil, goerr.Wrap(ErrExistingSlackChannelDisabled, "existing slack channel candidates requested",
			goerr.V("workspace_id", workspaceID))
	}
	tok, err := auth.TokenFromContext(ctx)
	if err != nil || tok.Sub == "" {
		return nil, goerr.Wrap(ErrAccessDenied, "listing existing slack channels requires an authenticated user",
			goerr.V("workspace_id", workspaceID))
	}

	list, err := uc.slackMemberChannels(ctx, workspaceID, tok.Sub, refresh)
	if err != nil {
		return nil, err
	}

	needle := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(query), "#"))
	matches := make([]SlackChannelCandidate, 0)
	for _, ch := range list.Channels {
		if uc.workspaceRegistry.IsConfiguredChannel(ch.ID) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(ch.Name), needle) {
			continue
		}
		matches = append(matches, SlackChannelCandidate{ID: ch.ID, Name: ch.Name, IsPrivate: ch.IsPrivate})
	}
	slices.SortFunc(matches, func(a, b SlackChannelCandidate) int {
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})

	// Only the matches that can reach the result are checked for a link, in
	// name order, so a search costs lookups for the page it returns rather
	// than for every channel the user has joined.
	result := &SlackChannelCandidateList{Candidates: []SlackChannelCandidate{}, FetchedAt: list.FetchedAt}
	for batch := range slices.Chunk(matches, slackChannelLinkCheckBatch) {
		linked, err := uc.linkedSlackChannels(ctx, batch)
		if err != nil {
			return nil, err
		}
		for _, c := range batch {
			if _, ok := linked[c.ID]; ok {
				continue
			}
			if len(result.Candidates) == existingSlackChannelCandidateLimit {
				result.HasMore = true
				return result, nil
			}
			result.Candidates = append(result.Candidates, c)
		}
	}
	return result, nil
}

// slackMemberChannels returns the stored channel list of userID, fetching it
// from Slack first when none is stored or refresh is true. A stale list is
// returned as is, and one request among those that see it stale (the one that
// wins the refresh claim) refreshes it in the background.
func (uc *CaseUseCase) slackMemberChannels(ctx context.Context, workspaceID, userID string, refresh bool) (*model.SlackMemberChannels, error) {
	if !refresh {
		list, err := uc.repo.SlackMemberChannels().Get(ctx, workspaceID, userID)
		if err == nil {
			now := time.Now()
			if !list.IsFresh(now, slackMemberChannelsFreshWindow) {
				claimed, err := uc.repo.SlackMemberChannels().ClaimRefresh(ctx, workspaceID, userID, now, slackMemberChannelsRefreshLease)
				if err != nil {
					return nil, goerr.Wrap(err, "failed to claim slack member channels refresh",
						goerr.V("workspace_id", workspaceID), goerr.V("user_id", userID))
				}
				if claimed {
					async.Dispatch(ctx, func(ctx context.Context) error {
						_, err := uc.fetchSlackMemberChannels(ctx, workspaceID, userID)
						return err
					})
				}
			}
			return list, nil
		}
		if !isRepoNotFound(err) {
			return nil, goerr.Wrap(err, "failed to get slack member channels",
				goerr.V("workspace_id", workspaceID), goerr.V("user_id", userID))
		}
	}
	return uc.fetchSlackMemberChannels(ctx, workspaceID, userID)
}

// fetchSlackMemberChannels lists userID's channels from Slack and stores them.
func (uc *CaseUseCase) fetchSlackMemberChannels(ctx context.Context, workspaceID, userID string) (*model.SlackMemberChannels, error) {
	channels, err := uc.slackService.ListMemberChannels(ctx, userID, uc.slackTeamIDForWorkspace(workspaceID))
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list slack channels of the user",
			goerr.V("workspace_id", workspaceID), goerr.V("user_id", userID))
	}

	list := &model.SlackMemberChannels{
		WorkspaceID: workspaceID,
		UserID:      userID,
		Channels:    make([]model.SlackMemberChannel, len(channels)),
		FetchedAt:   time.Now().UTC(),
	}
	for i, ch := range channels {
		list.Channels[i] = model.SlackMemberChannel{ID: ch.ID, Name: ch.Name, IsPrivate: ch.IsPrivate}
	}
	if err := uc.repo.SlackMemberChannels().Set(ctx, list); err != nil {
		return nil, goerr.Wrap(err, "failed to store slack member channels",
			goerr.V("workspace_id", workspaceID), goerr.V("user_id", userID))
	}
	return list, nil
}

// linkedSlackChannels returns the IDs among candidates that a case in any
// registered workspace already uses as its channel.
func (uc *CaseUseCase) linkedSlackChannels(ctx context.Context, candidates []SlackChannelCandidate) (map[string]struct{}, error) {
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.ID
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
	return linked, nil
}
