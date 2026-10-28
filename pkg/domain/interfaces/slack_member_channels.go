package interfaces

import (
	"context"
	"time"

	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

// SlackMemberChannelsRepository persists, per workspace and Slack user, the
// list of channels the user belongs to, so the existing-channel picker can
// search it without listing every channel from Slack on each request.
type SlackMemberChannelsRepository interface {
	// Get returns the stored list. Returns the backend's ErrNotFound
	// (memory.ErrNotFound / firestore.ErrNotFound) when none is stored.
	Get(ctx context.Context, workspaceID, userID string) (*model.SlackMemberChannels, error)

	// Set replaces the stored list wholesale (Validate then persist). A
	// reader never observes a mix of two lists. Concurrent writers are not
	// coordinated: the later write wins.
	Set(ctx context.Context, channels *model.SlackMemberChannels) error

	// ClaimRefresh records a refresh claim at now and returns true unless a
	// claim younger than lease already exists, in which case it changes
	// nothing and returns false. The check and the write are atomic across
	// instances.
	ClaimRefresh(ctx context.Context, workspaceID, userID string, now time.Time, lease time.Duration) (bool, error)
}
