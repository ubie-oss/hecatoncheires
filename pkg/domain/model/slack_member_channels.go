package model

import (
	"time"

	"github.com/m-mizutani/goerr/v2"
)

// ErrSlackMemberChannelsValidation is returned when a SlackMemberChannels or a
// SlackMemberChannelsRefresh fails validation.
var ErrSlackMemberChannelsValidation = goerr.New("slack member channels validation failed")

// SlackMemberChannel is one channel in a SlackMemberChannels list.
type SlackMemberChannel struct {
	// ID is the Slack channel ID. Required.
	ID string
	// Name is the channel name without the leading "#".
	Name string
	// IsPrivate is true for a private channel.
	IsPrivate bool
}

// SlackMemberChannels is the list of Slack channels one Slack user belongs to,
// as users.conversations returned it for the Slack team of one workspace, at
// FetchedAt. One list per (WorkspaceID, UserID).
//
// It is derived data: losing it costs one more users.conversations listing,
// nothing more. It exists because that listing pages through every channel
// the user has joined, which is too slow to repeat on every keystroke of the
// channel picker.
type SlackMemberChannels struct {
	// WorkspaceID is the workspace whose Slack team was listed. Required.
	WorkspaceID string
	// UserID is the Slack user whose channels were listed. Required.
	UserID string
	// Channels may be empty (the user has joined nothing the bot can see).
	Channels []SlackMemberChannel
	// FetchedAt is when Slack returned Channels. Required.
	FetchedAt time.Time
}

// IsFresh reports whether the list was fetched within window before now. The
// boundary counts as stale so a refresh is not deferred another window.
func (m *SlackMemberChannels) IsFresh(now time.Time, window time.Duration) bool {
	if m == nil || m.FetchedAt.IsZero() {
		return false
	}
	return now.Sub(m.FetchedAt) < window
}

// Validate enforces the invariants the repository relies on before every
// write.
func (m *SlackMemberChannels) Validate() error {
	if m == nil {
		return goerr.Wrap(ErrSlackMemberChannelsValidation, "slack member channels is nil")
	}
	if m.WorkspaceID == "" {
		return goerr.Wrap(ErrSlackMemberChannelsValidation, "workspace id is required")
	}
	if m.UserID == "" {
		return goerr.Wrap(ErrSlackMemberChannelsValidation, "user id is required",
			goerr.V("workspace_id", m.WorkspaceID))
	}
	if m.FetchedAt.IsZero() {
		return goerr.Wrap(ErrSlackMemberChannelsValidation, "fetched at is required",
			goerr.V("workspace_id", m.WorkspaceID), goerr.V("user_id", m.UserID))
	}
	for i, ch := range m.Channels {
		if ch.ID == "" {
			return goerr.Wrap(ErrSlackMemberChannelsValidation, "channel id must not be empty",
				goerr.V("workspace_id", m.WorkspaceID), goerr.V("user_id", m.UserID), goerr.V("index", i))
		}
	}
	return nil
}

// SlackMemberChannelsRefresh records that one caller took on refreshing the
// SlackMemberChannels of (WorkspaceID, UserID) at ClaimedAt. Another caller
// that finds a claim younger than its lease leaves the refresh to the
// claimant, so a burst of searches over a stale list starts one listing, not
// one per search.
type SlackMemberChannelsRefresh struct {
	// WorkspaceID and UserID identify the list being refreshed. Required.
	WorkspaceID string
	UserID      string
	// ClaimedAt is when the refresh was claimed. Required.
	ClaimedAt time.Time
}

// Validate enforces the invariants the repository relies on before every
// write.
func (r *SlackMemberChannelsRefresh) Validate() error {
	if r == nil {
		return goerr.Wrap(ErrSlackMemberChannelsValidation, "slack member channels refresh is nil")
	}
	if r.WorkspaceID == "" || r.UserID == "" {
		return goerr.Wrap(ErrSlackMemberChannelsValidation, "workspace id and user id are required",
			goerr.V("workspace_id", r.WorkspaceID), goerr.V("user_id", r.UserID))
	}
	if r.ClaimedAt.IsZero() {
		return goerr.Wrap(ErrSlackMemberChannelsValidation, "claimed at is required",
			goerr.V("workspace_id", r.WorkspaceID), goerr.V("user_id", r.UserID))
	}
	return nil
}
