package firestore

import (
	"context"
	"fmt"

	"cloud.google.com/go/firestore"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type caseRepository struct {
	client *firestore.Client
}

func newCaseRepository(client *firestore.Client) *caseRepository {
	return &caseRepository{
		client: client,
	}
}

func (r *caseRepository) casesCollection(workspaceID string) *firestore.CollectionRef {
	return r.client.Collection("workspaces").Doc(workspaceID).Collection("cases")
}

func (r *caseRepository) caseCounterRef(workspaceID string) *firestore.DocumentRef {
	return r.client.Collection("counters").Doc("case").Collection("workspaces").Doc(workspaceID)
}

func (r *caseRepository) getNextID(ctx context.Context, workspaceID string) (int64, error) {
	counterRef := r.caseCounterRef(workspaceID)

	var nextID int64
	err := r.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc, err := tx.Get(counterRef)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				nextID = 1
				return tx.Set(counterRef, map[string]interface{}{
					"value": nextID,
				})
			}
			return goerr.Wrap(err, "failed to get counter")
		}

		currentValue, err := doc.DataAt("value")
		if err != nil {
			return goerr.Wrap(err, "failed to get counter value")
		}

		val, ok := currentValue.(int64)
		if !ok {
			return goerr.New("counter value is not of type int64", goerr.V("value", currentValue))
		}
		nextID = val + 1
		return tx.Update(counterRef, []firestore.Update{
			{Path: "value", Value: nextID},
		})
	})

	if err != nil {
		return 0, goerr.Wrap(err, "failed to get next ID")
	}

	return nextID, nil
}

func (r *caseRepository) Create(ctx context.Context, workspaceID string, c *model.Case) (*model.Case, error) {
	// Validate at the persistence boundary — the only safe place to
	// catch an unattributable write before it lands in storage. The
	// caller (usecase) is responsible for everything else, including
	// CreatedAt / UpdatedAt; the repository assigns the storage-side
	// ID directly onto the caller's struct and persists the model
	// verbatim. NEVER rebuild via a field-by-field struct literal
	// or value-copy — those patterns silently drop any field added
	// to model.Case without an exhaustive search of every repo
	// Create / Update site.
	if err := c.ValidateNew(); err != nil {
		return nil, goerr.Wrap(err, "case validation failed before create")
	}

	nextID, err := r.getNextID(ctx, workspaceID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get next ID")
	}
	c.ID = nextID

	docID := fmt.Sprintf("%d", c.ID)
	if _, err := r.casesCollection(workspaceID).Doc(docID).Set(ctx, c); err != nil {
		return nil, goerr.Wrap(err, "failed to create case", goerr.V("id", c.ID))
	}

	return c, nil
}

func (r *caseRepository) Get(ctx context.Context, workspaceID string, id int64) (*model.Case, error) {
	docID := fmt.Sprintf("%d", id)
	docSnap, err := r.casesCollection(workspaceID).Doc(docID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", id))
		}
		return nil, goerr.Wrap(err, "failed to get case", goerr.V("id", id))
	}

	var c model.Case
	if err := docSnap.DataTo(&c); err != nil {
		return nil, goerr.Wrap(err, "failed to decode case", goerr.V("id", id))
	}

	return &c, nil
}

func (r *caseRepository) GetByIDs(ctx context.Context, workspaceID string, ids []int64) (map[int64]*model.Case, error) {
	result := make(map[int64]*model.Case, len(ids))
	if len(ids) == 0 {
		return result, nil
	}

	col := r.casesCollection(workspaceID)
	refs := make([]*firestore.DocumentRef, len(ids))
	for i, id := range ids {
		refs[i] = col.Doc(fmt.Sprintf("%d", id))
	}

	snaps, err := r.client.GetAll(ctx, refs)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to batch get cases", goerr.V("ids", ids))
	}

	for _, snap := range snaps {
		if !snap.Exists() {
			continue
		}
		var c model.Case
		if err := snap.DataTo(&c); err != nil {
			return nil, goerr.Wrap(err, "failed to decode case", goerr.V("doc_id", snap.Ref.ID))
		}
		result[c.ID] = &c
	}

	return result, nil
}

func (r *caseRepository) List(ctx context.Context, workspaceID string, opts ...interfaces.ListCaseOption) ([]*model.Case, error) {
	cfg := interfaces.BuildListCaseConfig(opts...)

	query := r.casesCollection(workspaceID).Query
	if statusFilter := cfg.Status(); statusFilter != nil {
		query = query.Where("Status", "==", string(*statusFilter))
	} else {
		// Default listings never include drafts. An `in` filter on Status
		// uses a single-field index — no composite index is required.
		// The empty string is included because legacy Firestore documents
		// predate the DRAFT status and stored an empty Status that
		// CaseStatus.Normalize() resolves to OPEN; excluding it here
		// would silently hide those rows from the default Cases view.
		query = query.Where("Status", "in", []string{
			"",
			string(types.CaseStatusOpen),
			string(types.CaseStatusClosed),
		})
	}

	iter := query.Documents(ctx)
	defer iter.Stop()

	var cases []*model.Case
	for {
		docSnap, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, goerr.Wrap(err, "failed to iterate cases")
		}

		var c model.Case
		if err := docSnap.DataTo(&c); err != nil {
			return nil, goerr.Wrap(err, "failed to decode case", goerr.V("doc_id", docSnap.Ref.ID))
		}

		// The archive scope is applied in memory, not as a Where clause: adding
		// an ArchivedAt condition to the Status filter above would require a
		// composite index, which this project forbids. The Action and Memo
		// repositories filter their archive scope the same way.
		if !cfg.ArchiveScope().Allows(c.IsArchived()) {
			continue
		}

		cases = append(cases, &c)
	}

	return cases, nil
}

func (r *caseRepository) ListDrafts(ctx context.Context, workspaceID string) ([]*model.Case, error) {
	// Single-field index on Status only; private-draft access control is
	// applied by the usecase layer, not by extra Where clauses (which would
	// require a composite index).
	iter := r.casesCollection(workspaceID).
		Where("Status", "==", string(types.CaseStatusDraft)).
		Documents(ctx)
	defer iter.Stop()

	drafts := make([]*model.Case, 0)
	for {
		docSnap, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, goerr.Wrap(err, "failed to iterate drafts")
		}

		var c model.Case
		if err := docSnap.DataTo(&c); err != nil {
			return nil, goerr.Wrap(err, "failed to decode draft", goerr.V("doc_id", docSnap.Ref.ID))
		}
		drafts = append(drafts, &c)
	}

	return drafts, nil
}

func (r *caseRepository) Update(ctx context.Context, workspaceID string, c *model.Case) (*model.Case, error) {
	if err := c.Validate(); err != nil {
		return nil, goerr.Wrap(err, "case validation failed before update")
	}

	docID := fmt.Sprintf("%d", c.ID)
	docRef := r.casesCollection(workspaceID).Doc(docID)

	if _, err := docRef.Get(ctx); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", c.ID))
		}
		return nil, goerr.Wrap(err, "failed to check case existence", goerr.V("id", c.ID))
	}

	if _, err := docRef.Set(ctx, c); err != nil {
		return nil, goerr.Wrap(err, "failed to update case", goerr.V("id", c.ID))
	}

	return c, nil
}

// Transact reads the case, hands it to fn, and writes the whole document back
// inside a single transaction, so fn observes exactly the state the write is
// applied to (the lost-update hazard of a read-Update-write through the plain
// Update path). The full *model.Case is rewritten via tx.Set — no field-by-field
// reconstruction, so a newly added model field is never silently dropped.
//
// RunTransaction re-runs this closure on contention, which is why the interface
// requires fn to be idempotent: every attempt starts from a freshly decoded
// case, and only the last attempt's value is returned.
func (r *caseRepository) Transact(ctx context.Context, workspaceID string, id int64, fn func(*model.Case) error) (*model.Case, error) {
	docID := fmt.Sprintf("%d", id)
	docRef := r.casesCollection(workspaceID).Doc(docID)

	var result *model.Case
	err := r.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		// Discard any previous attempt's decode so a retry cannot return a case
		// built from a stale snapshot.
		result = nil

		doc, err := tx.Get(docRef)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", id))
			}
			return goerr.Wrap(err, "failed to get case", goerr.V("id", id))
		}

		var c model.Case
		if err := doc.DataTo(&c); err != nil {
			return goerr.Wrap(err, "failed to decode case", goerr.V("id", id))
		}

		if err := fn(&c); err != nil {
			return err
		}
		if err := c.Validate(); err != nil {
			return goerr.Wrap(err, "case validation failed before transactional write", goerr.V("id", id))
		}
		if err := tx.Set(docRef, &c); err != nil {
			return goerr.Wrap(err, "failed to write case", goerr.V("id", id))
		}
		result = &c
		return nil
	})
	if err != nil {
		return nil, goerr.Wrap(err, "case transaction failed", goerr.V("id", id))
	}

	return result, nil
}

func (r *caseRepository) Delete(ctx context.Context, workspaceID string, id int64) error {
	docID := fmt.Sprintf("%d", id)
	docRef := r.casesCollection(workspaceID).Doc(docID)

	// Check if document exists
	_, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", id))
		}
		return goerr.Wrap(err, "failed to check case existence", goerr.V("id", id))
	}

	_, err = docRef.Delete(ctx)
	if err != nil {
		return goerr.Wrap(err, "failed to delete case", goerr.V("id", id))
	}

	return nil
}

func (r *caseRepository) GetBySlackChannelID(ctx context.Context, workspaceID string, channelID string) (*model.Case, error) {
	iter := r.casesCollection(workspaceID).
		Where("SlackChannelID", "==", channelID).
		Limit(1).
		Documents(ctx)
	defer iter.Stop()

	docSnap, err := iter.Next()
	if err == iterator.Done {
		return nil, nil
	}
	if err != nil {
		return nil, goerr.Wrap(err, "failed to query case by slack channel ID",
			goerr.V("channel_id", channelID))
	}

	var c model.Case
	if err := docSnap.DataTo(&c); err != nil {
		return nil, goerr.Wrap(err, "failed to decode case",
			goerr.V("channel_id", channelID))
	}

	return &c, nil
}

// firestoreInQueryLimit is the maximum number of values Firestore accepts in
// one `in` filter.
const firestoreInQueryLimit = 30

func (r *caseRepository) GetBySlackChannelIDs(ctx context.Context, workspaceID string, channelIDs []string) (map[string]*model.Case, error) {
	result := make(map[string]*model.Case)
	// An `in` filter on one field runs on the existing single-field index, so
	// this needs no composite index.
	for start := 0; start < len(channelIDs); start += firestoreInQueryLimit {
		end := min(start+firestoreInQueryLimit, len(channelIDs))
		chunk := channelIDs[start:end]

		iter := r.casesCollection(workspaceID).
			Where("SlackChannelID", "in", chunk).
			Documents(ctx)
		for {
			docSnap, err := iter.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				iter.Stop()
				return nil, goerr.Wrap(err, "failed to query cases by slack channel IDs",
					goerr.V("workspace_id", workspaceID),
					goerr.V("chunk_size", len(chunk)))
			}
			var c model.Case
			if err := docSnap.DataTo(&c); err != nil {
				iter.Stop()
				return nil, goerr.Wrap(err, "failed to decode case",
					goerr.V("workspace_id", workspaceID),
					goerr.V("doc_id", docSnap.Ref.ID))
			}
			if _, dup := result[c.SlackChannelID]; !dup {
				result[c.SlackChannelID] = &c
			}
		}
		iter.Stop()
	}
	return result, nil
}

func (r *caseRepository) GetBySlackThread(ctx context.Context, workspaceID string, channelID string, threadTS string) (*model.Case, error) {
	if channelID == "" || threadTS == "" {
		return nil, nil
	}

	// Two equality filters. Firestore satisfies a conjunction of equality
	// filters by merging the per-field single-field indexes, so this does NOT
	// require a manually-managed composite index. Filtering on both fields in
	// the query (rather than SlackThreadTS alone + an in-memory channel check)
	// avoids a correctness bug where two channels share the same thread
	// timestamp and Limit(1) returns the wrong channel's case.
	iter := r.casesCollection(workspaceID).
		Where("SlackChannelID", "==", channelID).
		Where("SlackThreadTS", "==", threadTS).
		Limit(1).
		Documents(ctx)
	defer iter.Stop()

	docSnap, err := iter.Next()
	if err == iterator.Done {
		return nil, nil
	}
	if err != nil {
		return nil, goerr.Wrap(err, "failed to query case by slack thread",
			goerr.V("channel_id", channelID), goerr.V("thread_ts", threadTS))
	}

	var c model.Case
	if err := docSnap.DataTo(&c); err != nil {
		return nil, goerr.Wrap(err, "failed to decode case",
			goerr.V("channel_id", channelID), goerr.V("thread_ts", threadTS))
	}

	return &c, nil
}

func (r *caseRepository) GetByRequestKey(ctx context.Context, workspaceID string, key string) (*model.Case, error) {
	iter := r.casesCollection(workspaceID).
		Where("RequestKey", "==", key).
		Limit(1).
		Documents(ctx)
	defer iter.Stop()

	docSnap, err := iter.Next()
	if err == iterator.Done {
		return nil, nil
	}
	if err != nil {
		return nil, goerr.Wrap(err, "failed to query case by request key",
			goerr.V("key", key))
	}

	var c model.Case
	if err := docSnap.DataTo(&c); err != nil {
		return nil, goerr.Wrap(err, "failed to decode case",
			goerr.V("key", key))
	}

	return &c, nil
}

// ScanAll walks the workspace's whole cases collection. The query carries no
// Where clause, so it needs no index at all — List / ListDrafts filter on Status
// and between them would skip any document holding an unexpected Status value,
// which is exactly the kind of drift the consistency check exists to surface.
func (r *caseRepository) ScanAll(ctx context.Context, workspaceID string, fn func(*model.Case) error) error {
	iter := r.casesCollection(workspaceID).Documents(ctx)
	defer iter.Stop()

	for {
		docSnap, err := iter.Next()
		if err == iterator.Done {
			return nil
		}
		if err != nil {
			return goerr.Wrap(err, "failed to iterate cases for scan",
				goerr.V("workspace_id", workspaceID))
		}

		var c model.Case
		if err := docSnap.DataTo(&c); err != nil {
			return goerr.Wrap(err, "failed to decode case",
				goerr.V("doc_id", docSnap.Ref.ID))
		}

		// Propagated unwrapped so the caller's errors.Is / errors.As still work.
		if err := fn(&c); err != nil {
			return err
		}
	}
}
