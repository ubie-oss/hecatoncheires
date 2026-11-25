package firestore

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	slackUsersCollection    = "slack_users"
	slackMetadataCollection = "slack_metadata"
	refreshStatusDocument   = "refresh_status"

	// Firestore batch operation limits
	// Reference: https://cloud.google.com/firestore/docs/query-data/get-data#go
	firestoreGetAllLimit = 30 // Maximum document references per GetAll
)

type slackUserRepository struct {
	client *firestore.Client
}

var _ interfaces.SlackUserRepository = &slackUserRepository{}

func newSlackUserRepository(client *firestore.Client) *slackUserRepository {
	return &slackUserRepository{
		client: client,
	}
}

func (r *slackUserRepository) collection() *firestore.CollectionRef {
	return r.client.Collection(slackUsersCollection)
}

func (r *slackUserRepository) metadataCollection() *firestore.CollectionRef {
	return r.client.Collection(slackMetadataCollection)
}

// GetAll retrieves all Slack users from Firestore
func (r *slackUserRepository) GetAll(ctx context.Context) ([]*model.SlackUser, error) {
	iter := r.collection().Documents(ctx)
	defer iter.Stop()

	var users []*model.SlackUser
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, goerr.Wrap(err, "failed to iterate Slack users")
		}

		var user model.SlackUser
		if err := doc.DataTo(&user); err != nil {
			return nil, goerr.Wrap(err, "failed to unmarshal Slack user", goerr.V("docID", doc.Ref.ID))
		}

		users = append(users, &user)
	}

	return users, nil
}

// GetByID retrieves a single Slack user by ID
func (r *slackUserRepository) GetByID(ctx context.Context, id model.SlackUserID) (*model.SlackUser, error) {
	doc, err := r.collection().Doc(string(id)).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, goerr.Wrap(ErrNotFound, "slack user not found", goerr.V("id", id))
		}
		return nil, goerr.Wrap(err, "failed to get Slack user", goerr.V("id", id))
	}

	var user model.SlackUser
	if err := doc.DataTo(&user); err != nil {
		return nil, goerr.Wrap(err, "failed to unmarshal Slack user", goerr.V("id", id))
	}

	return &user, nil
}

func (r *slackUserRepository) FindByEmail(ctx context.Context, email string) (*model.SlackUser, error) {
	if email == "" {
		return nil, nil
	}
	iter := r.collection().Where("Email", "==", email).Limit(2).Documents(ctx)
	defer iter.Stop()
	docs, err := iter.GetAll()
	if err != nil {
		return nil, goerr.Wrap(err, "failed to look up Slack user by email")
	}
	if len(docs) != 1 {
		return nil, nil
	}
	var user model.SlackUser
	if err := docs[0].DataTo(&user); err != nil {
		return nil, goerr.Wrap(err, "failed to unmarshal Slack user")
	}
	return &user, nil
}

// GetByIDs retrieves multiple Slack users by IDs (for DataLoader batching)
// Handles Firestore GetAll limit of 10 documents per batch by splitting into multiple requests
func (r *slackUserRepository) GetByIDs(ctx context.Context, ids []model.SlackUserID) (map[model.SlackUserID]*model.SlackUser, error) {
	if len(ids) == 0 {
		return make(map[model.SlackUserID]*model.SlackUser), nil
	}

	result := make(map[model.SlackUserID]*model.SlackUser, len(ids))

	// Split into batches of firestoreGetAllLimit (10 documents)
	for i := 0; i < len(ids); i += firestoreGetAllLimit {
		end := i + firestoreGetAllLimit
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]

		// Create document references
		refs := make([]*firestore.DocumentRef, len(batch))
		for j, id := range batch {
			refs[j] = r.collection().Doc(string(id))
		}

		// Batch get
		docs, err := r.client.GetAll(ctx, refs)
		if err != nil {
			return nil, goerr.Wrap(err, "failed to batch get Slack users", goerr.V("count", len(batch)))
		}

		for idx, doc := range docs {
			if !doc.Exists() {
				// Missing users are not included in the result map (not an error)
				continue
			}

			var user model.SlackUser
			if err := doc.DataTo(&user); err != nil {
				return nil, goerr.Wrap(err, "failed to unmarshal Slack user", goerr.V("id", batch[idx]))
			}

			result[batch[idx]] = &user
		}
	}

	return result, nil
}

// SaveMany saves multiple Slack users (upsert operation)
// Handles Firestore batch write limit of 500 documents by splitting into multiple batches
func (r *slackUserRepository) SaveMany(ctx context.Context, users []*model.SlackUser) error {
	if len(users) == 0 {
		return nil
	}

	// Validate all entries before starting the BulkWriter, so a single invalid
	// record does not leave a partial write behind.
	for _, user := range users {
		if err := user.Validate(); err != nil {
			return goerr.Wrap(err, "slack user validation failed before save")
		}
	}

	// Use BulkWriter which automatically handles batching
	bulkWriter := r.client.BulkWriter(ctx)
	defer bulkWriter.End()

	for _, user := range users {
		docRef := r.collection().Doc(string(user.ID))
		if _, err := bulkWriter.Set(docRef, user); err != nil {
			return goerr.Wrap(err, "failed to add Set operation to bulk writer", goerr.V("user_id", user.ID))
		}
	}

	// Flush and wait for all operations to complete
	bulkWriter.Flush()

	return nil
}

// DeleteAll deletes all Slack users from Firestore
// Handles Firestore batch delete limit of 500 documents by splitting into multiple batches
func (r *slackUserRepository) DeleteAll(ctx context.Context) error {
	// Retrieve all document references
	iter := r.collection().Documents(ctx)
	defer iter.Stop()

	var refs []*firestore.DocumentRef
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return goerr.Wrap(err, "failed to iterate Slack users for deletion")
		}
		refs = append(refs, doc.Ref)
	}

	if len(refs) == 0 {
		return nil
	}

	// Use BulkWriter which automatically handles batching
	bulkWriter := r.client.BulkWriter(ctx)
	defer bulkWriter.End()

	for _, ref := range refs {
		if _, err := bulkWriter.Delete(ref); err != nil {
			return goerr.Wrap(err, "failed to add Delete operation to bulk writer")
		}
	}

	// Flush and wait for all operations to complete
	bulkWriter.Flush()

	return nil
}

// GetMetadata retrieves refresh metadata
func (r *slackUserRepository) GetMetadata(ctx context.Context) (*model.SlackUserMetadata, error) {
	doc, err := r.metadataCollection().Doc(refreshStatusDocument).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// Return zero value if metadata doesn't exist yet
			return &model.SlackUserMetadata{
				LastRefreshSuccess: time.Time{},
				LastRefreshAttempt: time.Time{},
				UserCount:          0,
			}, nil
		}
		return nil, goerr.Wrap(err, "failed to get Slack user metadata")
	}

	var metadata model.SlackUserMetadata
	if err := doc.DataTo(&metadata); err != nil {
		return nil, goerr.Wrap(err, "failed to unmarshal Slack user metadata")
	}

	return &metadata, nil
}

// SaveMetadata saves refresh metadata
func (r *slackUserRepository) SaveMetadata(ctx context.Context, metadata *model.SlackUserMetadata) error {
	_, err := r.metadataCollection().Doc(refreshStatusDocument).Set(ctx, metadata)
	if err != nil {
		return goerr.Wrap(err, "failed to save Slack user metadata")
	}
	return nil
}
