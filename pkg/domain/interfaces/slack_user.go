package interfaces

import (
	"context"
	"time"

	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

// SlackUserRepository provides database operations for Slack users.
//
// N+1 Prevention Policy:
// - NO individual Save(user) method - always use SaveMany for batch writes
// - GetByID is minimal - prefer GetByIDs for batch retrieval via DataLoader
// - Full refreshes atomically publish a new directory through ReplaceAll
// - All operations avoid loops with individual DB calls
type SlackUserRepository interface {
	// GetAll retrieves all Slack users from the database
	GetAll(ctx context.Context) ([]*model.SlackUser, error)

	// GetByID retrieves a single Slack user by ID
	GetByID(ctx context.Context, id model.SlackUserID) (*model.SlackUser, error)

	// FindByEmail returns a user only when exactly one record has the same
	// non-empty email address after normalization.
	FindByEmail(ctx context.Context, email string) (*model.SlackUser, error)

	// GetByIDs retrieves multiple Slack users by IDs (for DataLoader batching)
	// Returns a map of ID -> SlackUser. Missing users are not included in the map.
	GetByIDs(ctx context.Context, ids []model.SlackUserID) (map[model.SlackUserID]*model.SlackUser, error)

	// SaveMany saves multiple Slack users (upsert operation)
	// Handles Firestore batch write limits (500 per batch) internally
	SaveMany(ctx context.Context, users []*model.SlackUser) error

	// ReplaceAll publishes a complete directory and its successful refresh time.
	// Failed writes preserve the previous directory, and an earlier refresh may
	// not supersede one that started later.
	ReplaceAll(ctx context.Context, users []*model.SlackUser, startedAt time.Time) error

	// RecordRefreshAttempt advances the last attempt time without changing the directory.
	RecordRefreshAttempt(ctx context.Context, startedAt time.Time) error

	// DeleteAll deletes all Slack users from the database
	// Handles Firestore batch delete limits (500 per batch) internally
	DeleteAll(ctx context.Context) error

	// GetMetadata retrieves refresh metadata
	GetMetadata(ctx context.Context) (*model.SlackUserMetadata, error)

	// SaveMetadata saves refresh metadata
	SaveMetadata(ctx context.Context, metadata *model.SlackUserMetadata) error
}
