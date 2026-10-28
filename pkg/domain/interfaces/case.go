package interfaces

import (
	"context"

	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

// CaseRepository defines the interface for Case data access
type CaseRepository interface {
	// Create creates a new case with auto-generated ID
	Create(ctx context.Context, workspaceID string, c *model.Case) (*model.Case, error)

	// Get retrieves a case by ID
	Get(ctx context.Context, workspaceID string, id int64) (*model.Case, error)

	// GetByIDs retrieves multiple cases by IDs in a single batch.
	// Returns a map keyed by case ID containing only the cases that
	// were found; missing IDs are silently absent from the result map
	// (callers must distinguish "missing" from "found"). This is the
	// batch fetch hook used by the GraphQL DataLoader to collapse
	// per-row Reporter / Assignees lookups into one repository call
	// per request.
	GetByIDs(ctx context.Context, workspaceID string, ids []int64) (map[int64]*model.Case, error)

	// List retrieves cases with optional filtering.
	// Cases in DRAFT status are excluded by default; use ListDrafts to read
	// drafts. Passing WithStatus(CaseStatusDraft) honours the filter, but
	// callers should generally rely on ListDrafts for the draft-author view.
	List(ctx context.Context, workspaceID string, opts ...ListCaseOption) ([]*model.Case, error)

	// ListDrafts retrieves all cases in DRAFT status across the workspace.
	// Drafts are surfaced workspace-wide so any team member can pick up an
	// in-progress entry; the usecase layer applies private-draft access
	// control (private drafts are visible only to their reporter).
	ListDrafts(ctx context.Context, workspaceID string) ([]*model.Case, error)

	// Update updates an existing case
	Update(ctx context.Context, workspaceID string, c *model.Case) (*model.Case, error)

	// Transact reads the case, hands it to fn for mutation, and writes it back
	// inside a single transaction, returning the written case. Unlike a plain
	// Get followed by Update — which races with a concurrent edit in the window
	// between them — the read fn observes IS the state the write is applied to,
	// so simultaneous "assign me" actions cannot clobber one another and a
	// caller may compute a before/after diff that is guaranteed to describe
	// exactly what was persisted. A missing case fails with ErrNotFound. The
	// full case document is rewritten, so model invariants are re-validated.
	//
	// fn owns every field it changes, including UpdatedAt — the repository
	// never reads the clock. An fn that changes nothing simply leaves the
	// document byte-identical (the write still happens; do not rely on it being
	// skipped). An error returned by fn aborts the transaction without writing
	// and is propagated to the caller with its chain intact, so errors.Is /
	// errors.As on the result still work.
	//
	// fn MUST be idempotent and free of external side effects. The Firestore
	// backend retries the closure on contention, so a Slack post or a counter
	// increment inside fn can run more than once; a captured result variable
	// must be reset at the top of fn so a retry cannot accumulate onto the
	// previous attempt's value. Act on the outcome after Transact returns, not
	// inside it. fn must also not call back into the repository — the in-memory
	// backend holds its write lock for the duration of the call.
	Transact(ctx context.Context, workspaceID string, id int64, fn func(*model.Case) error) (*model.Case, error)

	// Delete deletes a case by ID
	Delete(ctx context.Context, workspaceID string, id int64) error

	// GetBySlackChannelID retrieves a case by its Slack channel ID.
	// Returns nil, nil if no case is found with the given channel ID.
	GetBySlackChannelID(ctx context.Context, workspaceID string, channelID string) (*model.Case, error)

	// GetBySlackChannelIDs returns the workspace's cases whose SlackChannelID
	// is in channelIDs, keyed by channel ID. Channel IDs with no case are
	// absent from the map; an empty input returns an empty map.
	GetBySlackChannelIDs(ctx context.Context, workspaceID string, channelIDs []string) (map[string]*model.Case, error)

	// GetBySlackThread retrieves a thread-mode case by its monitored channel
	// and thread timestamp. Returns nil, nil if no matching case exists. Used
	// for thread-mode case lookup (idempotent creation and reply ingest).
	GetBySlackThread(ctx context.Context, workspaceID string, channelID string, threadTS string) (*model.Case, error)

	// GetByRequestKey retrieves a case by its request key.
	// Returns nil, nil if no case is found with the given key.
	GetByRequestKey(ctx context.Context, workspaceID string, key string) (*model.Case, error)

	// ScanAll streams every Case in the workspace — including drafts and any
	// document whose Status does not match a known value — to fn, in unspecified
	// order. It exists for whole-collection passes (the `validate --check-db`
	// consistency check); List and ListDrafts are status-filtered, so a document
	// carrying an unexpected Status would be silently skipped by both.
	//
	// fn MUST NOT call back into the repository: the in-memory backend holds its
	// read lock for the duration of the scan. Collect what the caller needs and
	// perform any lookups after ScanAll returns. An error returned by fn aborts
	// the scan and is propagated unwrapped, so errors.Is / errors.As on the
	// result still work.
	ScanAll(ctx context.Context, workspaceID string, fn func(*model.Case) error) error
}
