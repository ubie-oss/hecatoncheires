package repository_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/firestore"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
)

func manyMemberChannels(n int) []model.SlackMemberChannel {
	channels := make([]model.SlackMemberChannel, n)
	for i := range channels {
		channels[i] = model.SlackMemberChannel{
			ID:        fmt.Sprintf("C%06d", i),
			Name:      fmt.Sprintf("channel-%06d", i),
			IsPrivate: i%7 == 0,
		}
	}
	return channels
}

func runSlackMemberChannelsRepositoryTest(t *testing.T, newRepo func(t *testing.T) interfaces.Repository) {
	t.Helper()

	isNotFound := func(err error) bool {
		return errors.Is(err, memory.ErrNotFound) || errors.Is(err, firestore.ErrNotFound)
	}

	repo := newRepo(t)
	newKey := func() (string, string) {
		n := time.Now().UnixNano()
		return fmt.Sprintf("ws-%d", n), fmt.Sprintf("U%d", n)
	}

	t.Run("Set and Get round-trips all fields", func(t *testing.T) {
		ctx := context.Background()
		workspaceID, userID := newKey()
		fetchedAt := time.Now().UTC().Truncate(time.Millisecond)
		input := &model.SlackMemberChannels{
			WorkspaceID: workspaceID,
			UserID:      userID,
			Channels: []model.SlackMemberChannel{
				{ID: "C-PUB", Name: "general", IsPrivate: false},
				{ID: "C-PRIV", Name: "secret", IsPrivate: true},
			},
			FetchedAt: fetchedAt,
		}
		gt.NoError(t, repo.SlackMemberChannels().Set(ctx, input)).Required()

		got, err := repo.SlackMemberChannels().Get(ctx, workspaceID, userID)
		gt.NoError(t, err).Required()
		gt.String(t, got.WorkspaceID).Equal(workspaceID)
		gt.String(t, got.UserID).Equal(userID)
		gt.Value(t, got.Channels).Equal([]model.SlackMemberChannel{
			{ID: "C-PUB", Name: "general", IsPrivate: false},
			{ID: "C-PRIV", Name: "secret", IsPrivate: true},
		})
		gt.Bool(t, got.FetchedAt.Equal(fetchedAt)).True()
	})

	t.Run("Get returns not found when nothing is stored", func(t *testing.T) {
		workspaceID, userID := newKey()
		_, err := repo.SlackMemberChannels().Get(context.Background(), workspaceID, userID)
		gt.Bool(t, isNotFound(err)).True()
	})

	t.Run("an empty list is stored with its FetchedAt", func(t *testing.T) {
		ctx := context.Background()
		workspaceID, userID := newKey()
		fetchedAt := time.Now().UTC().Truncate(time.Millisecond)
		gt.NoError(t, repo.SlackMemberChannels().Set(ctx, &model.SlackMemberChannels{
			WorkspaceID: workspaceID, UserID: userID, FetchedAt: fetchedAt,
		})).Required()

		got, err := repo.SlackMemberChannels().Get(ctx, workspaceID, userID)
		gt.NoError(t, err).Required()
		gt.Array(t, got.Channels).Length(0)
		gt.Bool(t, got.FetchedAt.Equal(fetchedAt)).True()
	})

	t.Run("a list larger than one page keeps every channel in order", func(t *testing.T) {
		ctx := context.Background()
		workspaceID, userID := newKey()
		channels := manyMemberChannels(4501)
		gt.NoError(t, repo.SlackMemberChannels().Set(ctx, &model.SlackMemberChannels{
			WorkspaceID: workspaceID, UserID: userID, Channels: channels, FetchedAt: time.Now().UTC(),
		})).Required()

		got, err := repo.SlackMemberChannels().Get(ctx, workspaceID, userID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.Channels).Equal(channels)
	})

	t.Run("Set replaces a longer list without leaving its tail", func(t *testing.T) {
		ctx := context.Background()
		workspaceID, userID := newKey()
		gt.NoError(t, repo.SlackMemberChannels().Set(ctx, &model.SlackMemberChannels{
			WorkspaceID: workspaceID, UserID: userID, Channels: manyMemberChannels(4501), FetchedAt: time.Now().UTC(),
		})).Required()

		second := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
		gt.NoError(t, repo.SlackMemberChannels().Set(ctx, &model.SlackMemberChannels{
			WorkspaceID: workspaceID, UserID: userID,
			Channels:  []model.SlackMemberChannel{{ID: "C-ONLY", Name: "only"}},
			FetchedAt: second,
		})).Required()

		got, err := repo.SlackMemberChannels().Get(ctx, workspaceID, userID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.Channels).Equal([]model.SlackMemberChannel{{ID: "C-ONLY", Name: "only"}})
		gt.Bool(t, got.FetchedAt.Equal(second)).True()
	})

	t.Run("lists are kept per workspace and user", func(t *testing.T) {
		ctx := context.Background()
		workspaceID, userID := newKey()
		gt.NoError(t, repo.SlackMemberChannels().Set(ctx, &model.SlackMemberChannels{
			WorkspaceID: workspaceID, UserID: userID,
			Channels: []model.SlackMemberChannel{{ID: "C-A", Name: "a"}}, FetchedAt: time.Now().UTC(),
		})).Required()

		_, err := repo.SlackMemberChannels().Get(ctx, workspaceID, userID+"-other")
		gt.Bool(t, isNotFound(err)).True()
		_, err = repo.SlackMemberChannels().Get(ctx, workspaceID+"-other", userID)
		gt.Bool(t, isNotFound(err)).True()
	})

	t.Run("mutating the input after Set does not alter stored state", func(t *testing.T) {
		ctx := context.Background()
		workspaceID, userID := newKey()
		input := &model.SlackMemberChannels{
			WorkspaceID: workspaceID, UserID: userID,
			Channels: []model.SlackMemberChannel{{ID: "C-A", Name: "a"}}, FetchedAt: time.Now().UTC(),
		}
		gt.NoError(t, repo.SlackMemberChannels().Set(ctx, input)).Required()
		input.Channels[0].Name = "tampered"

		got, err := repo.SlackMemberChannels().Get(ctx, workspaceID, userID)
		gt.NoError(t, err).Required()
		gt.String(t, got.Channels[0].Name).Equal("a")
	})

	t.Run("Set rejects an invalid list", func(t *testing.T) {
		workspaceID, userID := newKey()
		err := repo.SlackMemberChannels().Set(context.Background(), &model.SlackMemberChannels{
			WorkspaceID: workspaceID, UserID: userID,
		})
		gt.Error(t, err).Is(model.ErrSlackMemberChannelsValidation)
	})

	t.Run("ClaimRefresh grants one claim per lease", func(t *testing.T) {
		ctx := context.Background()
		workspaceID, userID := newKey()
		now := time.Now().UTC().Truncate(time.Millisecond)
		lease := 15 * time.Minute

		claimed, err := repo.SlackMemberChannels().ClaimRefresh(ctx, workspaceID, userID, now, lease)
		gt.NoError(t, err).Required()
		gt.Bool(t, claimed).True()

		claimed, err = repo.SlackMemberChannels().ClaimRefresh(ctx, workspaceID, userID, now.Add(lease-time.Second), lease)
		gt.NoError(t, err).Required()
		gt.Bool(t, claimed).False()

		// Another user's list has its own claim.
		claimed, err = repo.SlackMemberChannels().ClaimRefresh(ctx, workspaceID, userID+"-other", now, lease)
		gt.NoError(t, err).Required()
		gt.Bool(t, claimed).True()

		// Once the lease has passed, the next caller takes the claim over.
		claimed, err = repo.SlackMemberChannels().ClaimRefresh(ctx, workspaceID, userID, now.Add(lease), lease)
		gt.NoError(t, err).Required()
		gt.Bool(t, claimed).True()
	})

	t.Run("ClaimRefresh rejects an empty user", func(t *testing.T) {
		workspaceID, _ := newKey()
		_, err := repo.SlackMemberChannels().ClaimRefresh(context.Background(), workspaceID, "", time.Now(), time.Minute)
		gt.Error(t, err).Is(model.ErrSlackMemberChannelsValidation)
	})
}

func TestSlackMemberChannelsRepository_Memory(t *testing.T) {
	t.Parallel()
	runSlackMemberChannelsRepositoryTest(t, func(t *testing.T) interfaces.Repository {
		return memory.New()
	})
}

func TestSlackMemberChannelsRepository_Firestore(t *testing.T) {
	t.Parallel()
	runSlackMemberChannelsRepositoryTest(t, newFirestoreRepository)
}
