package firestore

import (
	"context"
	"slices"
	"strconv"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// slackMemberChannelsPageSize bounds one page document. A channel entry is
	// an ID, a name of at most 80 characters and a flag, so 2,000 entries stay
	// far below Firestore's 1 MiB document limit.
	slackMemberChannelsPageSize = 2000
	// slackMemberChannelsMaxPages keeps Set within one transaction, which
	// accepts at most 500 writes.
	slackMemberChannelsMaxPages = 400
)

type slackMemberChannelsRepository struct {
	client *firestore.Client
}

var _ interfaces.SlackMemberChannelsRepository = &slackMemberChannelsRepository{}

func newSlackMemberChannelsRepository(client *firestore.Client) *slackMemberChannelsRepository {
	return &slackMemberChannelsRepository{client: client}
}

// pages returns the collection holding one user's list.
// Path: workspaces/{workspaceID}/slackMemberChannels/{userID}/pages/{index}
//
// A user can belong to more channels than one document holds, so the list is
// split into pages. Each page is a *model.SlackMemberChannels carrying a
// consecutive slice of Channels and the list's WorkspaceID, UserID and
// FetchedAt; the document ID is the page index. Set rewrites every page in one
// transaction and Get reads them in one, so a reader never mixes two lists.
func (r *slackMemberChannelsRepository) pages(workspaceID, userID string) *firestore.CollectionRef {
	return r.client.Collection("workspaces").Doc(workspaceID).
		Collection("slackMemberChannels").Doc(userID).
		Collection("pages")
}

// claimRef returns the refresh claim of one user's list.
// Path: workspaces/{workspaceID}/slackMemberChannelRefreshes/{userID}
func (r *slackMemberChannelsRepository) claimRef(workspaceID, userID string) *firestore.DocumentRef {
	return r.client.Collection("workspaces").Doc(workspaceID).
		Collection("slackMemberChannelRefreshes").Doc(userID)
}

func (r *slackMemberChannelsRepository) Get(ctx context.Context, workspaceID, userID string) (*model.SlackMemberChannels, error) {
	type page struct {
		index int
		data  model.SlackMemberChannels
	}
	var pages []page

	err := r.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		pages = pages[:0]
		snaps, err := tx.Documents(r.pages(workspaceID, userID)).GetAll()
		if err != nil {
			return goerr.Wrap(err, "failed to read slack member channel pages")
		}
		for _, snap := range snaps {
			index, err := strconv.Atoi(snap.Ref.ID)
			if err != nil {
				return goerr.Wrap(err, "unexpected slack member channel page id", goerr.V("page_id", snap.Ref.ID))
			}
			var p page
			p.index = index
			if err := snap.DataTo(&p.data); err != nil {
				return goerr.Wrap(err, "failed to decode slack member channel page", goerr.V("page_id", snap.Ref.ID))
			}
			pages = append(pages, p)
		}
		return nil
	}, firestore.ReadOnly)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get slack member channels",
			goerr.V("workspace_id", workspaceID), goerr.V("user_id", userID))
	}
	if len(pages) == 0 {
		return nil, goerr.Wrap(ErrNotFound, "slack member channels not found",
			goerr.V("workspace_id", workspaceID), goerr.V("user_id", userID))
	}

	slices.SortFunc(pages, func(a, b page) int { return a.index - b.index })
	result := pages[0].data
	result.Channels = nil
	for _, p := range pages {
		result.Channels = append(result.Channels, p.data.Channels...)
	}
	return &result, nil
}

func (r *slackMemberChannelsRepository) Set(ctx context.Context, channels *model.SlackMemberChannels) error {
	if err := channels.Validate(); err != nil {
		return goerr.Wrap(err, "slack member channels validation failed before set")
	}

	// An empty list still writes page 0, so FetchedAt records that the user
	// was listed and has no channels.
	chunks := [][]model.SlackMemberChannel{nil}
	if len(channels.Channels) > 0 {
		chunks = slices.Collect(slices.Chunk(channels.Channels, slackMemberChannelsPageSize))
	}
	if len(chunks) > slackMemberChannelsMaxPages {
		return goerr.New("too many slack member channels to store",
			goerr.V("workspace_id", channels.WorkspaceID), goerr.V("user_id", channels.UserID),
			goerr.V("channels", len(channels.Channels)), goerr.V("max", slackMemberChannelsPageSize*slackMemberChannelsMaxPages))
	}

	col := r.pages(channels.WorkspaceID, channels.UserID)
	err := r.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		existing, err := tx.Documents(col).GetAll()
		if err != nil {
			return goerr.Wrap(err, "failed to read existing slack member channel pages")
		}
		for i, chunk := range chunks {
			p := *channels
			p.Channels = chunk
			if err := tx.Set(col.Doc(strconv.Itoa(i)), &p); err != nil {
				return goerr.Wrap(err, "failed to set slack member channel page", goerr.V("page", i))
			}
		}
		// A shorter list than the stored one leaves pages past its end.
		for _, snap := range existing {
			index, err := strconv.Atoi(snap.Ref.ID)
			if err == nil && index < len(chunks) {
				continue
			}
			if err := tx.Delete(snap.Ref); err != nil {
				return goerr.Wrap(err, "failed to delete stale slack member channel page", goerr.V("page_id", snap.Ref.ID))
			}
		}
		return nil
	})
	if err != nil {
		return goerr.Wrap(err, "failed to set slack member channels",
			goerr.V("workspace_id", channels.WorkspaceID), goerr.V("user_id", channels.UserID))
	}
	return nil
}

func (r *slackMemberChannelsRepository) ClaimRefresh(ctx context.Context, workspaceID, userID string, now time.Time, lease time.Duration) (bool, error) {
	claim := &model.SlackMemberChannelsRefresh{WorkspaceID: workspaceID, UserID: userID, ClaimedAt: now}
	if err := claim.Validate(); err != nil {
		return false, goerr.Wrap(err, "slack member channels refresh validation failed before claim")
	}

	ref := r.claimRef(workspaceID, userID)
	claimed := false
	err := r.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		claimed = false
		snap, err := tx.Get(ref)
		if err != nil && status.Code(err) != codes.NotFound {
			return goerr.Wrap(err, "failed to read slack member channels refresh claim")
		}
		if err == nil {
			var held model.SlackMemberChannelsRefresh
			if err := snap.DataTo(&held); err != nil {
				return goerr.Wrap(err, "failed to decode slack member channels refresh claim")
			}
			if now.Sub(held.ClaimedAt) < lease {
				return nil
			}
		}
		if err := tx.Set(ref, claim); err != nil {
			return goerr.Wrap(err, "failed to write slack member channels refresh claim")
		}
		claimed = true
		return nil
	})
	if err != nil {
		return false, goerr.Wrap(err, "failed to claim slack member channels refresh",
			goerr.V("workspace_id", workspaceID), goerr.V("user_id", userID))
	}
	return claimed, nil
}
