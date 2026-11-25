package firestore

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	slackUsersCollection       = "slack_users"
	slackMetadataCollection    = "slack_metadata"
	refreshStatusDocument      = "refresh_status"
	activeDirectoryDocument    = "active_directory"
	slackDirectoryStageTimeout = 5 * time.Minute
	slackAbandonedSnapshotAge  = time.Hour

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

// slackDirectorySnapshot is stored in a separate document from refresh status.
// Older server revisions can still update status without changing this pointer.
type slackDirectorySnapshot struct {
	ID        string
	StartedAt time.Time
	Staged    bool
}

func (r *slackUserRepository) directoryPointer() *firestore.DocumentRef {
	return r.metadataCollection().Doc(activeDirectoryDocument)
}

func (r *slackUserRepository) snapshots() *firestore.CollectionRef {
	return r.directoryPointer().Collection("snapshots")
}

func (r *slackUserRepository) directory(ctx context.Context) (*firestore.CollectionRef, time.Time, error) {
	doc, err := r.directoryPointer().Get(ctx)
	if status.Code(err) == codes.NotFound {
		return r.collection(), time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, goerr.Wrap(err, "read active Slack user directory")
	}
	var pointer slackDirectorySnapshot
	if err := doc.DataTo(&pointer); err != nil {
		return nil, time.Time{}, goerr.Wrap(err, "decode active Slack user directory")
	}
	if pointer.ID == "" {
		return nil, time.Time{}, goerr.New("active Slack user directory has no snapshot ID")
	}
	return r.snapshots().Doc(pointer.ID).Collection("users"), doc.ReadTime, nil
}

// GetAll retrieves all Slack users from the current directory.
func (r *slackUserRepository) GetAll(ctx context.Context) ([]*model.SlackUser, error) {
	col, readTime, err := r.directory(ctx)
	if err != nil {
		return nil, goerr.Wrap(err, "resolve Slack user directory")
	}
	query := col.Query
	if !readTime.IsZero() {
		// Read the selected generation as of the pointer read. A refresh can
		// publish a new generation and retire this one while the query runs.
		query = *query.WithReadOptions(firestore.ReadTime(readTime))
	}
	return readSlackUsers(ctx, query)
}

func readSlackUsers(ctx context.Context, query firestore.Query) ([]*model.SlackUser, error) {
	iter := query.Documents(ctx)
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

func (r *slackUserRepository) GetByID(ctx context.Context, id model.SlackUserID) (*model.SlackUser, error) {
	col, readTime, err := r.directory(ctx)
	if err != nil {
		return nil, goerr.Wrap(err, "resolve Slack user directory")
	}
	ref := col.Doc(string(id))
	if !readTime.IsZero() {
		ref = ref.WithReadOptions(firestore.ReadTime(readTime))
	}
	doc, err := ref.Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, goerr.Wrap(ErrNotFound, "slack user not found", goerr.V("id", id))
	}
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get Slack user", goerr.V("id", id))
	}
	var user model.SlackUser
	if err := doc.DataTo(&user); err != nil {
		return nil, goerr.Wrap(err, "failed to unmarshal Slack user", goerr.V("id", id))
	}
	return &user, nil
}

func (r *slackUserRepository) FindByEmail(ctx context.Context, email string) (*model.SlackUser, error) {
	email = model.NormalizeSlackEmail(email)
	if email == "" {
		return nil, nil
	}
	col, readTime, err := r.directory(ctx)
	if err != nil {
		return nil, goerr.Wrap(err, "resolve Slack user directory")
	}
	query := col.Query
	if !readTime.IsZero() {
		query = col.Where("NormalizedEmail", "==", email).Limit(2)
		query = *query.WithReadOptions(firestore.ReadTime(readTime))
	}
	// Legacy documents may lack an index value. Scan until the first refresh
	// publishes a fully indexed generation, including duplicates differing by case.
	users, err := readSlackUsers(ctx, query)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to look up Slack user by email")
	}
	if !readTime.IsZero() && len(users) != 1 {
		return nil, nil
	}
	var match *model.SlackUser
	for _, user := range users {
		if model.NormalizeSlackEmail(user.Email) == email {
			if match != nil {
				return nil, nil
			}
			match = user
		}
	}
	return match, nil
}

func (r *slackUserRepository) GetByIDs(ctx context.Context, ids []model.SlackUserID) (map[model.SlackUserID]*model.SlackUser, error) {
	if len(ids) == 0 {
		return map[model.SlackUserID]*model.SlackUser{}, nil
	}
	col, readTime, err := r.directory(ctx)
	if err != nil {
		return nil, goerr.Wrap(err, "resolve Slack user directory")
	}
	if readTime.IsZero() {
		return readSlackUsersByID(col, ids, func(refs []*firestore.DocumentRef) ([]*firestore.DocumentSnapshot, error) {
			return r.client.GetAll(ctx, refs)
		})
	}
	var result map[model.SlackUserID]*model.SlackUser
	if err := r.client.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		users, err := readSlackUsersByID(col, ids, tx.GetAll)
		result = users
		return err
	}, firestore.ReadOnly, firestore.TransactionReadTime(readTime)); err != nil {
		return nil, goerr.Wrap(err, "read Slack users from selected directory")
	}
	return result, nil
}

func readSlackUsersByID(col *firestore.CollectionRef, ids []model.SlackUserID, get func([]*firestore.DocumentRef) ([]*firestore.DocumentSnapshot, error)) (map[model.SlackUserID]*model.SlackUser, error) {
	result := make(map[model.SlackUserID]*model.SlackUser, len(ids))
	for i := 0; i < len(ids); i += firestoreGetAllLimit {
		batch := ids[i:min(i+firestoreGetAllLimit, len(ids))]
		refs := make([]*firestore.DocumentRef, len(batch))
		for j, id := range batch {
			refs[j] = col.Doc(string(id))
		}
		docs, err := get(refs)
		if err != nil {
			return nil, goerr.Wrap(err, "failed to batch get Slack users", goerr.V("count", len(batch)))
		}
		for j, doc := range docs {
			if !doc.Exists() {
				continue
			}
			var user model.SlackUser
			if err := doc.DataTo(&user); err != nil {
				return nil, goerr.Wrap(err, "failed to unmarshal Slack user", goerr.V("id", batch[j]))
			}
			result[batch[j]] = &user
		}
	}
	return result, nil
}

func (r *slackUserRepository) SaveMany(ctx context.Context, users []*model.SlackUser) error {
	if len(users) == 0 {
		return nil
	}
	col, _, err := r.directory(ctx)
	if err != nil {
		return goerr.Wrap(err, "resolve Slack user directory")
	}
	return r.saveUsers(ctx, col, users)
}

func (r *slackUserRepository) saveUsers(ctx context.Context, col *firestore.CollectionRef, users []*model.SlackUser) error {
	for _, user := range users {
		if err := user.Validate(); err != nil {
			return goerr.Wrap(err, "slack user validation failed before save")
		}
	}
	writer := r.client.BulkWriter(ctx)
	defer writer.End()
	jobs := make([]*firestore.BulkWriterJob, 0, len(users))
	for _, user := range users {
		copy := *user
		copy.NormalizedEmail = model.NormalizeSlackEmail(copy.Email)
		job, err := writer.Set(col.Doc(string(user.ID)), &copy)
		if err != nil {
			return goerr.Wrap(err, "enqueue Slack user write", goerr.V("user_id", user.ID))
		}
		jobs = append(jobs, job)
	}
	writer.Flush()
	for _, job := range jobs {
		if _, err := job.Results(); err != nil {
			return goerr.Wrap(err, "write Slack user")
		}
	}
	return nil
}

func (r *slackUserRepository) DeleteAll(ctx context.Context) error {
	col, _, err := r.directory(ctx)
	if err != nil {
		return goerr.Wrap(err, "resolve Slack user directory")
	}
	return r.clearDirectory(ctx, col)
}

func (r *slackUserRepository) clearDirectory(ctx context.Context, col *firestore.CollectionRef) error {
	iter := col.Documents(ctx)
	defer iter.Stop()
	writer := r.client.BulkWriter(ctx)
	defer writer.End()
	var jobs []*firestore.BulkWriterJob
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return goerr.Wrap(err, "iterate Slack directory for removal")
		}
		job, err := writer.Delete(doc.Ref)
		if err != nil {
			return goerr.Wrap(err, "enqueue Slack user removal")
		}
		jobs = append(jobs, job)
	}
	writer.Flush()
	for _, job := range jobs {
		if _, err := job.Results(); err != nil {
			return goerr.Wrap(err, "remove Slack user")
		}
	}
	return nil
}

func (r *slackUserRepository) ReplaceAll(ctx context.Context, users []*model.SlackUser, startedAt time.Time) error {
	startedAt = startedAt.Truncate(time.Microsecond)
	snapshot := r.snapshots().NewDoc()
	pointer := slackDirectorySnapshot{ID: snapshot.ID, StartedAt: startedAt}
	if _, err := snapshot.Create(ctx, pointer); err != nil {
		return goerr.Wrap(err, "create Slack user directory snapshot")
	}
	stageCtx, cancel := context.WithTimeout(ctx, slackDirectoryStageTimeout)
	stageErr := r.saveUsers(stageCtx, snapshot.Collection("users"), users)
	cancel()
	if stageErr != nil {
		// Keep the marker for a later cleanup. After a transport failure, a
		// dispatched write can still reach Firestore after the caller returns.
		return goerr.Wrap(stageErr, "stage Slack user directory")
	}
	pointer.Staged = true
	if _, err := snapshot.Set(ctx, pointer); err != nil {
		return goerr.Wrap(err, "record staged Slack user directory")
	}

	var published bool
	err := r.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		published = false
		active, err := tx.Get(r.directoryPointer())
		if err == nil {
			var current slackDirectorySnapshot
			if err := active.DataTo(&current); err != nil {
				return goerr.Wrap(err, "decode active Slack user directory")
			}
			if !startedAt.After(current.StartedAt) {
				return nil
			}
		} else if status.Code(err) != codes.NotFound {
			return goerr.Wrap(err, "read active Slack user directory")
		}
		statusRef := r.metadataCollection().Doc(refreshStatusDocument)
		metadata, err := readRefreshStatus(tx, statusRef)
		if err != nil {
			return goerr.Wrap(err, "read Slack refresh status")
		}
		metadata.LastRefreshSuccess = startedAt
		if startedAt.After(metadata.LastRefreshAttempt) {
			metadata.LastRefreshAttempt = startedAt
		}
		metadata.UserCount = len(users)
		if err := tx.Set(r.directoryPointer(), pointer); err != nil {
			return goerr.Wrap(err, "set active Slack user directory")
		}
		if err := tx.Set(statusRef, metadata); err != nil {
			return goerr.Wrap(err, "set successful Slack refresh status")
		}
		published = true
		return nil
	})
	if err != nil {
		// A commit with an unknown outcome might have published the snapshot.
		// Future successful refreshes handle retirement.
		return goerr.Wrap(err, "publish Slack user directory")
	}
	if !published {
		r.retireSnapshot(ctx, snapshot)
		return nil
	}

	// A snapshot older than the one we published can never become active.
	iter := r.snapshots().Where("StartedAt", "<", startedAt).Documents(ctx)
	defer iter.Stop()
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			errutil.Handle(ctx, goerr.Wrap(err, "list retired Slack directories"), "Slack directory cleanup failed")
			break
		}
		var candidate slackDirectorySnapshot
		if err := doc.DataTo(&candidate); err != nil {
			errutil.Handle(ctx, goerr.Wrap(err, "decode retired Slack directory"), "Slack directory cleanup failed")
			continue
		}
		// Never remove the marker of a writer that could still be staging.
		// A crashed writer's marker is eligible only well past the stage deadline.
		if doc.Ref.ID != snapshot.ID && (candidate.Staged || time.Since(doc.CreateTime) > slackAbandonedSnapshotAge) {
			r.retireSnapshot(ctx, doc.Ref)
		}
	}
	return nil
}

func (r *slackUserRepository) retireSnapshot(ctx context.Context, ref *firestore.DocumentRef) {
	if err := r.clearDirectory(ctx, ref.Collection("users")); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "clear retired Slack directory"), "Slack directory cleanup failed")
		return
	}
	if _, err := ref.Delete(ctx); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "remove retired Slack directory marker"), "Slack directory cleanup failed")
	}
}

func (r *slackUserRepository) RecordRefreshAttempt(ctx context.Context, startedAt time.Time) error {
	ref := r.metadataCollection().Doc(refreshStatusDocument)
	if err := r.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		metadata, err := readRefreshStatus(tx, ref)
		if err != nil {
			return goerr.Wrap(err, "read Slack refresh status")
		}
		if !startedAt.After(metadata.LastRefreshAttempt) {
			return nil
		}
		metadata.LastRefreshAttempt = startedAt
		return tx.Set(ref, metadata)
	}); err != nil {
		return goerr.Wrap(err, "record Slack refresh attempt")
	}
	return nil
}

func readRefreshStatus(tx *firestore.Transaction, ref *firestore.DocumentRef) (model.SlackUserMetadata, error) {
	var metadata model.SlackUserMetadata
	doc, err := tx.Get(ref)
	if status.Code(err) == codes.NotFound {
		return metadata, nil
	}
	if err != nil {
		return metadata, goerr.Wrap(err, "get Slack refresh status")
	}
	if err := doc.DataTo(&metadata); err != nil {
		return metadata, goerr.Wrap(err, "decode Slack refresh status")
	}
	return metadata, nil
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
