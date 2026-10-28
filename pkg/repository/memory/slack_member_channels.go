package memory

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

type slackMemberChannelsKey struct {
	workspaceID string
	userID      string
}

// slackMemberChannelsRepository stores one channel list and one refresh claim
// per (workspace ID, Slack user ID).
type slackMemberChannelsRepository struct {
	mu     sync.Mutex
	lists  map[slackMemberChannelsKey]*model.SlackMemberChannels
	claims map[slackMemberChannelsKey]*model.SlackMemberChannelsRefresh
}

var _ interfaces.SlackMemberChannelsRepository = &slackMemberChannelsRepository{}

func newSlackMemberChannelsRepository() *slackMemberChannelsRepository {
	return &slackMemberChannelsRepository{
		lists:  make(map[slackMemberChannelsKey]*model.SlackMemberChannels),
		claims: make(map[slackMemberChannelsKey]*model.SlackMemberChannelsRefresh),
	}
}

// copySlackMemberChannels copies the whole struct first, so a newly added
// field is never silently dropped, then clones the only reference-typed field.
func copySlackMemberChannels(m *model.SlackMemberChannels) *model.SlackMemberChannels {
	copied := *m
	copied.Channels = slices.Clone(m.Channels)
	return &copied
}

func (r *slackMemberChannelsRepository) Get(ctx context.Context, workspaceID, userID string) (*model.SlackMemberChannels, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	list, ok := r.lists[slackMemberChannelsKey{workspaceID, userID}]
	if !ok {
		return nil, goerr.Wrap(ErrNotFound, "slack member channels not found",
			goerr.V("workspace_id", workspaceID), goerr.V("user_id", userID))
	}
	return copySlackMemberChannels(list), nil
}

func (r *slackMemberChannelsRepository) Set(ctx context.Context, channels *model.SlackMemberChannels) error {
	if err := channels.Validate(); err != nil {
		return goerr.Wrap(err, "slack member channels validation failed before set")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.lists[slackMemberChannelsKey{channels.WorkspaceID, channels.UserID}] = copySlackMemberChannels(channels)
	return nil
}

func (r *slackMemberChannelsRepository) ClaimRefresh(ctx context.Context, workspaceID, userID string, now time.Time, lease time.Duration) (bool, error) {
	claim := &model.SlackMemberChannelsRefresh{WorkspaceID: workspaceID, UserID: userID, ClaimedAt: now}
	if err := claim.Validate(); err != nil {
		return false, goerr.Wrap(err, "slack member channels refresh validation failed before claim")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	key := slackMemberChannelsKey{workspaceID, userID}
	if held, ok := r.claims[key]; ok && now.Sub(held.ClaimedAt) < lease {
		return false, nil
	}
	r.claims[key] = claim
	return true, nil
}
