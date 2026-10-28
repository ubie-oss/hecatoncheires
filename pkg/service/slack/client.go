package slack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/slack-go/slack"

	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/uierr"
)

// resolveDisplayName picks the most user-friendly label Slack exposes for a
// user. Slack-go's `User.RealName` is the legacy top-level real-name field
// and is rarely what people actually see in Slack clients; the human-set
// "Display name" lives on `Profile.DisplayName`, with the profile-level
// real name as the standard fallback Slack itself uses when display name
// is blank.
func resolveDisplayName(u slack.User) string {
	return cmp.Or(u.Profile.DisplayName, u.Profile.RealName, u.RealName)
}

const (
	// DefaultCacheTTL is the default TTL for channel name cache
	DefaultCacheTTL = 45 * time.Second
)

// cacheEntry holds a cached channel name with expiration
type cacheEntry struct {
	name      string
	expiresAt time.Time
}

// client implements Service interface
type client struct {
	api      *slack.Client
	cacheTTL time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry

	teamURLOnce sync.Once
	teamURL     string
	teamURLErr  error

	botUserIDOnce sync.Once
	botUserID     string
	botUserIDErr  error
}

// Option is a functional option for client configuration
type Option func(*client)

// WithCacheTTL sets the TTL for channel name cache
func WithCacheTTL(ttl time.Duration) Option {
	return func(c *client) {
		c.cacheTTL = ttl
	}
}

// New creates a new Slack service with the provided bot token
func New(token string, opts ...Option) (Service, error) {
	if token == "" {
		return nil, goerr.New("Slack bot token is required")
	}

	c := &client{
		api:      slack.New(token),
		cacheTTL: DefaultCacheTTL,
		cache:    make(map[string]cacheEntry),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c, nil
}

// ListJoinedChannels retrieves the list of channels the bot has joined.
// If teamID is non-empty, only channels in that workspace are returned (for org-level apps).
func (c *client) ListJoinedChannels(ctx context.Context, teamID string) ([]Channel, error) {
	var channels []Channel
	var cursor string

	for {
		params := &slack.GetConversationsParameters{
			// TODO: Add "private_channel" support after resolving scope configuration
			// Requires: groups:read scope in addition to channels:read
			Types:           []string{"public_channel"},
			ExcludeArchived: true,
			Limit:           100,
			Cursor:          cursor,
			TeamID:          teamID,
		}

		convs, nextCursor, err := c.api.GetConversationsContext(ctx, params)
		if err != nil {
			// Handle rate limiting by waiting and retrying,
			// matching the pattern used by GetUsersContext in slack-go/slack.
			if rateLimitErr, ok := err.(*slack.RateLimitedError); ok {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(rateLimitErr.RetryAfter):
					continue
				}
			}
			return nil, goerr.Wrap(err, "failed to get conversations")
		}

		for _, conv := range convs {
			// Only include channels the bot is a member of
			if conv.IsMember {
				channels = append(channels, Channel{
					ID:   conv.ID,
					Name: conv.Name,
				})
			}
		}

		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	return channels, nil
}

// ListMemberChannels lists the non-archived channels userID belongs to that
// the bot can see, via users.conversations with the bot token.
func (c *client) ListMemberChannels(ctx context.Context, userID, teamID string) ([]Channel, error) {
	var channels []Channel
	var cursor string

	for {
		params := &slack.GetConversationsForUserParameters{
			UserID:          userID,
			Types:           []string{"public_channel", "private_channel"},
			ExcludeArchived: true,
			Limit:           200,
			Cursor:          cursor,
			TeamID:          teamID,
		}

		convs, nextCursor, err := c.api.GetConversationsForUserContext(ctx, params)
		if err != nil {
			var rateLimitErr *slack.RateLimitedError
			if errors.As(err, &rateLimitErr) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(rateLimitErr.RetryAfter):
					continue
				}
			}
			return nil, goerr.Wrap(err, "failed to get conversations for user",
				goerr.V("user_id", userID), goerr.V("team_id", teamID))
		}

		for _, conv := range convs {
			channels = append(channels, Channel{
				ID:        conv.ID,
				Name:      conv.Name,
				IsPrivate: conv.IsPrivate,
			})
		}

		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	return channels, nil
}

// JoinChannel makes the bot a member of a public channel. Slack answers ok
// with an already_in_channel warning when the bot is a member already.
func (c *client) JoinChannel(ctx context.Context, channelID string) error {
	if _, _, _, err := c.api.JoinConversationContext(ctx, channelID); err != nil {
		return goerr.Wrap(err, "failed to join Slack channel", goerr.V("channel_id", channelID))
	}
	return nil
}

// ErrChannelUnavailable is returned (wrapped) by GetChannelInfo when Slack
// answers that the channel does not exist or is not visible to the bot.
var ErrChannelUnavailable = goerr.New("slack channel is unavailable")

// channelUnavailableErrors are the conversations.info errors that mean the
// channel does not exist for this bot (deleted, or private without the bot).
var channelUnavailableErrors = map[string]bool{
	"channel_not_found": true,
	"not_in_channel":    true,
}

func isChannelUnavailable(err error) bool {
	var resp slack.SlackErrorResponse
	if errors.As(err, &resp) {
		return channelUnavailableErrors[resp.Err]
	}
	return false
}

// GetChannelNames retrieves channel names for the given IDs with caching
func (c *client) GetChannelNames(ctx context.Context, ids []string) (map[string]string, error) {
	result := make(map[string]string)
	var missingIDs []string

	now := time.Now()

	// Check cache first
	c.mu.RLock()
	for _, id := range ids {
		if entry, ok := c.cache[id]; ok && entry.expiresAt.After(now) {
			result[id] = entry.name
		} else {
			missingIDs = append(missingIDs, id)
		}
	}
	c.mu.RUnlock()

	// Fetch missing channels from API
	if len(missingIDs) > 0 {
		c.mu.Lock()
		defer c.mu.Unlock()

		for _, id := range missingIDs {
			// Double-check cache after acquiring write lock
			if entry, ok := c.cache[id]; ok && entry.expiresAt.After(now) {
				result[id] = entry.name
				continue
			}

			info, err := c.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
				ChannelID: id,
			})
			if err != nil {
				// A name is supplementary: the ID is left out of the result
				// and the caller falls back to the ID. A channel the bot
				// cannot see is an expected outcome; any other failure is
				// reported so an outage is not mistaken for "no name".
				if !isChannelUnavailable(err) {
					errutil.Handle(ctx, goerr.Wrap(err, "failed to get Slack channel info",
						goerr.V("channel_id", id)), "failed to resolve Slack channel name")
				}
				continue
			}

			name := info.Name
			result[id] = name
			c.cache[id] = cacheEntry{
				name:      name,
				expiresAt: now.Add(c.cacheTTL),
			}
		}
	}

	return result, nil
}

// GetUserInfo retrieves user information for the given user ID
func (c *client) GetUserInfo(ctx context.Context, userID string) (*User, error) {
	user, err := c.api.GetUserInfoContext(ctx, userID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get user info", goerr.V("user_id", userID))
	}

	return &User{
		ID:       user.ID,
		Name:     user.Name,
		RealName: resolveDisplayName(*user),
		Email:    user.Profile.Email,
		ImageURL: user.Profile.Image48,
		Locale:   user.Locale,
	}, nil
}

// ListUsers retrieves all non-deleted, non-bot users.
// For org-level apps, teamID is required per Slack API spec.
func (c *client) ListUsers(ctx context.Context, teamID string) ([]*User, error) {
	var opts []slack.GetUsersOption
	if teamID != "" {
		opts = append(opts, slack.GetUsersOptionTeamID(teamID))
	}
	users, err := c.api.GetUsersContext(ctx, opts...)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list users")
	}

	result := make([]*User, 0, len(users))
	for _, u := range users {
		// Skip deleted users and bots
		if u.Deleted || u.IsBot {
			continue
		}

		result = append(result, &User{
			ID:       u.ID,
			Name:     u.Name,
			RealName: resolveDisplayName(u),
			Email:    u.Profile.Email,
			ImageURL: u.Profile.Image48,
		})
	}

	return result, nil
}

// CreateChannel creates a new Slack channel for a case.
// The channel name is automatically generated from caseID, caseName, and the given prefix.
// If isPrivate is true, the channel is created as a private channel.
// If teamID is non-empty, the channel is created in the specified workspace (for org-level apps).
func (c *client) CreateChannel(ctx context.Context, caseID int64, caseName string, prefix string, isPrivate bool, teamID string) (string, error) {
	channelName := GenerateRiskChannelName(caseID, caseName, prefix)
	channel, err := c.api.CreateConversationContext(ctx, slack.CreateConversationParams{
		ChannelName: channelName,
		IsPrivate:   isPrivate,
		TeamID:      teamID,
	})
	if err != nil {
		return "", goerr.Wrap(err, "failed to create Slack channel", goerr.V("channelName", channelName), goerr.V("caseID", caseID), goerr.V("caseName", caseName))
	}
	return channel.ID, nil
}

// GetChannelInfo retrieves a channel descriptor (name, topic, purpose,
// privacy, member count, archive flag) via Slack's `conversations.info`.
// Cached fields like channel name are duplicated by GetChannelNames'
// cache; this method intentionally returns a fresh full payload because
// the LLM context build is a one-shot cost and topic / purpose change
// over time without observable invalidation events.
func (c *client) GetChannelInfo(ctx context.Context, channelID string) (*ChannelInfo, error) {
	info, err := c.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
		ChannelID:         channelID,
		IncludeNumMembers: true,
	})
	if err != nil {
		if isChannelUnavailable(err) {
			return nil, goerr.Wrap(ErrChannelUnavailable, "get channel info",
				goerr.V("channel_id", channelID),
				goerr.V("slack_error", err.Error()))
		}
		return nil, goerr.Wrap(err, "get channel info", goerr.V("channel_id", channelID))
	}
	out := &ChannelInfo{
		ID:         info.ID,
		Name:       info.Name,
		Topic:      info.Topic.Value,
		Purpose:    info.Purpose.Value,
		IsPrivate:  info.IsPrivate,
		IsArchived: info.IsArchived,
		IsShared:   info.IsShared || info.IsExtShared || info.IsOrgShared,
		IsMember:   info.IsMember,
		NumMembers: info.NumMembers,
		Creator:    info.Creator,
	}
	if info.Created != 0 {
		out.CreatedAt = time.Unix(int64(info.Created), 0).UTC()
	}
	return out, nil
}

// GetConversationMembers retrieves the list of user IDs in the given channel
func (c *client) GetConversationMembers(ctx context.Context, channelID string) ([]string, error) {
	var members []string
	var cursor string

	for {
		params := &slack.GetUsersInConversationParameters{
			ChannelID: channelID,
			Limit:     1000,
			Cursor:    cursor,
		}

		userIDs, nextCursor, err := c.api.GetUsersInConversationContext(ctx, params)
		if err != nil {
			if rateLimitErr, ok := err.(*slack.RateLimitedError); ok {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(rateLimitErr.RetryAfter):
					continue
				}
			}
			return nil, goerr.Wrap(err, "failed to get conversation members",
				goerr.V("channel_id", channelID))
		}

		members = append(members, userIDs...)

		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	return members, nil
}

// InviteUsersToChannel invites users to a Slack channel one user at a
// time and aggregates the per-user failures into a single error.
//
// Why not a single batch call: Slack's `conversations.invite` is
// atomic across the supplied user list. If even one ID is invalid
// (user_not_found, cant_invite_self, …) the entire call returns an
// error and NONE of the users are added. Real-world workspaces tend
// to accumulate stale auto-invite IDs over time, so a single bad ID
// in workspace config silently locks the reporter out of every newly
// created channel. Inviting per-user isolates each failure to the ID
// it concerns; the valid users still land in the channel.
//
// Per-user transient rate limits (slack.RateLimitedError) are
// respected once, honouring Slack's RetryAfter hint and the caller's
// ctx cancellation.
//
// The returned error (when non-nil) carries the channel ID and the
// list of failed user IDs / reasons under goerr values so callers can
// surface a structured summary to the operator. Errors are
// non-fatal at the activation layer (errutil.Handle), so the case
// still succeeds — but the failed users are now identifiable instead
// of being hidden inside an opaque atomic batch error.
func (c *client) InviteUsersToChannel(ctx context.Context, channelID string, userIDs []string) error {
	if len(userIDs) == 0 {
		return nil
	}

	var failedIDs []string
	var failureReasons []string
	for _, uid := range userIDs {
		if err := c.inviteOneUser(ctx, channelID, uid); err != nil {
			failedIDs = append(failedIDs, uid)
			failureReasons = append(failureReasons, err.Error())
		}
	}

	if len(failedIDs) == 0 {
		return nil
	}
	return goerr.New("failed to invite some users to Slack channel",
		goerr.V("channel_id", channelID),
		goerr.V("failed_user_ids", failedIDs),
		goerr.V("failure_reasons", failureReasons),
		goerr.V("attempted_user_ids", userIDs),
	)
}

// inviteOneUser performs a single-user invite with one rate-limit
// retry. Returns nil on success.
func (c *client) inviteOneUser(ctx context.Context, channelID, userID string) error {
	if _, err := c.api.InviteUsersToConversationContext(ctx, channelID, userID); err != nil {
		var rateLimitErr *slack.RateLimitedError
		if errors.As(err, &rateLimitErr) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(rateLimitErr.RetryAfter):
			}
			if _, retryErr := c.api.InviteUsersToConversationContext(ctx, channelID, userID); retryErr != nil {
				return retryErr
			}
			return nil
		}
		return err
	}
	return nil
}

// RenameChannel renames an existing Slack channel for a case
// The channel name is automatically generated from caseID, caseName, and the given prefix
func (c *client) RenameChannel(ctx context.Context, channelID string, caseID int64, caseName string, prefix string) error {
	channelName := GenerateRiskChannelName(caseID, caseName, prefix)
	_, err := c.api.RenameConversationContext(ctx, channelID, channelName)
	if err != nil {
		return goerr.Wrap(err, "failed to rename Slack channel", goerr.V("channelID", channelID), goerr.V("channelName", channelName), goerr.V("caseID", caseID), goerr.V("caseName", caseName))
	}
	return nil
}

// AddBookmark adds a link bookmark to a Slack channel
func (c *client) AddBookmark(ctx context.Context, channelID, title, link string) error {
	_, err := c.api.AddBookmarkContext(ctx, channelID, slack.AddBookmarkParameters{
		Title: title,
		Type:  "link",
		Link:  link,
	})
	if err != nil {
		return goerr.Wrap(err, "failed to add bookmark to Slack channel",
			goerr.V("channel_id", channelID),
			goerr.V("title", title),
			goerr.V("link", link))
	}
	return nil
}

// GetTeamURL retrieves the Slack workspace URL using auth.test API.
// The result is cached permanently (sync.Once) since the team URL does not change.
func (c *client) GetTeamURL(ctx context.Context) (string, error) {
	c.teamURLOnce.Do(func() {
		resp, err := c.api.AuthTestContext(ctx)
		if err != nil {
			c.teamURLErr = goerr.Wrap(err, "failed to call auth.test")
			return
		}
		c.teamURL = strings.TrimRight(resp.URL, "/")
	})
	return c.teamURL, c.teamURLErr
}

// withUnfurlDisabled appends the slack-go options that suppress Slack's link
// and media unfurling to the caller's options. The bot embeds permalinks
// (cases, actions, run records) and external URLs as inline labels in
// virtually every message it posts; the auto-generated preview cards are pure
// noise and push the actual content out of view. Unfurling is therefore
// disabled unconditionally on every bot-originated post / update, so it is
// baked into the client rather than left to each caller (a single forgotten
// call site would let a preview card slip through). The result slice is sized
// to len(opts)+2 up front so it is built in a single allocation.
func withUnfurlDisabled(opts ...slack.MsgOption) []slack.MsgOption {
	res := make([]slack.MsgOption, 0, len(opts)+2)
	res = append(res, opts...)
	return append(res,
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionDisableMediaUnfurl(),
	)
}

// PostMessage posts a Block Kit message to a channel and returns the message timestamp
func (c *client) PostMessage(ctx context.Context, channelID string, blocks []slack.Block, text string) (string, error) {
	msgOpts := withUnfurlDisabled(
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(text, false),
	)
	_, ts, err := c.api.PostMessageContext(ctx, channelID, msgOpts...)
	if err != nil {
		return "", wrapSlackPostError(err, "failed to post Slack message", channelID)
	}
	return ts, nil
}

// UpdateMessage updates an existing Block Kit message identified by channel and timestamp.
// It explicitly clears any attachments the message previously held. chat.update
// only overrides fields actually included in the request, so without this Slack
// would keep stale attachments from a prior shape (e.g. when migrating the slot
// renderer from attachments-based to blocks-only). Callers that need to retain
// attachments must use UpdateMessageWithAttachment(s) instead.
//
// Note: slack-go's MsgOptionAttachments treats a nil variadic as a no-op, so
// the empty *non-nil* slice spread is required to actually marshal
// "attachments=[]" on the wire.
func (c *client) UpdateMessage(ctx context.Context, channelID string, timestamp string, blocks []slack.Block, text string) error {
	msgOpts := withUnfurlDisabled(
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(text, false),
		slack.MsgOptionAttachments([]slack.Attachment{}...),
	)
	_, _, _, err := c.api.UpdateMessageContext(ctx, channelID, timestamp, msgOpts...)
	if err != nil {
		return goerr.Wrap(err, "failed to update Slack message",
			goerr.V("channel_id", channelID),
			goerr.V("timestamp", timestamp))
	}
	return nil
}

// PostMessageWithAttachment posts a message with top-level text plus a single
// attachment carrying Block Kit content. See the interface doc for why this
// shape is used (broadcast preview rendering).
func (c *client) PostMessageWithAttachment(ctx context.Context, channelID string, text string, attachment slack.Attachment) (string, error) {
	msgOpts := withUnfurlDisabled(
		slack.MsgOptionText(text, false),
		slack.MsgOptionAttachments(attachment),
	)
	_, ts, err := c.api.PostMessageContext(ctx, channelID, msgOpts...)
	if err != nil {
		return "", goerr.Wrap(err, "failed to post Slack message with attachment",
			goerr.V("channel_id", channelID))
	}
	return ts, nil
}

// PostMessageWithAttachments posts a message whose visible body lives entirely
// inside the supplied attachments. The text parameter is the fallback used for
// notification previews / clients without Block Kit rendering.
func (c *client) PostMessageWithAttachments(ctx context.Context, channelID string, text string, attachments []slack.Attachment) (string, error) {
	msgOpts := withUnfurlDisabled(
		slack.MsgOptionText(text, false),
		slack.MsgOptionAttachments(attachments...),
	)
	_, ts, err := c.api.PostMessageContext(ctx, channelID, msgOpts...)
	if err != nil {
		return "", goerr.Wrap(err, "failed to post Slack message with attachments",
			goerr.V("channel_id", channelID),
			goerr.V("attachment_count", len(attachments)),
		)
	}
	return ts, nil
}

// UpdateMessageWithAttachments updates an attachments-only message. Slack
// replaces the attachments array wholesale, so callers must pass the full
// desired set each call.
func (c *client) UpdateMessageWithAttachments(ctx context.Context, channelID string, timestamp string, text string, attachments []slack.Attachment) error {
	msgOpts := withUnfurlDisabled(
		slack.MsgOptionText(text, false),
		slack.MsgOptionAttachments(attachments...),
	)
	_, _, _, err := c.api.UpdateMessageContext(ctx, channelID, timestamp, msgOpts...)
	if err != nil {
		return goerr.Wrap(err, "failed to update Slack message with attachments",
			goerr.V("channel_id", channelID),
			goerr.V("timestamp", timestamp),
			goerr.V("attachment_count", len(attachments)),
		)
	}
	return nil
}

// UpdateMessageWithAttachment updates a message previously posted via
// PostMessageWithAttachment, preserving the top-level-text + single-attachment
// shape so the broadcast-preview source stays intact across refreshes.
func (c *client) UpdateMessageWithAttachment(ctx context.Context, channelID string, timestamp string, text string, attachment slack.Attachment) error {
	msgOpts := withUnfurlDisabled(
		slack.MsgOptionText(text, false),
		slack.MsgOptionAttachments(attachment),
	)
	_, _, _, err := c.api.UpdateMessageContext(ctx, channelID, timestamp, msgOpts...)
	if err != nil {
		return goerr.Wrap(err, "failed to update Slack message with attachment",
			goerr.V("channel_id", channelID),
			goerr.V("timestamp", timestamp))
	}
	return nil
}

// GetConversationReplies retrieves messages from a thread
func (c *client) GetConversationReplies(ctx context.Context, channelID string, threadTS string, limit int) ([]ConversationMessage, error) {
	params := &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     limit,
	}

	msgs, _, _, err := c.api.GetConversationRepliesContext(ctx, params)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get conversation replies",
			goerr.V("channel_id", channelID),
			goerr.V("thread_ts", threadTS))
	}

	return ToConversationMessages(msgs), nil
}

// GetConversationHistory retrieves channel messages from the specified time
func (c *client) GetConversationHistory(ctx context.Context, channelID string, oldest time.Time, limit int) ([]ConversationMessage, error) {
	params := &slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Oldest:    fmt.Sprintf("%d.000000", oldest.Unix()),
		Limit:     limit,
	}

	resp, err := c.api.GetConversationHistoryContext(ctx, params)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get conversation history",
			goerr.V("channel_id", channelID),
			goerr.V("oldest", oldest))
	}

	return ToConversationMessages(resp.Messages), nil
}

// ToConversationMessages converts slack-go messages to ConversationMessage.
// The body goes through MessageBody because an integration such as the GitHub
// Slack app renders its whole notification into `attachments` and leaves
// `text` empty — reading msg.Text alone yields "" for those messages.
//
// Exported because the agent's User-token client (pkg/agent/tool/slack) reads
// the same endpoints with its own slack-go client and must not diverge on how
// a message body is derived.
func ToConversationMessages(msgs []slack.Message) []ConversationMessage {
	result := make([]ConversationMessage, 0, len(msgs))
	for _, msg := range msgs {
		result = append(result, ConversationMessage{
			UserID:    msg.User,
			UserName:  msg.Username,
			Text:      MessageBody(msg.Text, msg.Blocks, msg.Attachments),
			Timestamp: msg.Timestamp,
			ThreadTS:  msg.ThreadTimestamp,
		})
	}
	return result
}

// PostThreadReply posts a text message as a thread reply and returns the message timestamp
func (c *client) PostThreadReply(ctx context.Context, channelID string, threadTS string, text string) (string, error) {
	msgOpts := withUnfurlDisabled(
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(threadTS),
	)
	_, ts, err := c.api.PostMessageContext(ctx, channelID, msgOpts...)
	if err != nil {
		return "", wrapSlackPostError(err, "failed to post thread reply", channelID,
			goerr.V("thread_ts", threadTS))
	}
	return ts, nil
}

// PostThreadMessage posts a Block Kit message as a thread reply and returns the message timestamp.
// Optional PostThreadOption values (e.g. WithBroadcastToChannel) tweak the underlying chat.postMessage call.
func (c *client) PostThreadMessage(ctx context.Context, channelID string, threadTS string, blocks []slack.Block, text string, opts ...PostThreadOption) (string, error) {
	cfg := ApplyPostThreadOptions(opts...)
	msgOpts := withUnfurlDisabled(
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(threadTS),
	)
	if cfg.Broadcast {
		msgOpts = append(msgOpts, slack.MsgOptionBroadcast())
	}
	_, ts, err := c.api.PostMessageContext(ctx, channelID, msgOpts...)
	if err != nil {
		return "", wrapSlackPostError(err, "failed to post thread message", channelID,
			goerr.V("thread_ts", threadTS),
			goerr.V("broadcast", cfg.Broadcast))
	}
	return ts, nil
}

// OpenView opens a modal view in Slack using the provided trigger ID
func (c *client) OpenView(ctx context.Context, triggerID string, view slack.ModalViewRequest) error {
	_, err := c.api.OpenViewContext(ctx, triggerID, view)
	if err != nil {
		return wrapSlackViewError(err, "failed to open Slack modal view", triggerID)
	}
	return nil
}

// UpdateView replaces an already-open modal in place. The caller may pass
// viewID (returned when the modal was opened) or externalID (set on the
// view at open time); either one is sufficient to address the modal. hash
// is optional — non-empty hashes let Slack reject the update if the user
// has interacted with the view in the meantime.
func (c *client) UpdateView(ctx context.Context, view slack.ModalViewRequest, externalID, hash, viewID string) error {
	_, err := c.api.UpdateViewContext(ctx, view, externalID, hash, viewID)
	if err != nil {
		return wrapSlackViewError(err, "failed to update Slack modal view", viewID)
	}
	return nil
}

// wrapSlackViewError wraps a views.* failure with the structured detail
// Slack returns in response_metadata. The default goerr.Wrap path only
// captures the top-level error code (e.g. "invalid_arguments"), so by the
// time the failure reaches Sentry the JSON-pointer to the offending field
// is gone. Pulling SlackErrorResponse out of the chain and attaching
// response_metadata.messages / warnings makes the next occurrence
// diagnosable without an API replay.
func wrapSlackViewError(err error, msg, triggerID string) error {
	opts := []goerr.Option{
		goerr.V("trigger_id", triggerID),
	}
	var se slack.SlackErrorResponse
	if errors.As(err, &se) {
		opts = append(opts,
			goerr.V("slack_error", se.Err),
			goerr.V("slack_response_messages", se.ResponseMetadata.Messages),
			goerr.V("slack_response_warnings", se.ResponseMetadata.Warnings),
			// se.Errors carries the per-failure detail when Slack rejects
			// a structured payload (e.g. apps.manifest, conversations.invite).
			// For views.open the slice is usually empty, but on the rare
			// occasion it is populated it pins the exact failing item, so
			// surface it directly rather than only its length.
			goerr.V("slack_response_errors", se.Errors),
		)
	}
	return goerr.Wrap(err, msg, opts...)
}

// wrapSlackPostError wraps a chat.postMessage-family failure. Beyond the
// diagnostic goerr values (mirroring wrapSlackViewError), it classifies the
// Slack error code into a uierr.UserFacing and attaches it at this origin, so
// the user-facing renderer can turn e.g. not_in_channel into an actionable
// "invite the bot" message instead of the old generic failure text. extra
// carries call-specific diagnostic values (thread_ts, broadcast, …).
func wrapSlackPostError(err error, msg, channelID string, extra ...goerr.Option) error {
	opts := append([]goerr.Option{goerr.V("channel_id", channelID)}, extra...)
	var se slack.SlackErrorResponse
	if errors.As(err, &se) {
		opts = append(opts,
			goerr.V("slack_error", se.Err),
			goerr.V("slack_response_messages", se.ResponseMetadata.Messages),
			goerr.V("slack_response_warnings", se.ResponseMetadata.Warnings),
		)
		if uf, ok := uierr.SlackError(se.Err); ok {
			opts = append(opts, uierr.Attach(uf))
		}
	}
	return goerr.Wrap(err, msg, opts...)
}

// GetBotUserID retrieves the bot's own user ID via auth.test API.
// The result is cached permanently (sync.Once) since the bot user ID does not change.
func (c *client) GetBotUserID(ctx context.Context) (string, error) {
	c.botUserIDOnce.Do(func() {
		resp, err := c.api.AuthTestContext(ctx)
		if err != nil {
			c.botUserIDErr = goerr.Wrap(err, "failed to call auth.test for bot user ID",
				uierr.Attach(uierr.UserFacing{
					Kind:        uierr.KindPermission,
					What:        i18n.MsgUIErrSlackAuthWhat,
					Detail:      i18n.MsgUIErrSlackAuthDetail,
					Remediation: i18n.MsgUIErrSlackAuthFix,
					Cause:       "auth.test",
				}))
			return
		}
		c.botUserID = resp.UserID
	})
	return c.botUserID, c.botUserIDErr
}

// ListUserGroups retrieves all user groups in the workspace.
// If teamID is non-empty, only groups in that workspace are returned (for org-level apps).
func (c *client) ListUserGroups(ctx context.Context, teamID string) ([]UserGroup, error) {
	var opts []slack.GetUserGroupsOption
	if teamID != "" {
		opts = append(opts, slack.GetUserGroupsOptionTeamID(teamID))
	}
	groups, err := c.api.GetUserGroupsContext(ctx, opts...)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list user groups")
	}

	result := make([]UserGroup, 0, len(groups))
	for _, g := range groups {
		result = append(result, UserGroup{
			ID:     g.ID,
			Handle: g.Handle,
			Name:   g.Name,
		})
	}
	return result, nil
}

// ListTeams returns all workspaces accessible by the bot token.
// For org-level apps, this returns all workspaces in the enterprise.
func (c *client) ListTeams(ctx context.Context) ([]Team, error) {
	var teams []Team
	var cursor string

	for {
		params := slack.ListTeamsParameters{
			Cursor: cursor,
		}
		slackTeams, nextCursor, err := c.api.ListTeamsContext(ctx, params)
		if err != nil {
			return nil, goerr.Wrap(err, "failed to list teams")
		}

		for _, t := range slackTeams {
			teams = append(teams, Team{
				ID:   t.ID,
				Name: t.Name,
			})
		}

		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	return teams, nil
}

// GetUserGroupMembers retrieves the member user IDs of a user group
func (c *client) GetUserGroupMembers(ctx context.Context, groupID string) ([]string, error) {
	members, err := c.api.GetUserGroupMembersContext(ctx, groupID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get user group members",
			goerr.V("group_id", groupID))
	}
	return members, nil
}

// PostEphemeral posts an ephemeral message visible only to the specified user in a channel.
// Optional PostEphemeralOption values (e.g. WithEphemeralThread) tweak the underlying chat.postEphemeral call.
func (c *client) PostEphemeral(ctx context.Context, channelID string, userID string, text string, opts ...PostEphemeralOption) error {
	cfg := ApplyPostEphemeralOptions(opts...)
	msgOpts := withUnfurlDisabled(
		slack.MsgOptionText(text, false),
	)
	if cfg.ThreadTS != "" {
		msgOpts = append(msgOpts, slack.MsgOptionTS(cfg.ThreadTS))
	}
	_, err := c.api.PostEphemeralContext(ctx, channelID, userID, msgOpts...)
	if err != nil {
		return goerr.Wrap(err, "failed to post ephemeral message",
			goerr.V("channel_id", channelID),
			goerr.V("user_id", userID),
			goerr.V("thread_ts", cfg.ThreadTS))
	}
	return nil
}

// PostEphemeralBlocks posts an ephemeral Block Kit message and returns the message timestamp.
func (c *client) PostEphemeralBlocks(ctx context.Context, channelID string, userID string, blocks []slack.Block, text string) (string, error) {
	msgOpts := withUnfurlDisabled(
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(text, false),
	)
	ts, err := c.api.PostEphemeralContext(ctx, channelID, userID, msgOpts...)
	if err != nil {
		return "", goerr.Wrap(err, "failed to post ephemeral block message",
			goerr.V("channel_id", channelID),
			goerr.V("user_id", userID))
	}
	return ts, nil
}

// GetPermalink retrieves the permalink for a specific message.
func (c *client) GetPermalink(ctx context.Context, channelID string, messageTS string) (string, error) {
	link, err := c.api.GetPermalinkContext(ctx, &slack.PermalinkParameters{
		Channel: channelID,
		Ts:      messageTS,
	})
	if err != nil {
		return "", goerr.Wrap(err, "failed to get permalink",
			goerr.V("channel_id", channelID),
			goerr.V("message_ts", messageTS))
	}
	return link, nil
}
