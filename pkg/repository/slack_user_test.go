package repository_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
)

func runSlackUserRepositoryTest(t *testing.T, newRepo func(t *testing.T) interfaces.Repository) {
	t.Helper()

	t.Run("FindByEmail requires a unique exact match", func(t *testing.T) {
		ctx := context.Background()
		users := newRepo(t).SlackUser()
		gt.NoError(t, users.DeleteAll(ctx)).Required()
		email := "reader@example.com"
		gt.NoError(t, users.SaveMany(ctx, []*model.SlackUser{{ID: "U123", Email: email}, {ID: "U456"}})).Required()
		found, err := users.FindByEmail(ctx, email)
		gt.NoError(t, err).Required()
		gt.Value(t, found).NotNil().Required()
		gt.Value(t, found.ID).Equal(model.SlackUserID("U123"))
		for _, missing := range []string{"", "Reader@example.com", "unknown@example.com"} {
			found, err = users.FindByEmail(ctx, missing)
			gt.NoError(t, err).Required()
			gt.Value(t, found).Nil()
		}
		gt.NoError(t, users.SaveMany(ctx, []*model.SlackUser{{ID: "U789", Email: email}})).Required()
		found, err = users.FindByEmail(ctx, email)
		gt.NoError(t, err).Required()
		gt.Value(t, found).Nil()
	})

	t.Run("SaveMany rejects the whole batch when any user lacks an ID", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()

		gt.NoError(t, repo.SlackUser().DeleteAll(ctx)).Required()

		// One valid user, one with an empty ID: the batch must be rejected
		// before any write, so nothing lands in storage.
		err := repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
			{ID: model.SlackUserID(fmt.Sprintf("U-%d", time.Now().UnixNano())), Name: "valid"},
			{Name: "no-id"},
		})
		gt.Error(t, err).Is(model.ErrSlackUserValidation)

		users, err := repo.SlackUser().GetAll(ctx)
		gt.NoError(t, err).Required()
		gt.Array(t, users).Length(0)
	})

	t.Run("SaveMany and GetAll with empty list", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()

		// Clean up any stale data from previous test runs
		gt.NoError(t, repo.SlackUser().DeleteAll(ctx)).Required()

		// Save empty list
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{})).Required()

		// GetAll should return empty list
		users, err := repo.SlackUser().GetAll(ctx)
		gt.NoError(t, err).Required()

		gt.Array(t, users).Length(0)
	})

	t.Run("SaveMany and GetAll with single user", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		// Clean up any stale data from previous test runs
		gt.NoError(t, repo.SlackUser().DeleteAll(ctx)).Required()

		// Use random ID to avoid conflicts
		userID := model.SlackUserID(fmt.Sprintf("U%d", now.UnixNano()))

		user := &model.SlackUser{
			ID:        userID,
			Name:      "john.doe",
			RealName:  "John Doe",
			Email:     "john.doe@example.com",
			ImageURL:  "https://example.com/avatar.jpg",
			UpdatedAt: now,
		}

		// Save single user
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{user})).Required()

		// GetAll should return 1 user
		users, err := repo.SlackUser().GetAll(ctx)
		gt.NoError(t, err).Required()

		gt.Array(t, users).Length(1).Required()

		// Verify all fields
		got := users[0]
		gt.Value(t, got.ID).Equal(user.ID)
		gt.Value(t, got.Name).Equal(user.Name)
		gt.Value(t, got.RealName).Equal(user.RealName)
		gt.Value(t, got.Email).Equal(user.Email)
		gt.Value(t, got.ImageURL).Equal(user.ImageURL)
		gt.Bool(t, got.UpdatedAt.Sub(user.UpdatedAt).Abs() < time.Second).True()
	})

	t.Run("SaveMany and GetAll with multiple users", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		// Clean up any stale data from previous test runs
		gt.NoError(t, repo.SlackUser().DeleteAll(ctx)).Required()

		// Create multiple users with random IDs
		users := []*model.SlackUser{
			{
				ID:        model.SlackUserID(fmt.Sprintf("U%d_1", now.UnixNano())),
				Name:      "user1",
				RealName:  "User One",
				Email:     "user1@example.com",
				ImageURL:  "https://example.com/avatar1.jpg",
				UpdatedAt: now,
			},
			{
				ID:        model.SlackUserID(fmt.Sprintf("U%d_2", now.UnixNano())),
				Name:      "user2",
				RealName:  "User Two",
				Email:     "user2@example.com",
				ImageURL:  "https://example.com/avatar2.jpg",
				UpdatedAt: now,
			},
			{
				ID:        model.SlackUserID(fmt.Sprintf("U%d_3", now.UnixNano())),
				Name:      "user3",
				RealName:  "User Three",
				Email:     "user3@example.com",
				ImageURL:  "",
				UpdatedAt: now,
			},
		}

		// Save multiple users
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, users)).Required()

		// GetAll should return 3 users
		gotUsers, err := repo.SlackUser().GetAll(ctx)
		gt.NoError(t, err).Required()

		gt.Array(t, gotUsers).Length(3).Required()

		// Verify all users are present (order may vary)
		userMap := make(map[model.SlackUserID]*model.SlackUser)
		for _, u := range gotUsers {
			userMap[u.ID] = u
		}

		for _, expected := range users {
			got, ok := userMap[expected.ID]
			gt.Bool(t, ok).True()
			if !ok {
				continue
			}

			gt.Value(t, got.Name).Equal(expected.Name)
			gt.Value(t, got.RealName).Equal(expected.RealName)
			gt.Value(t, got.Email).Equal(expected.Email)
			gt.Value(t, got.ImageURL).Equal(expected.ImageURL)
		}
	})

	t.Run("SaveMany with >500 users", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		// Clean up any stale data from previous test runs
		gt.NoError(t, repo.SlackUser().DeleteAll(ctx)).Required()

		// Create 600 users to test Firestore batch limit
		users := make([]*model.SlackUser, 600)
		for i := 0; i < 600; i++ {
			users[i] = &model.SlackUser{
				ID:        model.SlackUserID(fmt.Sprintf("U%d_%d", now.UnixNano(), i)),
				Name:      fmt.Sprintf("user%d", i),
				RealName:  fmt.Sprintf("User %d", i),
				Email:     fmt.Sprintf("user%d@example.com", i),
				ImageURL:  "",
				UpdatedAt: now,
			}
		}

		// Save 600 users
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, users)).Required()

		// GetAll should return 600 users
		gotUsers, err := repo.SlackUser().GetAll(ctx)
		gt.NoError(t, err).Required()

		gt.Array(t, gotUsers).Length(600)
	})

	t.Run("GetByID returns user", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		userID := model.SlackUserID(fmt.Sprintf("U%d", now.UnixNano()))
		user := &model.SlackUser{
			ID:        userID,
			Name:      "alice",
			RealName:  "Alice Smith",
			Email:     "alice@example.com",
			ImageURL:  "https://example.com/alice.jpg",
			UpdatedAt: now,
		}

		// Save user
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{user})).Required()

		// GetByID should return the user
		got, err := repo.SlackUser().GetByID(ctx, userID)
		gt.NoError(t, err).Required()

		gt.Value(t, got.ID).Equal(user.ID)
		gt.Value(t, got.Name).Equal(user.Name)
		gt.Value(t, got.RealName).Equal(user.RealName)
	})

	t.Run("GetByID returns NotFound for missing user", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		missingID := model.SlackUserID(fmt.Sprintf("U_MISSING_%d", now.UnixNano()))

		// GetByID should return NotFound error
		_, err := repo.SlackUser().GetByID(ctx, missingID)
		gt.Value(t, err).NotNil().Required()

		// Check if error is ErrNotFound (implementation-specific check)
		// The error should contain "not found" in the message
		gt.String(t, err.Error()).NotEqual("")
	})

	t.Run("GetByIDs batches with <10 users", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		// Create 5 users
		users := make([]*model.SlackUser, 5)
		ids := make([]model.SlackUserID, 5)
		for i := 0; i < 5; i++ {
			id := model.SlackUserID(fmt.Sprintf("U%d_%d", now.UnixNano(), i))
			users[i] = &model.SlackUser{
				ID:        id,
				Name:      fmt.Sprintf("user%d", i),
				RealName:  fmt.Sprintf("User %d", i),
				Email:     fmt.Sprintf("user%d@example.com", i),
				ImageURL:  "",
				UpdatedAt: now,
			}
			ids[i] = id
		}

		// Save users
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, users)).Required()

		// GetByIDs should return all 5 users
		gotMap, err := repo.SlackUser().GetByIDs(ctx, ids)
		gt.NoError(t, err).Required()

		gt.Value(t, len(gotMap)).Equal(5)

		for _, id := range ids {
			_, ok := gotMap[id]
			gt.Bool(t, ok).True()
		}
	})

	t.Run("GetByIDs batches with >10 users", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		// Create 15 users to test Firestore GetAll limit (10)
		users := make([]*model.SlackUser, 15)
		ids := make([]model.SlackUserID, 15)
		for i := 0; i < 15; i++ {
			id := model.SlackUserID(fmt.Sprintf("U%d_%d", now.UnixNano(), i))
			users[i] = &model.SlackUser{
				ID:        id,
				Name:      fmt.Sprintf("user%d", i),
				RealName:  fmt.Sprintf("User %d", i),
				Email:     fmt.Sprintf("user%d@example.com", i),
				ImageURL:  "",
				UpdatedAt: now,
			}
			ids[i] = id
		}

		// Save users
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, users)).Required()

		// GetByIDs should return all 15 users
		gotMap, err := repo.SlackUser().GetByIDs(ctx, ids)
		gt.NoError(t, err).Required()

		gt.Value(t, len(gotMap)).Equal(15)

		for _, id := range ids {
			_, ok := gotMap[id]
			gt.Bool(t, ok).True()
		}
	})

	t.Run("GetByIDs batches with >20 users", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		// Create 25 users to test multiple batches
		users := make([]*model.SlackUser, 25)
		ids := make([]model.SlackUserID, 25)
		for i := 0; i < 25; i++ {
			id := model.SlackUserID(fmt.Sprintf("U%d_%d", now.UnixNano(), i))
			users[i] = &model.SlackUser{
				ID:        id,
				Name:      fmt.Sprintf("user%d", i),
				RealName:  fmt.Sprintf("User %d", i),
				Email:     fmt.Sprintf("user%d@example.com", i),
				ImageURL:  "",
				UpdatedAt: now,
			}
			ids[i] = id
		}

		// Save users
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, users)).Required()

		// GetByIDs should return all 25 users
		gotMap, err := repo.SlackUser().GetByIDs(ctx, ids)
		gt.NoError(t, err).Required()

		gt.Value(t, len(gotMap)).Equal(25)

		for _, id := range ids {
			_, ok := gotMap[id]
			gt.Bool(t, ok).True()
		}
	})

	t.Run("GetByIDs with missing users", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		// Create 3 users
		users := make([]*model.SlackUser, 3)
		existingIDs := make([]model.SlackUserID, 3)
		for i := 0; i < 3; i++ {
			id := model.SlackUserID(fmt.Sprintf("U%d_%d", now.UnixNano(), i))
			users[i] = &model.SlackUser{
				ID:        id,
				Name:      fmt.Sprintf("user%d", i),
				RealName:  fmt.Sprintf("User %d", i),
				Email:     fmt.Sprintf("user%d@example.com", i),
				ImageURL:  "",
				UpdatedAt: now,
			}
			existingIDs[i] = id
		}

		// Save users
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, users)).Required()

		// Request existing + missing users
		missingID := model.SlackUserID(fmt.Sprintf("U_MISSING_%d", now.UnixNano()))
		requestedIDs := append(existingIDs, missingID)

		// GetByIDs should return only existing users (missing users not included)
		gotMap, err := repo.SlackUser().GetByIDs(ctx, requestedIDs)
		gt.NoError(t, err).Required()

		gt.Value(t, len(gotMap)).Equal(3)

		for _, id := range existingIDs {
			_, ok := gotMap[id]
			gt.Bool(t, ok).True()
		}

		_, ok := gotMap[missingID]
		gt.Bool(t, ok).False()
	})

	t.Run("DeleteAll removes all users", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		// Clean up any stale data from previous test runs
		gt.NoError(t, repo.SlackUser().DeleteAll(ctx)).Required()

		// Create and save 3 users
		users := make([]*model.SlackUser, 3)
		for i := 0; i < 3; i++ {
			users[i] = &model.SlackUser{
				ID:        model.SlackUserID(fmt.Sprintf("U%d_%d", now.UnixNano(), i)),
				Name:      fmt.Sprintf("user%d", i),
				RealName:  fmt.Sprintf("User %d", i),
				Email:     fmt.Sprintf("user%d@example.com", i),
				ImageURL:  "",
				UpdatedAt: now,
			}
		}

		gt.NoError(t, repo.SlackUser().SaveMany(ctx, users)).Required()

		// Verify users exist
		gotUsers, err := repo.SlackUser().GetAll(ctx)
		gt.NoError(t, err).Required()
		gt.Array(t, gotUsers).Length(3).Required()

		// DeleteAll
		gt.NoError(t, repo.SlackUser().DeleteAll(ctx)).Required()

		// Verify all users are deleted
		gotUsers, err = repo.SlackUser().GetAll(ctx)
		gt.NoError(t, err).Required()
		gt.Array(t, gotUsers).Length(0)
	})

	t.Run("DeleteAll with >500 users", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		// Clean up any stale data from previous test runs
		gt.NoError(t, repo.SlackUser().DeleteAll(ctx)).Required()

		// Create 600 users to test Firestore batch delete limit
		users := make([]*model.SlackUser, 600)
		for i := 0; i < 600; i++ {
			users[i] = &model.SlackUser{
				ID:        model.SlackUserID(fmt.Sprintf("U%d_%d", now.UnixNano(), i)),
				Name:      fmt.Sprintf("user%d", i),
				RealName:  fmt.Sprintf("User %d", i),
				Email:     fmt.Sprintf("user%d@example.com", i),
				ImageURL:  "",
				UpdatedAt: now,
			}
		}

		gt.NoError(t, repo.SlackUser().SaveMany(ctx, users)).Required()

		// DeleteAll
		gt.NoError(t, repo.SlackUser().DeleteAll(ctx)).Required()

		// Verify all users are deleted
		gotUsers, err := repo.SlackUser().GetAll(ctx)
		gt.NoError(t, err).Required()
		gt.Array(t, gotUsers).Length(0)
	})

	t.Run("SaveMany overwrites existing users", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		userID := model.SlackUserID(fmt.Sprintf("U%d", now.UnixNano()))

		// Save initial user
		user1 := &model.SlackUser{
			ID:        userID,
			Name:      "alice.old",
			RealName:  "Alice Old",
			Email:     "alice.old@example.com",
			ImageURL:  "https://example.com/old.jpg",
			UpdatedAt: now,
		}

		gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{user1})).Required()

		// Overwrite with updated user
		user2 := &model.SlackUser{
			ID:        userID,
			Name:      "alice.new",
			RealName:  "Alice New",
			Email:     "alice.new@example.com",
			ImageURL:  "https://example.com/new.jpg",
			UpdatedAt: now.Add(time.Hour),
		}

		gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{user2})).Required()

		// GetByID should return updated user
		got, err := repo.SlackUser().GetByID(ctx, userID)
		gt.NoError(t, err).Required()

		gt.Value(t, got.Name).Equal("alice.new")
		gt.Value(t, got.RealName).Equal("Alice New")
		gt.Value(t, got.Email).Equal("alice.new@example.com")
		gt.Value(t, got.ImageURL).Equal("https://example.com/new.jpg")
	})

	t.Run("Metadata operations", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()
		now := time.Now()

		// Reset metadata to zero state for a clean test
		gt.NoError(t, repo.SlackUser().SaveMetadata(ctx, &model.SlackUserMetadata{})).Required()

		// GetMetadata should return zero value when no metadata exists
		metadata, err := repo.SlackUser().GetMetadata(ctx)
		gt.NoError(t, err).Required()

		gt.Bool(t, metadata.LastRefreshSuccess.IsZero() || metadata.LastRefreshSuccess.Unix() == 0).True()
		gt.Bool(t, metadata.LastRefreshAttempt.IsZero() || metadata.LastRefreshAttempt.Unix() == 0).True()
		gt.Value(t, metadata.UserCount).Equal(0)

		// Save metadata
		newMetadata := &model.SlackUserMetadata{
			LastRefreshSuccess: now,
			LastRefreshAttempt: now.Add(time.Minute),
			UserCount:          356,
		}

		gt.NoError(t, repo.SlackUser().SaveMetadata(ctx, newMetadata)).Required()

		// Get metadata should return saved values
		got, err := repo.SlackUser().GetMetadata(ctx)
		gt.NoError(t, err).Required()

		gt.Bool(t, got.LastRefreshSuccess.Sub(newMetadata.LastRefreshSuccess).Abs() < time.Second).True()
		gt.Bool(t, got.LastRefreshAttempt.Sub(newMetadata.LastRefreshAttempt).Abs() < time.Second).True()
		gt.Value(t, got.UserCount).Equal(newMetadata.UserCount)

		// Update metadata
		updatedMetadata := &model.SlackUserMetadata{
			LastRefreshSuccess: now.Add(time.Hour),
			LastRefreshAttempt: now.Add(time.Hour + time.Minute),
			UserCount:          400,
		}

		gt.NoError(t, repo.SlackUser().SaveMetadata(ctx, updatedMetadata)).Required()

		// Get metadata should return updated values
		got, err = repo.SlackUser().GetMetadata(ctx)
		gt.NoError(t, err).Required()

		gt.Value(t, got.UserCount).Equal(400)
	})
}

func TestMemorySlackUserRepository(t *testing.T) {
	t.Parallel()
	runSlackUserRepositoryTest(t, func(t *testing.T) interfaces.Repository {
		return memory.New()
	})
}

// TestFirestoreSlackUserRepository is intentionally left serial (no t.Parallel).
// The SlackUser repository exposes DeleteAll and GetAll as collection-wide
// operations with no workspace-level scoping. Running this test concurrently
// with any other Firestore test that touches the user collection would cause
// spurious failures: DeleteAll in one sub-test would wipe data written by a
// concurrent sub-test, and GetAll assertions on exact counts would become
// non-deterministic.
func TestFirestoreSlackUserRepository(t *testing.T) {
	runSlackUserRepositoryTest(t, newFirestoreRepository)
}
